//go:build windows

package modelfiles

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The test binary is also the engine stand-in: with OA_MODELFILES_CHILD set
// it maps the inherited section and reports what it could do with it.
func TestMain(m *testing.M) {
	if spec := os.Getenv("OA_MODELFILES_CHILD"); spec != "" {
		os.Exit(runChild(spec))
	}
	os.Exit(m.Run())
}

// payloadSize spans two chunks, the second partial, and no page multiple.
const payloadSize = ChunkSize + 12345

// payload is GGUF-like: a header, then bf16-shaped values whose high bytes
// repeat, so DEFLATE shrinks it as it shrinks real weights.
func payload() []byte {
	b := make([]byte, payloadSize)
	copy(b, "GGUF")
	binary.LittleEndian.PutUint32(b[4:], 3)
	x := uint32(7)
	for i := 8; i < len(b); i += 2 {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
		if i+1 < len(b) {
			b[i+1] = 0x3e + byte(x>>30&1)
		}
	}
	return b
}

func digestOf(b []byte) string {
	d := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(d[:])
}

// pack is one fixture folder: the payload as a loose file and as three
// members of one zip file.
type pack struct {
	dir, zip                            string
	data                                []byte
	loose, aligned, unaligned, deflated Extent
}

func writePack(t *testing.T) pack {
	t.Helper()
	dir := t.TempDir()
	p := pack{dir: dir, zip: filepath.Join(dir, "pkg.zip"), data: payload()}
	loose := filepath.Join(dir, "model.gguf")
	if err := os.WriteFile(loose, p.data, 0o600); err != nil {
		t.Fatal(err)
	}
	p.loose = Extent{Path: loose, Packed: payloadSize, Length: payloadSize, Method: zip.Store}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	w.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) { return flate.NewWriter(out, flate.BestSpeed) })
	stored := func(name string, align int64) {
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		start := int64(buf.Len()) + 30 + int64(len(name)) + 6
		pad := (33 - start%32) % 32 // one byte past a multiple of 32
		if align > 0 {
			pad = (align - start%align) % align
		}
		extra := make([]byte, 6+pad)
		binary.LittleEndian.PutUint16(extra[0:], 0xD935)
		binary.LittleEndian.PutUint16(extra[2:], uint16(2+pad))
		fw, err := w.CreateRaw(&zip.FileHeader{Name: name, Method: zip.Store, CRC32: crc32.ChecksumIEEE(p.data),
			CompressedSize64: payloadSize, UncompressedSize64: payloadSize, Extra: extra})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(p.data); err != nil {
			t.Fatal(err)
		}
	}
	// STORED members first: a DEFLATE member's last bytes reach the buffer
	// only when the next member opens, which would misplace the padding.
	stored("aligned.gguf", 65536)
	stored("unaligned.gguf", 0)
	readme, err := w.CreateHeader(&zip.FileHeader{Name: "readme.txt", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	readme.Write([]byte("a model package"))
	fw, err := w.CreateHeader(&zip.FileHeader{Name: "packed.gguf", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(p.data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.zip, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(p.zip)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		off, err := f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		src := Extent{Path: p.zip, Offset: off, Packed: int64(f.CompressedSize64), Length: int64(f.UncompressedSize64), Method: zip.Store, CRC32: f.CRC32}
		switch f.Name {
		case "aligned.gguf":
			p.aligned = src
		case "unaligned.gguf":
			p.unaligned = src
		case "packed.gguf":
			src.Method = zip.Deflate
			p.deflated = src
		}
	}
	if p.aligned.Offset%65536 != 0 || p.unaligned.Offset%sectorAlign == 0 || p.deflated.Packed >= payloadSize {
		t.Fatalf("fixture layout: aligned at %d, unaligned at %d, deflated %d bytes", p.aligned.Offset, p.unaligned.Offset, p.deflated.Packed)
	}
	return p
}

// counter is a release function that counts its calls.
type counter struct{ n atomic.Int32 }

func (c *counter) release() { c.n.Add(1) }

// mapped reads the section's bytes through a read-only loan in this process.
func mapped(t *testing.T, s *Section) []byte {
	t.Helper()
	loan, err := s.Lend()
	if err != nil {
		t.Fatal(err)
	}
	defer loan.End()
	addr, err := windows.MapViewOfFile(windows.Handle(loan.Handle()), windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(addr)
	return bytes.Clone(viewBytes(addr, s.Length()))
}

// TestFillFromEachSourceEqualsTheFileDigest: a loose file, an aligned and an
// unaligned STORED member and a DEFLATE member fill sections whose bytes,
// digest and chunk digests are the file's (INF-MS1, INF-MS2).
func TestFillFromEachSourceEqualsTheFileDigest(t *testing.T) {
	p := writePack(t)
	want := digestOf(p.data)
	chunks := []string{sum(p.data[:ChunkSize]), sum(p.data[ChunkSize:])}
	for name, src := range map[string]Extent{"loose": p.loose, "aligned": p.aligned, "unaligned": p.unaligned, "deflated": p.deflated} {
		var c counter
		s, err := Fill(context.Background(), src, want, c.release)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.Digest() != want || !slices.Equal(s.ChunkDigests(), chunks) || s.Length() != payloadSize {
			t.Fatalf("%s: digest %s, chunks %v", name, s.Digest(), s.ChunkDigests())
		}
		if !bytes.Equal(mapped(t, s), p.data) {
			t.Fatalf("%s: the section does not hold the object's bytes", name)
		}
		// mapped's loan was the last one: the section is freed with it.
		if !s.Freed() || c.n.Load() != 1 {
			t.Fatalf("%s: freed %v, released %d times", name, s.Freed(), c.n.Load())
		}
	}
}

// TestRefusedFillFreesTheSection: a digest mismatch, a corrupted stream and a
// range beyond the file free what Fill took and call release once (INF-MS2).
func TestRefusedFillFreesTheSection(t *testing.T) {
	p := writePack(t)
	other := digestOf([]byte("another object"))
	badCRC := p.deflated
	badCRC.CRC32++
	short := p.deflated
	short.Length++
	long := p.deflated
	long.Length--
	beyond := p.aligned
	beyond.Offset = p.aligned.Offset + 65536*1024
	for name, c := range map[string]struct {
		src    Extent
		digest string
		want   error
	}{
		"stored, other digest":   {p.aligned, other, ErrDigest},
		"deflated, other digest": {p.deflated, other, ErrDigest},
		"crc":                    {badCRC, digestOf(p.data), ErrSource},
		"stream ends early":      {short, digestOf(p.data), ErrSource},
		"stream runs long":       {long, digestOf(p.data[:payloadSize-1]), ErrSource},
		"beyond the file":        {beyond, digestOf(p.data), ErrSource},
	} {
		var n counter
		s, err := Fill(context.Background(), c.src, c.digest, n.release)
		if !errors.Is(err, c.want) || s != nil || n.n.Load() != 1 {
			t.Fatalf("%s: %v, section %v, released %d times", name, err, s, n.n.Load())
		}
	}
	var n counter
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fill(ctx, p.deflated, digestOf(p.data), n.release); !errors.Is(err, context.Canceled) || n.n.Load() != 1 {
		t.Fatalf("cancelled: %v, released %d times", err, n.n.Load())
	}
	// A digest of the right length that is not lower-case hex never reaches
	// a comparison.
	for _, digest := range []string{"sha256:" + strings.Repeat("g", 64), strings.ToUpper(digestOf(p.data)), digestOf(p.data)[7:]} {
		var n counter
		s, err := Fill(context.Background(), p.aligned, digest, n.release)
		if err == nil || errors.Is(err, ErrDigest) || errors.Is(err, ErrSource) || s != nil || n.n.Load() != 1 {
			t.Fatalf("digest %q: %v, released %d times", digest, err, n.n.Load())
		}
	}
}

// TestTheOwnersHandleCannotWrite: once Fill returns, the provider host's own
// handle maps for reading only and cannot be duplicated to write access, so
// the proof holds for the section's lifetime (INF-MS3).
func TestTheOwnersHandleCannotWrite(t *testing.T) {
	p := writePack(t)
	s, err := Fill(context.Background(), p.aligned, digestOf(p.data), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := windows.Handle(s.Handle())
	if addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE, 0, 0, 4096); err == nil {
		windows.UnmapViewOfFile(addr)
		t.Fatal("the owner's handle maps the section for writing")
	}
	var rw windows.Handle
	cur := windows.CurrentProcess()
	if err := windows.DuplicateHandle(cur, h, cur, &rw, sectionMapRead|0x0002, false, 0); err == nil {
		windows.CloseHandle(rw)
		t.Fatal("the owner's handle duplicates to map-write")
	}
	if !bytes.Equal(mapped(t, s), p.data) {
		t.Fatal("the sealed section does not hold the object's bytes")
	}
}

// TestLendRacingTheEndIsRefused: a Lend that runs after the last loan ended,
// or the owner closed an idle section, and before its handle is closed, is
// refused; it never receives a loan on a section whose holding was released
// (INF-MS5).
func TestLendRacingTheEndIsRefused(t *testing.T) {
	p := writePack(t)
	t.Cleanup(func() { testHookIdle = nil })
	for _, end := range []string{"last loan", "close"} {
		var c counter
		s, err := Fill(context.Background(), p.aligned, digestOf(p.data), c.release)
		if err != nil {
			t.Fatal(err)
		}
		var raced *Loan
		var racedErr error
		calls := 0
		testHookIdle = func() {
			if calls++; calls == 1 {
				raced, racedErr = s.Lend()
			}
		}
		if end == "last loan" {
			loan, err := s.Lend()
			if err != nil {
				t.Fatal(err)
			}
			loan.End()
		} else {
			s.Close()
		}
		testHookIdle = nil
		if racedErr == nil {
			raced.End()
			t.Fatalf("%s: a Lend in the gap received a live loan", end)
		}
		if !errors.Is(racedErr, ErrReleased) || !s.Freed() || c.n.Load() != 1 {
			t.Fatalf("%s: %v, freed %v, released %d times", end, racedErr, s.Freed(), c.n.Load())
		}
	}
}

// TestALoanWhoseEngineDidNotStartEnds: a loan taken for an engine that did
// not start holds the section until End, which frees it and releases the
// holding once (INF-MS4).
func TestALoanWhoseEngineDidNotStartEnds(t *testing.T) {
	p := writePack(t)
	var c counter
	s, err := Fill(context.Background(), p.aligned, digestOf(p.data), c.release)
	if err != nil {
		t.Fatal(err)
	}
	loan, err := s.Lend()
	if err != nil {
		t.Fatal(err)
	}
	if s.Freed() || c.n.Load() != 0 {
		t.Fatal("a loan with no engine was ended by nothing")
	}
	loan.End() // the engine's start failed; Started was never called
	if !s.Freed() || c.n.Load() != 1 {
		t.Fatalf("freed %v, released %d times", s.Freed(), c.n.Load())
	}
}

var procGetProcessIoCounters = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessIoCounters")

type ioCounters struct {
	ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes uint64
}

func writtenBytes(t *testing.T) uint64 {
	t.Helper()
	var c ioCounters
	if r, _, err := procGetProcessIoCounters.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c))); r == 0 {
		t.Fatal(err)
	}
	return c.WriteBytes
}

var procGetMappedFileName = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetMappedFileNameW")

// mappedFileName maps the section through a loan and asks Windows which file
// backs the view; it answers the call's error, nil when a file is named.
func mappedFileName(t *testing.T, s *Section) error {
	t.Helper()
	loan, err := s.Lend()
	if err != nil {
		t.Fatal(err)
	}
	defer loan.End()
	addr, err := windows.MapViewOfFile(windows.Handle(loan.Handle()), windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.UnmapViewOfFile(addr)
	var name [windows.MAX_LONG_PATH]uint16
	r, _, err := procGetMappedFileName.Call(uintptr(windows.CurrentProcess()), addr, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if r != 0 {
		t.Logf("the view is backed by %s", windows.UTF16ToString(name[:r]))
		return nil
	}
	return err
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

// TestFillWritesNoFile: filling from a STORED and a DEFLATE member creates
// nothing in the temporary folder or beside the container, and the process
// writes far fewer bytes than the object holds (INF-MS1).
func TestFillWritesNoFile(t *testing.T) {
	p := writePack(t)
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	if os.TempDir() != temp {
		t.Fatalf("temporary folder %s, want %s", os.TempDir(), temp)
	}
	before := entries(t, p.dir)
	written := writtenBytes(t)
	for _, src := range []Extent{p.aligned, p.deflated} {
		s, err := Fill(context.Background(), src, digestOf(p.data), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Writes through a mapped view of a file-backed section are no
		// process I/O; the mapping itself names its backing. A pagefile
		// section has no file, and only that answers ERROR_FILE_INVALID.
		if err := mappedFileName(t, s); !errors.Is(err, windows.ERROR_FILE_INVALID) {
			t.Fatalf("the section's view names a file: %v", err)
		}
		s.Close()
	}
	if delta := writtenBytes(t) - written; delta > payloadSize/16 {
		t.Fatalf("the fills wrote %d bytes for a %d-byte object", delta, payloadSize)
	}
	if got := entries(t, temp); len(got) != 0 {
		t.Fatalf("the temporary folder holds %v", got)
	}
	if got := entries(t, p.dir); !slices.Equal(got, before) {
		t.Fatalf("the container's folder holds %v, before %v", got, before)
	}
}

// childReport is what the engine stand-in prints.
type childReport struct {
	Digest string            `json:"digest"`
	Error  string            `json:"error,omitempty"`
	Probes map[string]string `json:"probes,omitempty"`
}

func runChild(spec string) int {
	args := map[string]string{}
	for _, kv := range strings.Split(spec, ",") {
		k, v, _ := strings.Cut(kv, "=")
		args[k] = v
	}
	value, _ := strconv.ParseUint(args["handle"], 10, 64)
	size, _ := strconv.ParseInt(args["size"], 10, 64)
	h := windows.Handle(value)
	var r childReport
	addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		r.Error = "MapViewOfFile: " + err.Error()
	} else {
		r.Digest = digestOf(viewBytes(addr, size))
		if args["probe"] == "1" {
			r.Probes = probe(h, addr)
		}
		windows.UnmapViewOfFile(addr)
	}
	windows.CloseHandle(h)
	out, _ := json.Marshal(r)
	fmt.Println(string(out))
	if args["wait"] == "1" {
		io.Copy(io.Discard, os.Stdin)
	}
	return 0
}

func outcome(err error) string {
	if err != nil {
		return "denied"
	}
	return "allowed"
}

// probe tries every way the measurement in MEMORY-LENDING.md §6 found for an
// engine of the same account to write through a read-only loan.
func probe(h windows.Handle, addr uintptr) map[string]string {
	p := map[string]string{}
	cur := windows.CurrentProcess()
	w, err := windows.MapViewOfFile(h, windows.FILE_MAP_WRITE, 0, 0, 4096)
	p["map_write"] = outcome(err)
	if err == nil {
		windows.UnmapViewOfFile(w)
	}
	var old uint32
	p["protect_readwrite"] = outcome(windows.VirtualProtect(addr, 4096, windows.PAGE_READWRITE, &old))
	var dac windows.Handle
	err = windows.DuplicateHandle(cur, h, cur, &dac, windows.WRITE_DAC, false, 0)
	p["duplicate_write_dac"] = outcome(err)
	if err == nil {
		windows.CloseHandle(dac)
	}
	var rw windows.Handle
	err = windows.DuplicateHandle(cur, h, cur, &rw, sectionMapRead|0x0002, false, 0)
	p["duplicate_map_write"] = outcome(err)
	if err == nil {
		w, err := windows.MapViewOfFile(rw, windows.FILE_MAP_WRITE, 0, 0, 4096)
		p["map_write_after_duplicate"] = outcome(err)
		if err == nil {
			windows.UnmapViewOfFile(w)
		}
		windows.CloseHandle(rw)
	}
	return p
}

// engine starts the stand-in with the loan's handle as its only extra
// inherited handle, and closes exited once it has exited.
type engine struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *json.Decoder
	exited chan struct{}
}

func startEngine(t *testing.T, loan *Loan, length int64, flags string) *engine {
	t.Helper()
	spec := fmt.Sprintf("handle=%d,size=%d%s", loan.Handle(), length, flags)
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "OA_MODELFILES_CHILD="+spec)
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(loan.Handle())}}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	loan.Started()
	e := &engine{cmd: cmd, stdin: stdin, out: json.NewDecoder(stdout), exited: make(chan struct{})}
	return e
}

func (e *engine) report(t *testing.T) childReport {
	t.Helper()
	var r childReport
	if err := e.out.Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// stop lets the stand-in exit and closes exited after Wait.
func (e *engine) stop(t *testing.T) {
	t.Helper()
	e.stdin.Close()
	// The stand-in prints one report, which report already read.
	if err := e.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	close(e.exited)
}

func waitFreed(t *testing.T, s *Section, c *counter) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !s.Freed() || c.n.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("freed %v, released %d times", s.Freed(), c.n.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEngineCannotRaiseItsLoan: an engine process of the same account holding
// the read-only loan reads the object and cannot map, protect, duplicate or
// re-ACL its way to write access under the protected descriptor. The default
// descriptor, measured as the control, lets the same engine duplicate its
// handle to write access (INF-MS3).
func TestEngineCannotRaiseItsLoan(t *testing.T) {
	p := writePack(t)
	for _, protected := range []bool{true, false} {
		if !protected {
			saved := sectionSDDL
			sectionSDDL = ""
			t.Cleanup(func() { sectionSDDL = saved })
		}
		s, err := Fill(context.Background(), p.deflated, digestOf(p.data), nil)
		if err != nil {
			t.Fatal(err)
		}
		loan, err := s.Lend()
		if err != nil {
			t.Fatal(err)
		}
		e := startEngine(t, loan, s.Length(), ",probe=1")
		loan.EndWhen(e.exited)
		r := e.report(t)
		e.stop(t)
		if r.Error != "" || r.Digest != s.Digest() {
			t.Fatalf("protected %v: %+v", protected, r)
		}
		for _, k := range []string{"map_write", "protect_readwrite"} {
			if r.Probes[k] != "denied" {
				t.Fatalf("protected %v: %s %s", protected, k, r.Probes[k])
			}
		}
		raised := r.Probes["duplicate_map_write"] == "allowed" || r.Probes["duplicate_write_dac"] == "allowed"
		if raised == protected {
			t.Fatalf("protected %v: probes %v", protected, r.Probes)
		}
		s.Close()
	}
}

// TestLoanEndsWhenTheEngineExits: two engines share one section; it stays
// while either runs and is freed, with its holding released once, after the
// second exits (INF-MS4, INF-MS5).
func TestLoanEndsWhenTheEngineExits(t *testing.T) {
	p := writePack(t)
	var c counter
	s, err := Fill(context.Background(), p.aligned, digestOf(p.data), c.release)
	if err != nil {
		t.Fatal(err)
	}
	var engines []*engine
	for range 2 {
		loan, err := s.Lend()
		if err != nil {
			t.Fatal(err)
		}
		e := startEngine(t, loan, s.Length(), ",wait=1")
		loan.EndWhen(e.exited)
		if r := e.report(t); r.Digest != s.Digest() {
			t.Fatalf("engine read %+v", r)
		}
		engines = append(engines, e)
	}
	engines[0].stop(t)
	time.Sleep(50 * time.Millisecond)
	if s.Freed() || c.n.Load() != 0 {
		t.Fatal("the section was freed while an engine still held it")
	}
	engines[1].stop(t)
	waitFreed(t, s, &c)
	if _, err := s.Lend(); !errors.Is(err, ErrReleased) {
		t.Fatalf("a freed section lent: %v", err)
	}
}
