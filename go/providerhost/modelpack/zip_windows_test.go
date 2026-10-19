package modelpack

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
	"golang.org/x/sys/windows"
)

// fixture is one model on disk: the object as a loose file, its pack, and
// a record folder.
type fixture struct {
	dir, file, pack string
	src             []byte
	digest          string
	index           Index
}

func digestOf(b []byte) string {
	d := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(d[:])
}

func records(t *testing.T) *modelfiles.SpanRecords {
	t.Helper()
	r, err := modelfiles.OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newFixture packs object() into a pack file; recs, when not nil, receives
// the packer's record.
func newFixture(t *testing.T, recs *modelfiles.SpanRecords) fixture {
	t.Helper()
	src, tensors := object()
	dir := t.TempDir()
	f := fixture{dir: dir, file: filepath.Join(dir, "model.safetensors"), pack: filepath.Join(dir, "model.oapack.zip"),
		src: src, digest: digestOf(src)}
	if err := os.WriteFile(f.file, src, 0o600); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	archive, err := WriteZip(&b, []ArchiveSource{{Path: "model.safetensors", Data: src, WeightFormat: "safetensors", Tensors: tensors}}, fast, recs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.pack, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f.index = archive.Files[0].Index
	return f
}

type releases struct{ n atomic.Int32 }

func (r *releases) release() { r.n.Add(1) }

// sectionBytes reads a section through a read-only loan in this process.
func sectionBytes(t *testing.T, s *modelfiles.Section) []byte {
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
	p := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return bytes.Clone(unsafe.Slice((*byte)(p), s.Length()))
}

// INF-MP6, INF-MP7: file → pack → section gives the file's digest, its chunk
// digests and its bytes. The pack is one zip layer of two STORED entries,
// the units starting on a 64 KiB boundary.
func TestPackZipRoundTrip(t *testing.T) {
	recs := records(t)
	f := newFixture(t, recs)
	plain, err := modelfiles.Fill(context.Background(), modelfiles.Extent{Path: f.file, Packed: int64(len(f.src)),
		Length: int64(len(f.src)), Method: zip.Store}, f.digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()

	zr, err := zip.OpenReader(f.pack)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range zr.File {
		off, _ := e.DataOffset()
		names = append(names, e.Name)
		if e.Method != zip.Store || (e.Name != archiveIndexEntry && off%(64<<10) != 0) {
			t.Errorf("%s: method %d at %d", e.Name, e.Method, off)
		}
	}
	zr.Close()
	if !slices.Equal(names, []string{archiveIndexEntry, "model.safetensors" + unitsSuffix}) {
		t.Fatalf("entries %v", names)
	}
	archive, err := ReadZip(f.pack)
	if err != nil {
		t.Fatal(err)
	}
	a := archive.Files[0]
	if a.Index.Digest != f.digest || a.Path != "model.safetensors" || a.Extent.Offset%(64<<10) != 0 || a.Extent.Length != int64(len(f.src)) {
		t.Fatalf("ReadZip: %+v", a)
	}

	var rel releases
	s, err := FillZip(context.Background(), recs, f.pack, f.digest, 4, rel.release)
	if err != nil {
		t.Fatal(err)
	}
	if s.Digest() != plain.Digest() || !slices.Equal(s.ChunkDigests(), plain.ChunkDigests()) {
		t.Fatalf("pack fill %s %v, file fill %s %v", s.Digest(), s.ChunkDigests(), plain.Digest(), plain.ChunkDigests())
	}
	if !bytes.Equal(sectionBytes(t, s), f.src) {
		t.Fatal("the section's bytes differ from the file's")
	}
	s.Close()
	if rel.n.Load() != 1 {
		t.Fatalf("release called %d times", rel.n.Load())
	}
}

// INF-MP7: a fill from fixed-code units, whose escapes FillDecoded's hash
// pass applies, gives the object's bytes and spans on a first fill, which
// records them, and on a fill under that record.
func TestFillZipAppliesEscapesInTheHashPass(t *testing.T) {
	src, tensors := fusedObject()
	var b bytes.Buffer
	archive, err := WriteZip(&b, []ArchiveSource{{Path: "model.safetensors", Data: src, WeightFormat: "safetensors", Tensors: tensors}}, fixedOnly, nil)
	if err != nil {
		t.Fatal(err)
	}
	if encodings(archive.Files[0].Index)[EncodingExpFixed3V1] == 0 {
		t.Fatal("no unit took the fixed code")
	}
	path := filepath.Join(t.TempDir(), "model.oa.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recs := records(t)
	for _, fill := range []string{"first fill", "fill under the record"} {
		s, err := FillZip(context.Background(), recs, path, digestOf(src), 4, nil)
		if err != nil {
			t.Fatalf("%s: %v", fill, err)
		}
		if !bytes.Equal(sectionBytes(t, s), src) || !slices.Equal(s.ChunkDigests(), archive.Files[0].Index.Spans) {
			t.Fatalf("%s: the section differs from the object", fill)
		}
		s.Close()
		if _, ok := recs.Lookup(digestOf(src)); !ok {
			t.Fatalf("%s left no record", fill)
		}
	}
}

// rewrite copies an archive with its file entries' bytes unchanged and the
// first file's index edited, laid out again as the writer lays it out, as
// someone who can write the archive file can.
func rewrite(t *testing.T, from, to string, edit func(*Index)) {
	t.Helper()
	zr, err := zip.OpenReader(from)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var archive ModelArchive
	rc, err := zr.File[0].Open()
	if err != nil || zr.File[0].Name != archiveIndexEntry {
		t.Fatalf("the index is not the first entry: %v", err)
	}
	if err := json.NewDecoder(rc).Decode(&archive); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	edit(&archive.Files[0].Index)
	body, _ := json.Marshal(archive)
	var b bytes.Buffer
	z := &zipWriter{w: &b}
	if err := z.begin(archiveIndexEntry, int64(len(body)), crc32.ChecksumIEEE(body), false); err != nil {
		t.Fatal(err)
	}
	z.Write(body)
	for _, e := range zr.File[1:] {
		raw, err := e.OpenRaw()
		if err != nil {
			t.Fatal(err)
		}
		if err := z.begin(e.Name, int64(e.CompressedSize64), e.CRC32, true); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(z, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// forged packs a changed object and rewrites its index to claim the genuine
// object's digest, keeping the spans that match the changed bytes: every
// check inside the pack passes.
func forged(t *testing.T, f fixture) string {
	t.Helper()
	src, tensors := object()
	src[5] ^= 0xff
	var b bytes.Buffer
	if _, err := WriteZip(&b, []ArchiveSource{{Path: "model.safetensors", Data: src, WeightFormat: "safetensors", Tensors: tensors}}, fast, nil); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(f.dir, "changed.zip")
	if err := os.WriteFile(changed, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.dir, "forged.zip")
	rewrite(t, changed, path, func(x *Index) { x.Digest = f.digest })
	return path
}

// INF-MP1, INF-MP7: once OA holds the object's record, a pack whose index
// claims the object with spans of other bytes is refused by the record.
func TestForgedPackRefusedByTheRecord(t *testing.T) {
	recs := records(t)
	f := newFixture(t, recs)
	path := forged(t, f)
	var rel releases
	_, err := FillZip(context.Background(), recs, path, f.digest, 4, rel.release)
	if !errors.Is(err, modelfiles.ErrDigest) || !strings.Contains(err.Error(), "span") {
		t.Fatalf("forged pack with the record: %v, want a span digest refusal", err)
	}
	if rel.n.Load() != 1 {
		t.Fatalf("release called %d times", rel.n.Load())
	}
	// The record stays the genuine object's.
	if rec, ok := recs.Lookup(f.digest); !ok || !slices.Equal(rec.Spans, f.index.Spans) {
		t.Fatal("the refused fill changed the record")
	}
}

// INF-MP1, INF-MS8: the first fill of a pack OA has no record of checks
// what the pack claims, hashes the whole object, and records it. A forged
// pack passes its own spans and is refused by the whole hash.
func TestFirstFillHashesTheWholeObject(t *testing.T) {
	f := newFixture(t, nil)
	recs := records(t)
	path := forged(t, f)
	_, err := FillZip(context.Background(), recs, path, f.digest, 4, nil)
	if !errors.Is(err, modelfiles.ErrDigest) || !strings.Contains(err.Error(), "decodes to") {
		t.Fatalf("forged pack, no record: %v, want the whole-object refusal", err)
	}
	if _, ok := recs.Lookup(f.digest); ok {
		t.Fatal("a refused first fill left a record")
	}
	s, err := FillZip(context.Background(), recs, f.pack, f.digest, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	rec, ok := recs.Lookup(f.digest)
	if !ok || !slices.Equal(rec.Spans, f.index.Spans) || rec.Length != int64(len(f.src)) {
		t.Fatalf("after the first fill: record %+v (%v)", rec, ok)
	}
	// Later fills trust the record: a record naming other spans now refuses
	// the genuine pack, which only a fill reading the record can do.
	rec.Spans = slices.Clone(rec.Spans)
	rec.Spans[0] = strings.Repeat("0", 64)
	if err := recs.Put(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := FillZip(context.Background(), recs, f.pack, f.digest, 4, nil); !errors.Is(err, modelfiles.ErrDigest) {
		t.Fatalf("genuine pack against another record: %v", err)
	}
}

// corruptUnits breaks the header of every encoded unit in the pack file, so
// decoding any of them answers ErrCorrupt.
func corruptUnits(t *testing.T, f fixture) {
	t.Helper()
	b, err := os.ReadFile(f.pack)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := ReadZip(f.pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.index.Chunks {
		if c.Encoding == EncodingExpHuffmanV1 {
			b[archive.Files[0].Extent.Offset+c.PackedOffset] = 0xee // the format version
		}
	}
	if err := os.WriteFile(f.pack, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// INF-MP7: an index bound to another object, and an index that does not
// tile its object, are refused before a section exists or a unit is
// decoded: with every encoded unit broken, the refusal names the binding
// or the index, never a unit.
func TestFillZipRefusesBeforeDecoding(t *testing.T) {
	recs := records(t)
	f := newFixture(t, recs)
	corruptUnits(t, f)
	var rel releases
	_, err := FillZip(context.Background(), recs, f.pack, digestOf([]byte("other")), 4, rel.release)
	if !errors.Is(err, modelfiles.ErrDigest) || errors.Is(err, ErrCorrupt) || rel.n.Load() != 1 {
		t.Fatalf("another digest: %v, release %d; want the binding refusal alone", err, rel.n.Load())
	}
	bad := filepath.Join(f.dir, "bad.zip")
	rewrite(t, f.pack, bad, func(x *Index) { x.Chunks[1].Offset++ })
	_, err = FillZip(context.Background(), recs, bad, f.digest, 4, nil)
	// ReadZip names the pack file in its refusal; a refusal while decoding
	// would not.
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "covers") || !strings.Contains(err.Error(), bad) {
		t.Fatalf("an index with a gap: %v, want ReadZip's index refusal", err)
	}
}

// INF-MP7: a fill from a pack needs OA's span records.
func TestFillZipNeedsRecords(t *testing.T) {
	f := newFixture(t, nil)
	var rel releases
	if s, err := FillZip(context.Background(), nil, f.pack, f.digest, 4, rel.release); err == nil || s != nil || rel.n.Load() != 1 {
		t.Fatalf("no records: section %v, %v, release %d", s, err, rel.n.Load())
	}
}

var procGetMappedFileName = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetMappedFileNameW")

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

// INF-MP7: filling from a pack creates nothing in the temporary folder or
// beside the pack, and the section is backed by no file.
func TestFillZipWritesNoFile(t *testing.T) {
	recs := records(t)
	f := newFixture(t, recs)
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	before := entries(t, f.dir)
	for _, r := range []*modelfiles.SpanRecords{recs, records(t)} { // recorded, then a first fill
		s, err := FillZip(context.Background(), r, f.pack, f.digest, 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		loan, err := s.Lend()
		if err != nil {
			t.Fatal(err)
		}
		addr, err := windows.MapViewOfFile(windows.Handle(loan.Handle()), windows.FILE_MAP_READ, 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var name [windows.MAX_LONG_PATH]uint16
		ret, _, callErr := procGetMappedFileName.Call(uintptr(windows.CurrentProcess()), addr, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		windows.UnmapViewOfFile(addr)
		loan.End()
		s.Close()
		// A pagefile section has no file, and only that answers
		// ERROR_FILE_INVALID.
		if ret != 0 || !errors.Is(callErr, windows.ERROR_FILE_INVALID) {
			t.Fatalf("the section's view names a file: %s, %v", windows.UTF16ToString(name[:ret]), callErr)
		}
	}
	if got := entries(t, temp); len(got) != 0 {
		t.Fatalf("the temporary folder holds %v", got)
	}
	if got := entries(t, f.dir); !slices.Equal(got, before) {
		t.Fatalf("the pack's folder holds %v, before %v", got, before)
	}
}
