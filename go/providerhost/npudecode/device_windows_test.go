package npudecode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
	"github.com/openabstractions/abstraction-identity/go/trustedpath"
	"github.com/openabstractions/abstraction-inference/go/providerhost/modelfiles"
	"github.com/openabstractions/abstraction-inference/go/providerhost/modelpack"
)

// trustedBase is a folder whose own chain passes ID-D2 with kitOwners: a
// private folder made in the temporary folder or, when that one's chain
// grants others change rights (as a sandbox group's grant on %TEMP% does),
// in the home folder; removed after the test. It skips the test when
// neither passes.
func trustedBase(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	for _, parent := range []string{os.TempDir(), home} {
		if parent == "" {
			continue
		}
		dir := filepath.Join(parent, fmt.Sprintf("oa-npudecode-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
		if err := privatedir.Make(dir); err != nil {
			continue
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		probe := filepath.Join(dir, "probe")
		if err := os.WriteFile(probe, nil, 0o600); err == nil && trustedpath.CheckFile(probe, kitOwners()) == nil {
			os.Remove(probe)
			return dir
		}
	}
	t.Skip("no folder here whose chain passes ID-D2 for this account")
	return ""
}

// privateKit makes a private folder holding four stand-in kit files and
// their manifest under trustedBase; the shim is no DLL, so nothing in it
// can load.
func privateKit(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	dir := filepath.Join(trustedBase(t), "kit")
	if err := privatedir.Make(dir); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{ShimFile: []byte("not a DLL"), XclbinFile: []byte("xclbin"), InstsFile: []byte("inst"),
		ShapeFile: []byte(`{"format":"oa-npu-fixed3","version":1,"kernel":"MLIR_AIE","units":64,"slot":397376,"unit_bytes":524288,"columns":8}`)}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeKitManifest(t, dir, files)
	return dir, files
}

// INF-MP10: a kit whose files identity's ID-D2 refuses (a missing file, a
// write right granted to another principal on the shim or on kit.json, a
// junction on the path), whose manifest is missing or pins other bytes, is
// refused before any library loads; XRT configuration of the process's own
// is refused first.
func TestKitRefusedBeforeLoading(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, Config{Dir: t.TempDir()}); !errors.Is(err, ErrKit) || !errors.Is(err, trustedpath.ErrUntrusted) {
		t.Fatalf("an empty folder: %v", err)
	}
	if _, err := Query(Config{Dir: t.TempDir()}); !errors.Is(err, trustedpath.ErrUntrusted) {
		t.Fatalf("Query, an empty folder: %v", err)
	}
	// A kit file another principal may modify, the shim and the manifest
	// alike, is refused by ID-D2 though its bytes still match.
	for _, name := range []string{ShimFile, ManifestFile} {
		wide, _ := privateKit(t)
		if out, err := exec.Command("icacls", filepath.Join(wide, name), "/grant", "*S-1-1-0:(M)").CombinedOutput(); err != nil {
			t.Fatalf("icacls: %v %s", err, out)
		}
		_, err := Open(ctx, Config{Dir: wide})
		if !errors.Is(err, ErrKit) || !errors.Is(err, trustedpath.ErrUntrusted) || !strings.Contains(err.Error(), name+": ") || strings.Contains(err.Error(), ": folder ") {
			t.Fatalf("Everyone may modify %s: %v", name, err)
		}
	}
	dir, files := privateKit(t)
	// The same kit reached through a junction beside it.
	link := filepath.Join(filepath.Dir(dir), "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, dir).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	if _, err := Open(ctx, Config{Dir: link}); !errors.Is(err, ErrKit) || !errors.Is(err, trustedpath.ErrUntrusted) || !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("a kit through a junction: %v", err)
	}
	// The pinned stand-in shim passes the manifest and fails to load.
	if _, err := Open(ctx, Config{Dir: dir}); err == nil || errors.Is(err, ErrKit) || !strings.Contains(err.Error(), "load") {
		t.Fatalf("a stand-in shim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ShimFile), []byte("another DLL"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, Config{Dir: dir}); !errors.Is(err, ErrKit) || !strings.Contains(err.Error(), ShimFile) {
		t.Fatalf("a shim the manifest does not pin: %v", err)
	}
	writeKitManifest(t, dir, map[string][]byte{ShimFile: []byte("another DLL"), XclbinFile: files[XclbinFile], InstsFile: files[InstsFile], ShapeFile: files[ShapeFile]})
	if _, err := Open(ctx, Config{Dir: dir, ManifestSHA256: strings.Repeat("ab", 32)}); !errors.Is(err, ErrKit) {
		t.Fatalf("a manifest of another pin: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Query(Config{Dir: dir}); !errors.Is(err, ErrKit) {
		t.Fatalf("no manifest: %v", err)
	}
	t.Setenv("XRT_INI_PATH", filepath.Join(dir, "xrt.ini"))
	if _, err := Open(ctx, Config{Dir: dir}); !errors.Is(err, ErrXRTConfig) {
		t.Fatalf("XRT_INI_PATH set: %v", err)
	}
	if _, err := Query(Config{Dir: dir}); !errors.Is(err, ErrXRTConfig) {
		t.Fatalf("Query, XRT_INI_PATH set: %v", err)
	}
}

// INF-MP10: Close during a fill waits until the fill gives the device back,
// then frees it once; no fill can claim it afterwards.
func TestCloseWaitsForTheFill(t *testing.T) {
	var freed atomic.Int32
	d := newDevice(Shape{Units: 1}, 0, "fake", func() { freed.Add(1) })
	if !d.TryAcquire() {
		t.Fatal("a new device is busy")
	}
	done := make(chan error)
	go func() { done <- d.Close() }()
	select {
	case <-done:
		t.Fatal("Close returned while a fill held the device")
	case <-time.After(100 * time.Millisecond):
	}
	if freed.Load() != 0 {
		t.Fatal("Close freed the device under a fill")
	}
	d.Done()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if freed.Load() != 1 || d.TryAcquire() {
		t.Fatalf("freed %d times; claimable after Close", freed.Load())
	}
	if err := d.Close(); err != nil || freed.Load() != 1 {
		t.Fatalf("a second Close: %v, freed %d times", err, freed.Load())
	}
	if err := d.Decode(context.Background(), 0); err == nil {
		t.Fatal("a closed device decoded")
	}
}

// npuKit is the kit OA_NPU_KIT names, after checking that XRT finds an
// NPU. The NPU test runs only when that variable is set: each NPU run is
// asked for.
func npuKit(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("OA_NPU_KIT")
	if dir == "" {
		t.Skip("OA_NPU_KIT names no NPU kit; the NPU test runs only when asked")
	}
	if _, err := os.Stat(filepath.Join(dir, ShimFile)); err != nil {
		t.Skipf("no shim in the kit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("SystemRoot"), "System32", "xrt_coreutil.dll")); err != nil {
		t.Skipf("no XRT runtime from an NPU driver: %v", err)
	}
	name, err := Query(Config{Dir: dir})
	if err != nil {
		t.Skipf("the kit does not load, or XRT finds no NPU: %v", err)
	}
	t.Logf("NPU: %s", name)
	return dir
}

// bf16Object is a header and 40 MiB of BF16 weights with NaNs, so units
// cross 16 MiB spans and fill more than one batch.
func bf16Object() ([]byte, []modelpack.Tensor) {
	r := rand.New(rand.NewPCG(5, 6))
	header := make([]byte, 1001)
	for i := range header {
		header[i] = byte(r.Uint32())
	}
	vals := 20 << 20
	bf := make([]byte, 2*vals)
	for i := range vals {
		sd := 0.02
		if r.IntN(64) == 0 {
			sd = 0.3
		}
		b := math.Float32bits(float32(r.NormFloat64() * sd))
		v := uint16(b >> 16)
		if i%997 == 0 {
			v = 0x7fc1
		}
		binary.LittleEndian.PutUint16(bf[2*i:], v)
	}
	return append(header, bf...), []modelpack.Tensor{{Offset: int64(len(header)), Length: int64(len(bf)), DType: "BF16"}}
}

// INF-MP10, with an NPU: Open's self-test passes, and fills from an archive
// of fixed-code units through the NPU are proven, first fill and under the
// record, with every unit decoded on the NPU.
func TestNPUFill(t *testing.T) {
	dir := npuKit(t)
	ctx := context.Background()
	dev, err := Open(ctx, Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	src, tensors := bf16Object()
	var b bytes.Buffer
	archive, err := modelpack.WriteZip(&b, []modelpack.ArchiveSource{{Path: "model.safetensors", Data: src, WeightFormat: "safetensors", Tensors: tensors}},
		modelpack.Rates{BF16Fixed3: 20e9, Disk: 6.5e9}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixed := 0
	for _, c := range archive.Files[0].Index.Chunks {
		if c.Encoding == modelpack.EncodingExpFixed3V1 {
			fixed++
		}
	}
	path := filepath.Join(t.TempDir(), "model.oa.zip")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := modelfiles.OpenSpanRecords(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	for _, fill := range []string{"first fill", "fill under the record"} {
		var r modelpack.FillReport
		s, err := modelpack.FillZipWith(ctx, recs, path, digest, modelpack.FillOptions{Workers: 4, Device: dev, Report: &r})
		if err != nil {
			t.Fatalf("%s: %v", fill, err)
		}
		s.Close()
		if r.DeviceUnits != fixed || r.CPUUnits != 0 || r.Retried || r.DeviceErr != nil || fixed == 0 {
			t.Fatalf("%s: report %+v for %d fixed-code units", fill, r, fixed)
		}
	}
	runs, busy := dev.Stats()
	t.Logf("%d NPU runs, %v from start to completion", runs, busy)
}
