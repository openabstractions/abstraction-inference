//go:build windows

package modelfiles

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Section access rights (winnt.h) and the commit flag x/sys does not name.
const (
	sectionQuery   = 0x0001
	sectionMapRead = 0x0004
	secCommit      = 0x08000000
	// viewAlign is Windows' allocation granularity; the section's size is
	// rounded up to it.
	viewAlign = 64 << 10
	// sectorAlign is the alignment an unbuffered read needs of its file
	// offset, length and buffer.
	sectorAlign = 4096
	// maxReaders bounds the handles reading one stored source at once.
	maxReaders = 8
)

// sectionSDDL is the section's protected DACL: OWNER RIGHTS (OW) may query
// and map for reading (0x5), nothing more. The OWNER RIGHTS entry takes away
// the owner's implicit READ_CONTROL and WRITE_DAC, so no process of the
// account can duplicate a read-only handle to write access or rewrite the
// DACL. The creating handle keeps the access asked for at creation until Fill
// seals the section (INF-MS3). Tests set it empty to measure the default
// descriptor.
var sectionSDDL = "D:P(A;;0x5;;;OW)"

// testHookIdle, when set by a test, runs after the section's last loan has
// ended or its owner has closed it, before the handle is closed: the moment
// a Lend racing that end must already be refused.
var testHookIdle func()

// Section is one object's bytes in an unnamed, pagefile-backed section the
// provider host owns. Its bytes were proved against the object's digest, no
// writable view of it exists, and its handle maps for reading only.
type Section struct {
	src     Extent
	digest  string
	chunks  []string
	release func()
	once    sync.Once
	// recordErr is set once, before the section is returned, when a proven
	// first fill could not write its span record.
	recordErr error

	mu     sync.Mutex
	h      windows.Handle
	active int
	closed bool
}

// Fill places src's Length bytes in a new section, proves them against
// digest ("sha256:" and 64 lower-case hex digits) and seals it (INF-MS1,
// INF-MS2). It writes no file. Bytes whose digest differs, a source that
// cannot give them, and a cancelled ctx free the section and return an error.
// release, when not nil, is called exactly once: when the section is freed, or
// before Fill returns an error.
func Fill(ctx context.Context, src Extent, digest string, release func()) (_ *Section, err error) {
	s := &Section{src: src, digest: digest, release: release}
	defer func() {
		if err != nil {
			s.free()
		}
	}()
	want, err := checkDigest(digest)
	if err != nil {
		return nil, err
	}
	if err := src.check(); err != nil {
		return nil, err
	}
	if err := checkRange(src); err != nil {
		return nil, err
	}
	addr, view, err := s.create(src.Length)
	if err != nil {
		return nil, err
	}
	var whole string
	if src.Method == zip.Store {
		s.chunks, err = fillStored(ctx, src, view)
		if err == nil {
			sum := sha256.Sum256(view[:src.Length])
			whole = hex.EncodeToString(sum[:])
		}
	} else {
		s.chunks, whole, err = fillDeflate(ctx, src, view)
	}
	err = errors.Join(err, windows.UnmapViewOfFile(addr))
	if err != nil {
		return nil, err
	}
	if whole != want {
		return nil, fmt.Errorf("%w: %s holds sha256:%s, the object is %s", ErrDigest, src.Path, whole, digest)
	}
	return s, s.seal()
}

// checkRange accepts a source whose stored bytes lie inside a regular file.
func checkRange(src Extent) error {
	st, err := os.Stat(src.Path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSource, err)
	}
	if !st.Mode().IsRegular() || src.Packed > st.Size()-src.Offset {
		return fmt.Errorf("%w: %d bytes at offset %d lie beyond %s", ErrSource, src.Packed, src.Offset, src.Path)
	}
	return nil
}

// create makes the section of length bytes under its protected descriptor
// and maps one writable view of it; the caller unmaps the view before seal.
func (s *Section) create(length int64) (uintptr, []byte, error) {
	mapped := roundUp(length, viewAlign)
	var sa *windows.SecurityAttributes
	if sectionSDDL != "" {
		sd, err := windows.SecurityDescriptorFromString(sectionSDDL)
		if err != nil {
			return 0, nil, err
		}
		sa = &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	}
	// The whole size is charged to commit here, once.
	h, err := windows.CreateFileMapping(windows.InvalidHandle, sa, windows.PAGE_READWRITE|secCommit, uint32(mapped>>32), uint32(mapped), nil)
	if err != nil {
		return 0, nil, fmt.Errorf("modelfiles: CreateFileMapping of %d bytes: %w", mapped, err)
	}
	s.h = h
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE, 0, 0, uintptr(mapped))
	if err != nil {
		return 0, nil, fmt.Errorf("modelfiles: MapViewOfFile: %w", err)
	}
	return addr, viewBytes(addr, mapped), nil
}

// seal turns the creating handle, the one handle that could map the section
// for writing, into a map-read and query handle. From here no writable view
// exists and no handle can make one (INF-MS3).
func (s *Section) seal() error {
	var sealed windows.Handle
	cur := windows.CurrentProcess()
	err := windows.DuplicateHandle(cur, s.h, cur, &sealed, sectionMapRead|sectionQuery, false, windows.DUPLICATE_CLOSE_SOURCE)
	s.mu.Lock()
	s.h = sealed // the source is closed whether or not the duplicate exists
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("modelfiles: seal: %w", err)
	}
	return nil
}

// FillDecoded places an object's bytes in a new section through decode,
// which reads them from src and decodes them into the section's writable
// view, proves them and seals the section (INF-MS8). src names the stored
// bytes decode reads: a STORED region of Packed bytes at Offset of Path,
// decoding to Length bytes. It writes no file.
//
// The fill owns the proof. After decode it hashes every span itself: when r
// holds a record of digest, the spans must equal the record's, and decode
// was given them to refuse early. Otherwise decode gets no spans, and the
// fill hashes the whole object against digest as well and records the
// spans. A missing r, a record of another length, bytes of another digest,
// an error from decode and a cancelled ctx free the section. A proven first
// fill whose record cannot be written still returns its section with no
// error; the section's RecordErr reports the failure, one log line names
// it, and the next fill hashes the whole object again. release, when not
// nil, is called exactly once: when the section is freed, or before
// FillDecoded returns an error.
func (r *SpanRecords) FillDecoded(ctx context.Context, src Extent, digest string, decode Decoder, release func()) (_ *Section, err error) {
	s := &Section{src: src, digest: digest, release: release}
	defer func() {
		if err != nil {
			s.free()
		}
	}()
	if r == nil {
		return nil, errors.New("modelfiles: a decoded fill needs OA's span records")
	}
	want, err := checkDigest(digest)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(src.Path) || src.Method != zip.Store || src.Offset < 0 || src.Packed <= 0 || src.Length <= 0 {
		return nil, fmt.Errorf("%w: a decoded source is a STORED region of an absolute path", ErrSource)
	}
	if err := checkRange(src); err != nil {
		return nil, err
	}
	rec, recorded := r.Lookup(digest)
	if recorded && rec.Length != src.Length {
		return nil, fmt.Errorf("%w: the record of %s holds %d bytes, the source %d", ErrDigest, digest, rec.Length, src.Length)
	}
	addr, view, err := s.create(src.Length)
	if err != nil {
		return nil, err
	}
	object := view[:src.Length]
	var given []string
	if recorded {
		given = rec.Spans
	}
	finish, err := decode(ctx, object, given)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		s.chunks = spanSums(object, finish)
		if recorded {
			for i, span := range s.chunks {
				if span != rec.Spans[i] {
					err = fmt.Errorf("%w: span %d of %s differs from OA's record", ErrDigest, i, src.Path)
					break
				}
			}
		} else if whole := sum(object); whole != want {
			err = fmt.Errorf("%w: %s decodes to sha256:%s, the object is %s", ErrDigest, src.Path, whole, digest)
		}
	}
	err = errors.Join(err, windows.UnmapViewOfFile(addr))
	if err != nil {
		return nil, err
	}
	if err := s.seal(); err != nil {
		return nil, err
	}
	if !recorded {
		if err := r.Put(SpanRecord{Digest: digest, Length: src.Length, Spans: s.chunks}); err != nil {
			s.recordErr = fmt.Errorf("%w: %s: %v", ErrRecord, digest, err)
			log.Printf("%v; the section is proven, and the next fill of this object hashes it whole again", s.recordErr)
		}
	}
	return s, nil
}

// hashBlock is the block a decoder's finish completes just before the fill
// hashes it, small enough to stay in a core's cache between the two.
const hashBlock = 256 << 10

// spanSums hashes each ChunkSize span of b on several goroutines, handing
// each block to finish's function for its span first (Decoder).
func spanSums(b []byte, finish func(span int) func(block []byte)) []string {
	out := make([]string, chunkCount(int64(len(b))))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(out)) {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < len(out); i = int(next.Add(1) - 1) {
				span := b[i*ChunkSize : min((i+1)*ChunkSize, len(b))]
				var complete func([]byte)
				if finish != nil {
					complete = finish(i)
				}
				if complete == nil {
					out[i] = sum(span)
					continue
				}
				h := sha256.New()
				for at := 0; at < len(span); at += hashBlock {
					block := span[at:min(at+hashBlock, len(span))]
					complete(block)
					h.Write(block)
				}
				out[i] = hex.EncodeToString(h.Sum(nil))
			}
		})
	}
	wg.Wait()
	return out
}

func roundUp(n, a int64) int64 { return (n + a - 1) / a * a }

func sum(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

// viewBytes wraps a mapped view, which lives outside the Go heap and stays
// valid until UnmapViewOfFile.
func viewBytes(addr uintptr, n int64) []byte {
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return unsafe.Slice((*byte)(p), n)
}

// fillStored reads a stored source chunk by chunk into view with positioned
// reads on several handles, hashing each chunk as its read completes. A
// sector-aligned source is read unbuffered, so the file cache keeps no second
// copy of the bytes.
func fillStored(ctx context.Context, src Extent, view []byte) ([]string, error) {
	n := chunkCount(src.Length)
	digests := make([]string, n)
	unbuffered := src.Offset%sectorAlign == 0
	workers := min(runtime.NumCPU(), maxReaders, n)
	var next atomic.Int64
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			r, err := openStored(src.Path, unbuffered)
			if err != nil {
				errs[w] = fmt.Errorf("%w: %v", ErrSource, err)
				return
			}
			defer r.Close()
			for {
				i := next.Add(1) - 1
				if i >= int64(n) {
					return
				}
				if err := ctx.Err(); err != nil {
					errs[w] = err
					return
				}
				pos := i * ChunkSize
				need := min(ChunkSize, src.Length-pos)
				get := need
				if unbuffered {
					get = roundUp(need, sectorAlign)
				}
				got, err := r.readAt(view[pos:pos+get], src.Offset+pos)
				if got < need {
					errs[w] = fmt.Errorf("%w: %s gave %d of %d bytes at offset %d: %v", ErrSource, src.Path, got, need, src.Offset+pos, err)
					return
				}
				digests[i] = sum(view[pos : pos+need])
			}
		})
	}
	wg.Wait()
	// An unbuffered read rounds its length up to a sector and may have read
	// the container's following bytes past the object.
	clear(view[src.Length:])
	return digests, errors.Join(errs...)
}

// storedReader reads at file offsets with no shared file position.
type storedReader struct {
	h          windows.Handle
	unbuffered bool
}

func openStored(path string, unbuffered bool) (*storedReader, error) {
	p, err := windows.UTF16PtrFromString(extended(path))
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if unbuffered {
		flags = windows.FILE_FLAG_NO_BUFFERING
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	return &storedReader{h: h, unbuffered: unbuffered}, nil
}

// readAt fills buf from offset until it is full or the file ends, and
// reports how many bytes it read.
func (r *storedReader) readAt(buf []byte, offset int64) (int64, error) {
	var got int64
	for got < int64(len(buf)) {
		at := offset + got
		ov := windows.Overlapped{Offset: uint32(at), OffsetHigh: uint32(at >> 32)}
		var done uint32
		err := windows.ReadFile(r.h, buf[got:], &done, &ov)
		got += int64(done)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || (err == nil && done == 0) {
			return got, io.EOF
		}
		if err != nil {
			return got, err
		}
		if r.unbuffered && done%sectorAlign != 0 {
			// A short unbuffered read is the file's end.
			return got, io.EOF
		}
	}
	return got, nil
}

func (r *storedReader) Close() error { return windows.CloseHandle(r.h) }

// RegionReader reads byte ranges of a STORED region through the same
// unbuffered reader a stored fill uses, so the file cache keeps no copy of
// the bytes (INF-MS8). Each goroutine opens its own; it has no shared file
// position. Offsets are relative to the region's start.
type RegionReader struct {
	r       *storedReader
	src     Extent
	scratch []byte
}

// OpenRegion opens one reader of src's Packed stored bytes.
func OpenRegion(src Extent) (*RegionReader, error) {
	if err := checkRange(src); err != nil {
		return nil, err
	}
	r, err := openStored(src.Path, true)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSource, err)
	}
	return &RegionReader{r: r, src: src}, nil
}

// ReadAt fills p from offset off of the region. Unbuffered reads take whole
// sectors at sector offsets, so it reads the sectors covering the range into
// its own aligned buffer and copies the range out.
func (g *RegionReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || int64(len(p)) > g.src.Packed-off {
		return 0, fmt.Errorf("%w: %d bytes at %d lie beyond the region's %d", ErrSource, len(p), off, g.src.Packed)
	}
	pos := g.src.Offset + off
	start := pos &^ (sectorAlign - 1)
	end := roundUp(pos+int64(len(p)), sectorAlign)
	need := int(end - start)
	if len(g.scratch) < need {
		g.scratch = alignedBuffer(need)
	}
	got, err := g.r.readAt(g.scratch[:need], start)
	if got < pos-start+int64(len(p)) {
		return 0, fmt.Errorf("%w: %s gave %d of %d bytes at offset %d: %v", ErrSource, g.src.Path, max(0, got-(pos-start)), len(p), pos, err)
	}
	return copy(p, g.scratch[pos-start:]), nil
}

// Close closes the reader's handle.
func (g *RegionReader) Close() error { return g.r.Close() }

// alignedBuffer gives n bytes starting on a sector boundary, as an
// unbuffered read needs of its buffer.
func alignedBuffer(n int) []byte {
	b := make([]byte, n+sectorAlign)
	skip := (sectorAlign - int(uintptr(unsafe.Pointer(&b[0]))%sectorAlign)) % sectorAlign
	return b[skip : skip+n : skip+n]
}

// extended gives a drive or UNC path the \\?\ prefix, so a path beyond
// MAX_PATH opens as Go's os.Open would open it.
func extended(p string) string {
	// A \\?\ path is passed to the file system as written, without the
	// normalization of "." and ".." Win32 applies to other paths.
	p = filepath.Clean(p)
	switch {
	case strings.HasPrefix(p, `\\?\`):
		return p
	case strings.HasPrefix(p, `\\`):
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// fillDeflate decodes one DEFLATE stream into view on one core, hashing the
// whole object, each chunk and the CRC-32 as the bytes arrive. The stream
// must give exactly Length bytes; a back-reference may reach 32 KiB behind
// any point, so one stream decodes in order.
func fillDeflate(ctx context.Context, src Extent, view []byte) ([]string, string, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrSource, err)
	}
	defer f.Close()
	fr := flate.NewReader(bufio.NewReaderSize(io.NewSectionReader(f, src.Offset, src.Packed), 1<<20))
	defer fr.Close()
	whole, crc := sha256.New(), crc32.NewIEEE()
	digests := make([]string, 0, chunkCount(src.Length))
	for pos := int64(0); pos < src.Length; pos += ChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		part := view[pos:min(pos+ChunkSize, src.Length)]
		if n, err := io.ReadFull(fr, part); err != nil {
			return nil, "", fmt.Errorf("%w: %s's stream ends after %d of %d bytes: %v", ErrSource, src.Path, pos+int64(n), src.Length, err)
		}
		digests = append(digests, sum(part))
		whole.Write(part)
		crc.Write(part)
	}
	var one [1]byte
	if _, err := io.ReadFull(fr, one[:]); err != io.EOF {
		return nil, "", fmt.Errorf("%w: %s's stream runs past %d bytes: %v", ErrSource, src.Path, src.Length, err)
	}
	if crc.Sum32() != src.CRC32 {
		return nil, "", fmt.Errorf("%w: %s's stream fails its CRC-32", ErrSource, src.Path)
	}
	return digests, hex.EncodeToString(whole.Sum(nil)), nil
}

// Length is the object's size in bytes.
func (s *Section) Length() int64 { return s.src.Length }

// Digest is the object's "sha256:" digest the section's bytes were proved
// against.
func (s *Section) Digest() string { return s.digest }

// ChunkDigests are the hex SHA-256 of each ChunkSize span of the object, in
// order; the last span is the remainder.
func (s *Section) ChunkDigests() []string { return append([]string(nil), s.chunks...) }

// Extent is what the section was filled from.
func (s *Section) Extent() Extent { return s.src }

// RecordErr reports, wrapping ErrRecord, that a proven first fill through
// FillDecoded could not write OA's span record; it is nil otherwise. The
// section's bytes are proven either way.
func (s *Section) RecordErr() error { return s.recordErr }

// Handle is the section's handle in this process, zero once it is freed. An
// engine never receives it; it receives a Loan's read-only duplicate.
func (s *Section) Handle() uintptr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uintptr(s.h)
}

// Freed reports whether the section's memory has been given back.
func (s *Section) Freed() bool { return s.Handle() == 0 }

// Lend opens one loan: an inheritable duplicate of the section with map-read
// and query access only, for the handle list of the engine about to start
// (INF-MS4). The loan holds the section until it ends: call End if the engine
// does not start. A freed or closed section lends nothing.
func (s *Section) Lend() (*Loan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.h == 0 || s.closed {
		return nil, ErrReleased
	}
	var ro windows.Handle
	cur := windows.CurrentProcess()
	if err := windows.DuplicateHandle(cur, s.h, cur, &ro, sectionMapRead|sectionQuery, true, 0); err != nil {
		return nil, fmt.Errorf("modelfiles: read-only duplicate: %w", err)
	}
	s.active++
	return &Loan{s: s, h: ro, value: uintptr(ro)}, nil
}

// Close ends the owner's use of the section: it lends nothing more and is
// freed now, or when its last open loan ends.
func (s *Section) Close() error {
	s.mu.Lock()
	s.closed = true
	idle := s.active == 0
	var h windows.Handle
	if idle {
		h = s.takeLocked()
	}
	s.mu.Unlock()
	if idle {
		s.finish(h)
	}
	return nil
}

func (s *Section) endLoan() {
	s.mu.Lock()
	s.active--
	// No warm-residency rule exists for sections: the last loan's end frees
	// it (INF-MS5). The section is closed and its handle taken in the same
	// critical section that saw the count reach zero, so no Lend between
	// that moment and the close can succeed.
	idle := s.active == 0
	var h windows.Handle
	if idle {
		h = s.takeLocked()
	}
	s.mu.Unlock()
	if idle {
		s.finish(h)
	}
}

// free gives the section back on Fill's refusal.
func (s *Section) free() {
	s.mu.Lock()
	h := s.takeLocked()
	s.mu.Unlock()
	s.finish(h)
}

// takeLocked closes the section to new loans and takes its handle; s.mu is
// held.
func (s *Section) takeLocked() windows.Handle {
	h := s.h
	s.h, s.closed = 0, true
	return h
}

// finish closes the section's handle, which gives the memory back once no
// engine maps the section, and ends the caller's holding once.
func (s *Section) finish(h windows.Handle) {
	if testHookIdle != nil {
		testHookIdle()
	}
	if h != 0 {
		//unchecked: closing the section's own handle; nothing else can be done with a failure
		windows.CloseHandle(h)
	}
	s.once.Do(func() {
		if s.release != nil {
			s.release()
		}
	})
}

// Loan is one engine's lease on a section. It holds the section until End:
// at the engine's exit through EndWhen, or at once when the engine does not
// start.
type Loan struct {
	s     *Section
	value uintptr

	mu    sync.Mutex
	h     windows.Handle
	ended bool
}

// Handle is the read-only handle's value: the engine inherits it at this
// value, so it goes in supervision's Options.Inherit and on the engine's
// command line with the section's Length.
func (l *Loan) Handle() uintptr { return l.value }

// Started closes this process's copy of the read-only handle once the
// engine holds its own inherited copy.
func (l *Loan) Started() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closeHandle()
}

func (l *Loan) closeHandle() {
	if l.h != 0 {
		//unchecked: the duplicate is this process's own; the engine's copy is unaffected
		windows.CloseHandle(l.h)
		l.h = 0
	}
}

// End ends the lease: the engine has exited, reported that it released the
// section, or did not start. Ending a loan twice is one end.
func (l *Loan) End() {
	l.mu.Lock()
	if l.ended {
		l.mu.Unlock()
		return
	}
	l.ended = true
	l.closeHandle()
	l.mu.Unlock()
	l.s.endLoan()
}

// EndWhen ends the lease when exited closes: give it the channel that closes
// once the engine's whole process tree has exited.
func (l *Loan) EndWhen(exited <-chan struct{}) {
	go func() {
		<-exited
		l.End()
	}()
}
