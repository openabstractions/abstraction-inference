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
	"hash/crc32"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// archiveSources is a small model folder: a safetensors file whose BF16
// tensor is encoded, a quantised-looking file stored as is, and a config.
func archiveSources() []ArchiveSource {
	st := safetensorsFile(stTensor{"layer.weight", "BF16", weights(400_000, 100, bf16)}, stTensor{"norm", "F32", make([]byte, 64)})
	_, tensors, err := ReadTensors(bytes.NewReader(st), int64(len(st)))
	if err != nil {
		panic(err)
	}
	return []ArchiveSource{
		{Path: "model.safetensors", Data: st, WeightFormat: "safetensors", Tensors: tensors},
		{Path: "sub/quant.gguf", Data: randomBytes(300_000, 101)},
		{Path: "config.json", Data: []byte(`{"architectures":["X"]}`)},
	}
}

func writeArchive(t *testing.T, sources []ArchiveSource, zip64 bool) (string, ModelArchive) {
	t.Helper()
	defer func(f bool) { forceZip64 = f }(forceZip64)
	forceZip64 = zip64
	var b bytes.Buffer
	archive, err := WriteZip(&b, sources, Rates{BF16: 7e9, BF16Fixed3: 20e9, F16: 15e9, Disk: 6.5e9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "model.oa.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, archive
}

// INF-MP6: the archive is the index first, then one STORED, 64 KiB-aligned
// entry per file; a file whose units all stay stored is its own entry,
// which Go's archive/zip extracts as the file; every file restores byte
// for byte; with Zip64 fields everywhere the same holds.
func TestArchiveLayout(t *testing.T) {
	sources := archiveSources()
	for _, zip64 := range []bool{false, true} {
		path, _ := writeArchive(t, sources, zip64)
		zr, err := zip.OpenReader(path)
		if err != nil {
			t.Fatalf("zip64=%v: %v", zip64, err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
			off, err := f.DataOffset()
			if err != nil || f.Method != zip.Store || (f.Name != archiveIndexEntry && off%packAlign != 0) {
				t.Errorf("zip64=%v: %s at %d, method %d: %v", zip64, f.Name, off, f.Method, err)
			}
			for _, s := range sources {
				if f.Name == s.Path {
					rc, _ := f.Open() // a stored file is the file
					got, err := io.ReadAll(rc)
					rc.Close()
					if err != nil || !bytes.Equal(got, s.Data) {
						t.Errorf("zip64=%v: archive/zip extracts %s as other bytes: %v", zip64, s.Path, err)
					}
				}
			}
		}
		zr.Close()
		want := []string{archiveIndexEntry, "model.safetensors" + unitsSuffix, "sub/quant.gguf", "config.json"}
		if !slices.Equal(names, want) {
			t.Fatalf("zip64=%v: entries %v, want %v", zip64, names, want)
		}
		archive, err := ReadZip(path)
		if err != nil {
			t.Fatalf("zip64=%v: ReadZip: %v", zip64, err)
		}
		for i, a := range archive.Files {
			var out bytes.Buffer
			if err := RestoreFile(context.Background(), &out, a, 2); err != nil || !bytes.Equal(out.Bytes(), sources[i].Data) {
				t.Fatalf("zip64=%v: %s restores to other bytes: %v", zip64, a.Path, err)
			}
		}
		if len(archive.Files[0].TensorTable) != 2 || archive.Files[0].TensorTable[0].Name == "" || archive.Files[0].TensorTable[0].Normalized == "" {
			t.Fatalf("tensor table %+v", archive.Files[0].TensorTable)
		}
	}
}

// INF-MP6: standard zip tools on this machine list the archive and extract
// every stored file as the file: bsdtar (tar), PowerShell's Expand-Archive,
// 7-Zip and Info-ZIP's unzip, each where present; with and without Zip64.
func TestArchiveOpensInStandardTools(t *testing.T) {
	sources := archiveSources()
	tried := 0
	for _, zip64 := range []bool{false, true} {
		path, _ := writeArchive(t, sources, zip64)
		check := func(tool, dir string) {
			for _, s := range sources[1:] { // the stored files
				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(s.Path)))
				if err != nil || !bytes.Equal(got, s.Data) {
					t.Errorf("%s (zip64=%v) extracts %s as other bytes: %v", tool, zip64, s.Path, err)
				}
			}
			units, err := os.ReadFile(filepath.Join(dir, "model.safetensors"+unitsSuffix))
			if err != nil || len(units) == 0 {
				t.Errorf("%s (zip64=%v) lost the units entry: %v", tool, zip64, err)
			}
		}
		if tar, ok := bsdtar(); ok {
			tried++
			out, err := exec.Command(tar, "-tf", path).CombinedOutput()
			if err != nil || !strings.Contains(string(out), "sub/quant.gguf") || !strings.Contains(string(out), archiveIndexEntry) {
				t.Errorf("tar -tf (zip64=%v): %v\n%s", zip64, err, out)
			}
			dir := t.TempDir()
			if out, err := exec.Command(tar, "-xf", path, "-C", dir).CombinedOutput(); err != nil {
				t.Errorf("tar -xf (zip64=%v): %v\n%s", zip64, err, out)
			} else {
				check("tar", dir)
			}
		}
		if runtime.GOOS == "windows" {
			if ps, err := exec.LookPath("powershell.exe"); err == nil {
				tried++
				dir := t.TempDir()
				// Use the module shipped with this executable; an earlier same-named
				// module in the inherited PSModulePath may not load under policy.
				cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command",
					"Import-Module -Name (Join-Path $PSHOME \"Modules\\Microsoft.PowerShell.Archive\\Microsoft.PowerShell.Archive.psd1\") -ErrorAction Stop\n"+
						"Expand-Archive -LiteralPath $env:OA_ARCHIVE -DestinationPath $env:OA_DEST -Force")
				cmd.Env = append(os.Environ(), "OA_ARCHIVE="+path, "OA_DEST="+dir)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("Expand-Archive (zip64=%v): %v\n%s", zip64, err, out)
				} else {
					check("Expand-Archive", dir)
				}
			}
		}
		for _, tool := range []struct{ name, list string }{{"7z", "l"}, {"unzip", "-l"}} {
			if bin, err := exec.LookPath(tool.name); err == nil {
				tried++
				if out, err := exec.Command(bin, tool.list, path).CombinedOutput(); err != nil || !strings.Contains(string(out), "config.json") {
					t.Errorf("%s (zip64=%v): %v\n%s", tool.name, zip64, err, out)
				}
			}
		}
	}
	if tried == 0 {
		t.Skip("no standard zip tool on this machine")
	}
	t.Logf("%d tool runs", tried)
}

// rawEntry is one zip entry of an archive a test assembles.
type rawEntry struct {
	name string
	data []byte
}

// archiveParts reads an archive's index and its file entries' bytes.
func archiveParts(t *testing.T, path string) (ModelArchive, []rawEntry) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var archive ModelArchive
	var entries []rawEntry
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == archiveIndexEntry {
			if err := json.Unmarshal(b, &archive); err != nil {
				t.Fatal(err)
			}
			continue
		}
		entries = append(entries, rawEntry{f.Name, b})
	}
	return archive, entries
}

// assemble writes an archive of index and entries, laid out as the writer
// lays one out, as someone who can write the archive file can.
func assemble(t *testing.T, archive ModelArchive, entries []rawEntry) string {
	t.Helper()
	body, err := json.Marshal(archive)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	z := &zipWriter{w: &b}
	if err := z.begin(archiveIndexEntry, int64(len(body)), crc32.ChecksumIEEE(body), false); err != nil {
		t.Fatal(err)
	}
	z.Write(body)
	for _, e := range entries {
		if err := z.begin(e.name, int64(len(e.data)), crc32.ChecksumIEEE(e.data), true); err != nil {
			t.Fatal(err)
		}
		z.Write(e.data)
	}
	if err := z.close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "edited.oa.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// INF-MP6: archive paths are relative paths every platform extracts as
// written; WriteZip refuses to write any other, and ReadZip refuses an
// index naming one.
func TestArchivePaths(t *testing.T) {
	bad := []string{"/abs", "../up", "a/../b", "a//b", `a\b`, "c:x", "./a", archiveIndexEntry,
		"CON", "con.json", "sub/Nul", "aux.tar.gz", "PRN", "COM0", "com9.bin", "LPT1", "lpt9.x",
		"CONIN$", "conout$.txt", "COM¹", "lpt².bin", "com³",
		"dir./a", "a.", "a ", "sub /a", "a<b", "a>b", `a"b`, "a|b", "a?b", "a*b", "a\x01b", "a\tb", "a\x7fb"}
	for _, p := range append([]string{""}, bad...) {
		if _, err := WriteZip(io.Discard, []ArchiveSource{{Path: p, Data: []byte("x")}}, Rates{}, nil); !errors.Is(err, ErrCorrupt) {
			t.Errorf("WriteZip, path %q: %v", p, err)
		}
	}
	for _, p := range []string{"console.json", "COM10", "LPTX", "comma", "auxiliary/x", "a.b/c d", "nul_x", "CONIN", "conin$x", "COM⁴", "LPT¹¹"} {
		if err := checkArchivePath(p); err != nil {
			t.Errorf("path %q refused: %v", p, err)
		}
	}
	if _, err := WriteZip(io.Discard, []ArchiveSource{{Path: "A.json", Data: []byte("x")}, {Path: "a.json", Data: []byte("y")}}, Rates{}, nil); !errors.Is(err, ErrCorrupt) {
		t.Errorf("two paths that differ in case only: %v", err)
	}
	source, _ := writeArchive(t, archiveSources(), false)
	archive, entries := archiveParts(t, source)
	if _, err := ReadZip(assemble(t, archive, entries)); err != nil {
		t.Fatalf("the reassembled archive: %v", err)
	}
	// config.json, the third file, is stored under its own path.
	for _, p := range bad {
		if p == archiveIndexEntry {
			continue
		}
		edited := archive
		edited.Files = slices.Clone(archive.Files)
		edited.Files[2].Path, edited.Files[2].Entry = p, p
		moved := slices.Clone(entries)
		moved[2].name = p
		if _, err := ReadZip(assemble(t, edited, moved)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("ReadZip, path %q: %v", p, err)
		}
	}
}

// INF-MP6: ReadZip refuses two zip entries whose names differ in letter
// case alone, two index paths or entries that do, and a zip entry the
// index does not name.
func TestArchiveNamesUnique(t *testing.T) {
	source, _ := writeArchive(t, archiveSources(), false)
	archive, entries := archiveParts(t, source)
	refused := func(what, want string, a ModelArchive, e []rawEntry) {
		t.Helper()
		_, err := ReadZip(assemble(t, a, e))
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want a refusal naming %q", what, err, want)
		}
	}
	refused("an unnamed entry", "not in the index", archive, append(slices.Clone(entries), rawEntry{"extra.bin", []byte("x")}))
	refused("an unnamed entry named like an encoded file's path", "not in the index", archive,
		append(slices.Clone(entries), rawEntry{"model.safetensors", []byte("x")}))
	refused("entries differing in case", "letter case", archive, append(slices.Clone(entries), rawEntry{"CONFIG.JSON", []byte("x")}))

	// A second file whose path differs from the encoded file's path in case
	// alone, held in its own, distinct entry.
	twin := archive
	twin.Files = append(slices.Clone(archive.Files), archive.Files[2])
	twin.Files[3].Path, twin.Files[3].Entry = "Model.safetensors", "Model.safetensors"
	refused("paths differing in case", "letter case", twin, append(slices.Clone(entries), rawEntry{"Model.safetensors", entries[2].data}))
}

// INF-MP6: a file entry's data starts on a 64 KiB boundary whatever
// precedes it. Where the padding would carry the local header's extra field
// past 65,535 bytes, zero bytes before the header take the excess, and zip
// readers still find the entry and its bytes; with Zip64 fields too.
func TestZipAlignmentAcrossTheWrap(t *testing.T) {
	data := []byte("aligned entry")
	for _, zip64 := range []bool{false, true} {
		for size := packAlign - 160; size <= packAlign+16; size++ {
			var b bytes.Buffer
			z := &zipWriter{w: &b, forceZip64: zip64}
			first := make([]byte, size)
			first[0], first[size-1] = 1, 2
			if err := z.begin("a", int64(size), crc32.ChecksumIEEE(first), false); err != nil {
				t.Fatal(err)
			}
			z.Write(first)
			if err := z.begin("b", int64(len(data)), crc32.ChecksumIEEE(data), true); err != nil {
				t.Fatal(err)
			}
			z.Write(data)
			if err := z.close(); err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
			if err != nil {
				t.Fatalf("zip64=%v, first entry %d bytes: %v", zip64, size, err)
			}
			for i, want := range [][]byte{first, data} {
				f := zr.File[i]
				off, err := f.DataOffset()
				if err != nil || (i == 1 && off%packAlign != 0) {
					t.Fatalf("zip64=%v, first entry %d bytes: %s at %d: %v", zip64, size, f.Name, off, err)
				}
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("zip64=%v, first entry %d bytes: %s reads as other bytes: %v", zip64, size, f.Name, err)
				}
			}
		}
	}
}

// INF-MP6: the same values held as BF16 and as F16 have the same
// dtype-normalised digest and different byte digests.
func TestNormalizedDigest(t *testing.T) {
	vals := []float32{0, 1, -2, 0.5, 0.25, 3, -0.125, 64}
	var b, f []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint16(b, bf16(v))
		f = binary.LittleEndian.AppendUint16(f, f16(v))
	}
	got := tensorTable(slices.Concat(b, f), []Tensor{{Offset: 0, Length: int64(len(b)), DType: "BF16"}, {Offset: int64(len(b)), Length: int64(len(f)), DType: "F16"}})
	if got[0].Normalized == "" || got[0].Normalized != got[1].Normalized || got[0].Digest == got[1].Digest {
		t.Fatalf("BF16 %+v, F16 %+v", got[0], got[1])
	}
	var f32 []byte
	for _, v := range vals {
		f32 = binary.LittleEndian.AppendUint32(f32, math.Float32bits(v))
	}
	if sum := sha256.Sum256(f32); got[0].Normalized != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("the normalised digest is not the SHA-256 of the F32 values")
	}
	// Bit for bit: -0 and +0 differ, and a signalling NaN's payload widens
	// unquieted.
	signed := tensorTable(values16(0x0000, 0x8000), []Tensor{{Offset: 0, Length: 2, DType: "BF16"}, {Offset: 2, Length: 2, DType: "BF16"}})
	if signed[0].Normalized == signed[1].Normalized {
		t.Fatal("-0 and +0 share a normalised digest")
	}
	for _, c := range []struct {
		dtype string
		in    uint16
		want  uint32
	}{{"F16", 0x7c01, 0x7f802000}, {"F16", 0xfe00, 0xffc00000}, {"BF16", 0x7f81, 0x7f810000}} {
		if got := widener(c.dtype)(c.in); got != c.want {
			t.Errorf("%s %#04x widens to %#08x, want %#08x", c.dtype, c.in, got, c.want)
		}
	}
}
