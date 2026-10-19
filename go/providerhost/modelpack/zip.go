package modelpack

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
)

// An OA model archive (INF-MP6; research/model-locators/MODEL-ARCHIVE.md
// §4) is one zip layer of STORED entries: first the index, oa-archive.json,
// then one entry per model file. A file whose units all stay stored is
// written under its own path, so every zip tool extracts the file itself; a
// file with encoded units is written as <path>.oa-units, which only OA
// decodes. Every file entry's data starts on a 64 KiB boundary.
const (
	archiveIndexEntry = "oa-archive.json"
	unitsSuffix       = ".oa-units"
	archiveFormat     = "oa-model-archive"
	archiveVersion    = 1
	// packAlign is the boundary a file entry's data starts on: llamafile's
	// alignment and OA's for every STORED entry (DECISION.md §7 rule 9).
	packAlign = 64 << 10
	// maxIndexBytes bounds the index entry a reader parses.
	maxIndexBytes = 64 << 20
)

// ArchiveTensor is one tensor of an archived weight file: its name, dtype
// and shape as the file names them, its decoded bytes [Offset,
// Offset+Length) in the file, the SHA-256 of those bytes, and for BF16 and
// F16 tensors the SHA-256 of their values widened exactly to little-endian
// F32, which is equal for the same values held in either dtype.
type ArchiveTensor struct {
	Name       string  `json:"name"`
	DType      string  `json:"dtype"`
	Shape      []int64 `json:"shape,omitempty"`
	Offset     int64   `json:"offset"`
	Length     int64   `json:"length"`
	Digest     string  `json:"digest"`
	Normalized string  `json:"normalized_digest,omitempty"`
}

// ArchiveFile is one model file in an archive: its slash-separated relative
// Path, the zip Entry holding it, its WeightFormat when it is a weight file
// ("safetensors" or "gguf"), its pack Index (INF-MP1; every unit stored
// when Entry is Path), and its TensorTable. Extent is where ReadZip found
// the entry's data in the archive file.
type ArchiveFile struct {
	Path         string            `json:"path"`
	Entry        string            `json:"entry"`
	WeightFormat string            `json:"format,omitempty"`
	Index        Index             `json:"index"`
	TensorTable  []ArchiveTensor   `json:"tensors,omitempty"`
	Extent       modelfiles.Extent `json:"-"`
}

// ModelArchive is an archive's index entry, oa-archive.json; Layout is its
// format name, "oa-model-archive".
type ModelArchive struct {
	Layout  string        `json:"format"`
	Version int           `json:"version"`
	Files   []ArchiveFile `json:"files"`
}

// ArchiveSource is one file WriteZip puts in an archive: its
// slash-separated relative Path, its bytes, and for a weight file its
// WeightFormat and Tensors (ReadTensors).
type ArchiveSource struct {
	Path         string
	Data         []byte
	WeightFormat string
	Tensors      []Tensor
	// Base, when not nil, is the base a weight file's delta units are
	// coded against (PackDelta, INF-MP12). Keep its borrowed bytes immutable
	// and alive for the full WriteZip call.
	Base *DeltaBase
}

// counter counts and CRCs what passes through it to w (nil: nothing).
type counter struct {
	w   io.Writer
	n   int64
	crc uint32
}

func (c *counter) Write(p []byte) (int, error) {
	c.crc = crc32.Update(c.crc, crc32.IEEETable, p)
	c.n += int64(len(p))
	if c.w == nil {
		return len(p), nil
	}
	return c.w.Write(p)
}

// checkArchivePath accepts a relative, slash-separated UTF-8 path that every
// platform OA runs on can extract as written: no empty, "." or ".."
// element; no backslash, colon, NUL or other control character, or any of
// <>"|?*; no element ending in a dot or a space; and no element whose name
// before its first dot is a Windows device name (windowsDevice), in any
// letter case.
func checkArchivePath(p string) error {
	bad := func(why string) error { return fmt.Errorf("%w: archive path %q: %s", ErrCorrupt, p, why) }
	switch {
	case p == "" || !utf8.ValidString(p):
		return bad("not a UTF-8 path")
	case p == archiveIndexEntry:
		return bad("the index's name")
	case strings.HasPrefix(p, "/"):
		return bad("absolute")
	case strings.ContainsAny(p, "\\:<>\"|?*"):
		return bad(`holds one of \:<>"|?*`)
	case strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return bad("holds a control character")
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "" || part == "." || part == "..":
			return bad("an empty, . or .. element")
		case strings.HasSuffix(part, ".") || strings.HasSuffix(part, " "):
			return bad("an element ending in a dot or a space")
		case windowsDevice(part):
			return bad("a Windows device name")
		}
	}
	return nil
}

// windowsDevice reports a path element Windows opens as a device, whatever
// its extension: CON, PRN, AUX, NUL, CONIN$, CONOUT$, and COM or LPT with
// one digit, the superscripts ¹, ² and ³ included.
func windowsDevice(part string) bool {
	stem, _, _ := strings.Cut(strings.ToUpper(part), ".")
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	port, ok := strings.CutPrefix(stem, "COM")
	if !ok {
		port, ok = strings.CutPrefix(stem, "LPT")
	}
	if !ok {
		return false
	}
	switch port {
	case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	}
	return false
}

// tensorTable digests each named tensor of a weight file.
func tensorTable(data []byte, tensors []Tensor) []ArchiveTensor {
	var out []ArchiveTensor
	for _, t := range tensors {
		b := data[t.Offset : t.Offset+t.Length]
		sum := sha256.Sum256(b)
		a := ArchiveTensor{Name: t.Name, DType: t.DType, Shape: t.Shape, Offset: t.Offset, Length: t.Length,
			Digest: "sha256:" + hex.EncodeToString(sum[:])}
		if widen := widener(t.DType); widen != nil && len(b)%2 == 0 {
			h := sha256.New()
			var f [4096]byte
			for at := 0; at < len(b); {
				n := 0
				for ; n+4 <= len(f) && at < len(b); at += 2 {
					binary.LittleEndian.PutUint32(f[n:], widen(binary.LittleEndian.Uint16(b[at:])))
					n += 4
				}
				h.Write(f[:n])
			}
			a.Normalized = "sha256:" + hex.EncodeToString(h.Sum(nil))
		}
		out = append(out, a)
	}
	return out
}

// widener widens a 16-bit float dtype's value to F32 bits exactly, a NaN's
// sign and payload carried over unquieted.
func widener(dtype string) func(uint16) uint32 {
	switch dtype {
	case "BF16":
		return func(v uint16) uint32 { return uint32(v) << 16 }
	case "F16":
		return func(h uint16) uint32 {
			sign := uint32(h&0x8000) << 16
			exp := uint32(h>>10) & 0x1f
			mant := uint32(h & 0x3ff)
			switch {
			case exp == 0x1f:
				return sign | 0x7f800000 | mant<<13
			case exp == 0 && mant == 0:
				return sign
			case exp == 0:
				e := uint32(113)
				for mant&0x400 == 0 {
					mant <<= 1
					e--
				}
				return sign | e<<23 | (mant&0x3ff)<<13
			}
			return sign | (exp+112)<<23 | mant<<13
		}
	}
	return nil
}

// WriteZip writes an OA model archive of files to w (INF-MP6) and returns
// its index. Each file is packed as Pack does with rates; a file whose
// units all stay stored is written under its own path, and one with encoded
// units as <path>.oa-units. It encodes each file twice, first to learn its
// entry's size and CRC-32 and the index that precedes the entries, and
// refuses the archive if the two encodings differ. records, when not nil,
// receives OA's span record of each weight file: WriteZip hashed every byte
// of it (INF-MP1, INF-MS7). Any base supplied through ArchiveSource.Base is
// borrowed; keep its bytes immutable and alive until this call returns,
// across both encoding passes.
func WriteZip(w io.Writer, files []ArchiveSource, rates Rates, records *modelfiles.SpanRecords) (ModelArchive, error) {
	archive := ModelArchive{Layout: archiveFormat, Version: archiveVersion}
	type plan struct {
		size int64
		crc  uint32
	}
	plans := make([]plan, len(files))
	seen := map[string]bool{}
	for i, f := range files {
		if err := checkArchivePath(f.Path); err != nil {
			return ModelArchive{}, err
		}
		probe := &counter{}
		index, err := PackDelta(probe, f.Data, f.Tensors, rates, f.Base)
		if err != nil {
			return ModelArchive{}, fmt.Errorf("%s: %w", f.Path, err)
		}
		entry := f.Path
		for _, c := range index.Chunks {
			if c.Encoding != EncodingStored {
				entry = f.Path + unitsSuffix
				break
			}
		}
		// A path and an entry name are unique in any letter case, as
		// Windows extracts them: no file's path may be another's entry.
		names := []string{f.Path}
		if entry != f.Path {
			names = append(names, entry)
		}
		for _, name := range names {
			key := strings.ToLower(name)
			if seen[key] {
				return ModelArchive{}, fmt.Errorf("%w: archive path %q twice", ErrCorrupt, name)
			}
			seen[key] = true
		}
		archive.Files = append(archive.Files, ArchiveFile{Path: f.Path, Entry: entry, WeightFormat: f.WeightFormat,
			Index: index, TensorTable: tensorTable(f.Data, f.Tensors)})
		plans[i] = plan{probe.n, probe.crc}
	}
	body, err := json.Marshal(archive)
	if err != nil {
		return ModelArchive{}, err
	}
	z := &zipWriter{w: w, forceZip64: forceZip64}
	if err := z.begin(archiveIndexEntry, int64(len(body)), crc32.ChecksumIEEE(body), false); err != nil {
		return ModelArchive{}, err
	}
	if _, err := z.Write(body); err != nil {
		return ModelArchive{}, err
	}
	for i, f := range files {
		a := &archive.Files[i]
		if err := z.begin(a.Entry, plans[i].size, plans[i].crc, true); err != nil {
			return ModelArchive{}, err
		}
		out := &counter{w: z}
		index, err := PackDelta(out, f.Data, f.Tensors, rates, f.Base)
		if err != nil {
			return ModelArchive{}, fmt.Errorf("%s: %w", f.Path, err)
		}
		if out.n != plans[i].size || out.crc != plans[i].crc || len(index.Chunks) != len(a.Index.Chunks) {
			return ModelArchive{}, fmt.Errorf("%w: %s: two encodings of one file differ", ErrCorrupt, f.Path)
		}
	}
	if err := z.close(); err != nil {
		return ModelArchive{}, err
	}
	if records != nil {
		for _, a := range archive.Files {
			if a.WeightFormat == "" {
				continue
			}
			if err := records.Put(modelfiles.SpanRecord{Digest: a.Index.Digest, Length: a.Index.Length, Spans: a.Index.Spans}); err != nil {
				return ModelArchive{}, err
			}
		}
	}
	return archive, nil
}

// forceZip64 makes WriteZip write Zip64 fields everywhere; a test sets it.
var forceZip64 = false

// ReadZip reads the OA model archive at path, an absolute path, through its
// zip directory: the index and, for each file, the extent of its entry's
// data. It refuses (ErrCorrupt) an archive whose index is missing, not
// STORED, over 64 MiB, of another format or version, or does not parse with
// no unknown field; two entry names, or two of the index's paths and entry
// names, equal in some letter case; an entry the index does not name; a
// file whose path checkArchivePath refuses, whose entry is not its path (every unit
// stored) or its path with .oa-units (some unit encoded), is not a STORED,
// unencrypted entry starting on a 64 KiB boundary, or whose index is not
// whole by INF-MP1 or does not use exactly the entry's bytes; and a tensor
// outside its file. It reads no file's data.
func ReadZip(file string) (ModelArchive, error) {
	refuse := func(format string, args ...any) (ModelArchive, error) {
		return ModelArchive{}, fmt.Errorf("%w: %s: %s", ErrCorrupt, file, fmt.Sprintf(format, args...))
	}
	if !filepath.IsAbs(file) {
		return refuse("not an absolute path")
	}
	zr, err := zip.OpenReader(file)
	if err != nil {
		return refuse("%v", err)
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	folded := map[string]bool{}
	for _, f := range zr.File {
		// Windows extracts names in any letter case to one file.
		key := strings.ToLower(f.Name)
		if folded[key] {
			return refuse("entry %q twice in some letter case", f.Name)
		}
		folded[key] = true
		entries[f.Name] = f
		if f.Method != zip.Store || f.CompressedSize64 != f.UncompressedSize64 || f.Flags&0x1 != 0 {
			return refuse("%s is not a STORED, unencrypted entry", f.Name)
		}
	}
	idx := entries[archiveIndexEntry]
	if idx == nil || idx.UncompressedSize64 > maxIndexBytes {
		return refuse("no index entry %s of at most 64 MiB", archiveIndexEntry)
	}
	rc, err := idx.Open()
	if err != nil {
		return refuse("%v", err)
	}
	body, err := io.ReadAll(rc) // archive/zip checks the CRC-32 at the end
	err = errors.Join(err, rc.Close())
	if err != nil {
		return refuse("index: %v", err)
	}
	var archive ModelArchive
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&archive); err != nil {
		return refuse("index: %v", err)
	}
	if archive.Layout != archiveFormat || archive.Version != archiveVersion {
		return refuse("index of format %q version %d", archive.Layout, archive.Version)
	}
	// Every path and entry name is unique in any letter case, and every zip
	// entry besides the index is exactly one file's entry.
	names := map[string]bool{strings.ToLower(archiveIndexEntry): true}
	used := map[string]bool{}
	for i := range archive.Files {
		a := &archive.Files[i]
		if err := checkArchivePath(a.Path); err != nil {
			return ModelArchive{}, err
		}
		pathKey, entryKey := strings.ToLower(a.Path), strings.ToLower(a.Entry)
		if names[pathKey] || (entryKey != pathKey && names[entryKey]) {
			return refuse("%s or its entry %q named twice in some letter case", a.Path, a.Entry)
		}
		names[pathKey], names[entryKey] = true, true
		encoded := false
		for _, c := range a.Index.Chunks {
			encoded = encoded || c.Encoding != EncodingStored
		}
		want := a.Path
		if encoded {
			want += unitsSuffix
		}
		f := entries[a.Entry]
		if a.Entry != want || f == nil {
			return refuse("%s is held in %q, want %q", a.Path, a.Entry, want)
		}
		used[a.Entry] = true
		if err := modelfiles.CheckDigest(a.Index.Digest); err != nil {
			return refuse("%s: %v", a.Path, err)
		}
		if _, err := checkIndex(a.Index); err != nil {
			return refuse("%s: %v", a.Path, err)
		}
		if _, err := checkSpans(a.Index.Length, a.Index.Spans); err != nil {
			return refuse("%s: %v", a.Path, err)
		}
		off, err := f.DataOffset()
		if err != nil {
			return refuse("%v", err)
		}
		if off%packAlign != 0 {
			return refuse("%s starts at %d, off the 64 KiB boundary", a.Entry, off)
		}
		size := int64(f.CompressedSize64)
		var end int64
		for _, c := range a.Index.Chunks {
			if c.Packed > size-c.PackedOffset {
				return refuse("%s: a unit lies beyond the entry's %d bytes", a.Path, size)
			}
			end = max(end, c.PackedOffset+c.Packed)
		}
		if end != size {
			return refuse("%s: units use %d of the entry's %d bytes", a.Path, end, size)
		}
		for _, t := range a.TensorTable {
			if t.Offset < 0 || t.Length < 0 || t.Length > a.Index.Length-t.Offset {
				return refuse("%s: tensor %q outside the file", a.Path, t.Name)
			}
		}
		a.Extent = modelfiles.Extent{Path: file, Offset: off, Packed: size, Length: a.Index.Length, Method: zip.Store}
	}
	for name := range entries {
		if name != archiveIndexEntry && !used[name] {
			return refuse("entry %q is not in the index", name)
		}
	}
	return archive, nil
}

// Lookup is the archive's file whose object digest is digest.
func (a ModelArchive) Lookup(digest string) (ArchiveFile, bool) {
	for _, f := range a.Files {
		if f.Index.Digest == digest {
			return f, true
		}
	}
	return ArchiveFile{}, false
}

// FillZip places the object of digest, held in the OA model archive at
// file, in a new memory section through modelfiles' FillDecoded (INF-MP7,
// INF-MS8). It needs OA's span records. The whole index is checked, and an
// archive holding no file of that digest refused (modelfiles.ErrDigest),
// before a section exists or a unit is read. Each worker reads units through
// its own modelfiles.RegionReader, the unbuffered reader of a stored fill,
// and decodes them on up to workers goroutines straight into the section.
// exp-fixed3-v1 units leave their escapes to FillDecoded's hash pass, which
// applies them to each block just before hashing it (escapePass); that
// pass is the one check of the bytes, against OA's record of the object or
// its whole digest.
// release, when not nil, is called exactly once, as FillDecoded calls it. A
// section returned is proven; its RecordErr reports a span record that
// could not be written.
func FillZip(ctx context.Context, records *modelfiles.SpanRecords, file, digest string, workers int, release func()) (*modelfiles.Section, error) {
	return FillZipWith(ctx, records, file, digest, FillOptions{Workers: workers, Release: release})
}

// FillOptions are FillZipWith's options: FillZip's workers and release,
// and an optional decoder of exp-fixed3-v1 units beside the CPU.
type FillOptions struct {
	Workers int
	Release func()
	// Device, when not nil, decodes the archive's exp-fixed3-v1 units of at
	// most ChunkBytes in batches while the CPU decodes every other unit
	// (INF-MP10). Give only a device that passed CheckFixedDevice.
	Device FixedDevice
	// Report, when not nil, receives how the fill decoded its
	// exp-fixed3-v1 units.
	Report *FillReport
	// Base, for an object holding delta units (INF-MP12), is the proven
	// section of its base: a section exists only once its bytes were proved
	// against its digest (INF-MS2, INF-MS8). The fill reads it through a
	// loan of its own, and a section is freed at its last loan's end
	// (INF-MS5): a base no engine holds is freed when the fill ends. A
	// caller that uses the base after the fill, or fills further deltas
	// from it, keeps a loan of its own on it until it is done.
	Base *modelfiles.Section
}

// FillZipWith fills as FillZip does. With o.Device it uses the device when
// the archive file holds exp-fixed3-v1 units the device can take, the
// device reports no error, and no other fill holds it; otherwise the CPU
// decodes every unit. A device error hands that batch and every later one
// to the CPU. The proof is the fill's span check, as for every fill: when
// it refuses the bytes of a fill the device took part in, the fill runs
// once more on the CPU alone, and if that fill is proven the device is
// distrusted. release is called exactly once either way.
//
// An object holding delta units (INF-MP12) needs o.Base. Before a section
// is made or a unit read, such an object without a base is refused
// (ErrBase), and one whose base section is another object, by digest or
// length, is refused (modelfiles.ErrDigest). The fill holds a loan on the
// base section while it decodes, so the base stays in memory until the
// last delta unit is read, and maps it read-only. The fine-tune's bytes are
// then proved as every fill's are, span by span against OA's record or
// whole against its digest: a base that matches the delta's claim but is
// not the one the delta was made from fails that proof. An object with no
// base ignores o.Base.
func FillZipWith(ctx context.Context, records *modelfiles.SpanRecords, file, digest string, o FillOptions) (*modelfiles.Section, error) {
	report := o.Report
	if report == nil {
		report = new(FillReport)
	}
	*report = FillReport{}
	var a ArchiveFile
	err := errors.New("modelpack: a fill from an archive needs OA's span records")
	if records != nil {
		var archive ModelArchive
		if archive, err = ReadZip(file); err == nil {
			var ok bool
			if a, ok = archive.Lookup(digest); !ok {
				err = fmt.Errorf("%w: the archive holds no object %s", modelfiles.ErrDigest, digest)
			}
		}
	}
	var proven ProvenBase
	done := func() {}
	if err == nil && a.Index.Base != nil {
		switch {
		case o.Base == nil:
			err = fmt.Errorf("%w: base %s", ErrBase, a.Index.Base.Digest)
		case o.Base.Digest() != a.Index.Base.Digest || o.Base.Length() != a.Index.Base.Length:
			err = fmt.Errorf("%w: the base section is %s of %d bytes, the delta names %s of %d",
				errBaseDigest, o.Base.Digest(), o.Base.Length(), a.Index.Base.Digest, a.Index.Base.Length)
		default:
			proven, done, err = borrowBase(o.Base)
		}
	}
	if err != nil {
		if o.Release != nil {
			o.Release()
		}
		return nil, err
	}
	defer done()
	baseBytes, err := baseFor(a.Index, proven)
	if err != nil {
		if o.Release != nil {
			o.Release()
		}
		return nil, err
	}
	fill := func(dev FixedDevice, release func()) (*modelfiles.Section, error) {
		var df *deviceFill
		if dev != nil {
			df = &deviceFill{dev: dev}
		}
		decode := func(ctx context.Context, view []byte, _ []string) (func(int) func([]byte), error) {
			open := func() (readFunc, func(), error) {
				r, err := modelfiles.OpenRegion(a.Extent)
				if err != nil {
					return nil, nil, err
				}
				//unchecked: closing a read-only handle after its last read
				return readerAt(r), func() { r.Close() }, nil
			}
			patches := make([]*fixedPatch, len(a.Index.Chunks))
			start := time.Now()
			err := unpack(ctx, view, a.Index, a.Index.Spans, o.Workers, false, open, patches, df, baseBytes)
			if df != nil {
				df.report(report)
			}
			report.Decode = time.Since(start)
			if err != nil {
				return nil, err
			}
			if beforeEscapePass != nil {
				beforeEscapePass(patches)
			}
			return escapePass(view, a.Index.Chunks, patches), nil
		}
		s, err := records.FillDecoded(ctx, a.Extent, digest, decode, release)
		if df == nil {
			for _, c := range a.Index.Chunks {
				if c.Encoding == EncodingExpFixed3V1 {
					report.CPUUnits++
				}
			}
		}
		return s, err
	}
	dev := o.Device
	if dev != nil && (dev.Err() != nil || !slices.ContainsFunc(a.Index.Chunks, func(c ChunkEntry) bool { return deviceUnit(&c) })) {
		dev = nil
	}
	if dev != nil && !dev.TryAcquire() {
		report.Busy = true
		dev = nil
	}
	if dev == nil {
		return fill(nil, o.Release)
	}
	defer dev.Done()
	// The first fill's release waits until it is known whether the fill runs
	// again, so release is called once.
	held := &heldRelease{release: o.Release}
	held.hold.Store(true)
	s, err := fill(dev, held.call)
	held.hold.Store(false)
	if err == nil {
		return s, nil
	}
	if errors.Is(err, modelfiles.ErrDigest) && report.DeviceUnits > 0 && ctx.Err() == nil {
		first := *report
		*report = FillReport{}
		s, cpuErr := fill(nil, o.Release)
		report.DeviceUnits, report.Batches, report.DeviceErr, report.Retried = first.DeviceUnits, first.Batches, first.DeviceErr, true
		if cpuErr == nil {
			bad := fmt.Errorf("modelpack: the proof refused device output the CPU decode of the same units passed: %w", err)
			dev.Distrust(bad)
			log.Printf("%v; the device is distrusted", bad)
		}
		return s, cpuErr
	}
	if held.held.Load() && o.Release != nil {
		o.Release()
	}
	return nil, err
}

// beforeEscapePass, when a test sets it, sees a fill's escape patches after
// the decode and before the hash pass; a test alters them to show the proof
// refuses the bytes.
var beforeEscapePass func(patches []*fixedPatch)

// heldRelease calls release, except while hold is set, when it only
// records the call.
type heldRelease struct {
	release    func()
	hold, held atomic.Bool
}

func (h *heldRelease) call() {
	if h.hold.Load() {
		h.held.Store(true)
		return
	}
	if h.release != nil {
		h.release()
	}
}

// RestoreFile writes one file of the archive ReadZip read to w, as Restore
// does, reading its entry through one file handle the decoders share.
func RestoreFile(ctx context.Context, w io.Writer, a ArchiveFile, workers int) error {
	return RestoreFileWithBase(ctx, w, a, ProvenBase{}, workers)
}

// RestoreFileWithBase restores as RestoreFile does a file that may hold delta
// units, against base, as RestoreWithBase does. Keep the bytes borrowed by
// base immutable and alive until this call returns.
func RestoreFileWithBase(ctx context.Context, w io.Writer, a ArchiveFile, base ProvenBase, workers int) error {
	f, err := os.Open(a.Extent.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	return RestoreWithBase(ctx, w, io.NewSectionReader(f, a.Extent.Offset, a.Extent.Packed), a.Index, base, workers)
}

// Bases lists the base objects the archive's delta units name: a store
// holding this archive keeps each of them while it keeps the archive
// (INF-MP12).
func (a ModelArchive) Bases() []BaseRef {
	var out []BaseRef
	for _, f := range a.Files {
		if f.Index.Base != nil && !slices.Contains(out, *f.Index.Base) {
			out = append(out, *f.Index.Base)
		}
	}
	return out
}
