package npudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeShape(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ShapeFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// INF-MP10: the kit's fixed3.json must describe modelpack's slot and unit
// sizes; any other design is refused before the shim loads.
func TestReadShape(t *testing.T) {
	good := `{"format":"oa-npu-fixed3","version":1,"kernel":"MLIR_AIE","units":64,"slot":397376,"unit_bytes":524288,"columns":8}`
	s, err := readShape(writeShape(t, good))
	if err != nil || s.Units != 64 || s.Kernel != "MLIR_AIE" {
		t.Fatalf("%+v, %v", s, err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"slot":397376`, `"slot":397312`, 1),
		strings.Replace(good, `"unit_bytes":524288`, `"unit_bytes":262144`, 1),
		strings.Replace(good, `"version":1`, `"version":2`, 1),
		strings.Replace(good, `"units":64`, `"units":0`, 1),
		strings.Replace(good, `"kernel":"MLIR_AIE"`, `"kernel":""`, 1),
		`{`,
	} {
		if _, err := readShape(writeShape(t, bad)); !errors.Is(err, ErrKit) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	if _, err := readShape(t.TempDir()); !errors.Is(err, ErrKit) {
		t.Fatalf("no shape file: %v", err)
	}
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// writeKitManifest writes kit.json pinning files and returns its bytes.
func writeKitManifest(t *testing.T, dir string, files map[string][]byte) []byte {
	t.Helper()
	m := Manifest{Format: "oa-npu-kit", Version: 1, SHA256: map[string]string{}}
	for name, b := range files {
		m.SHA256[name] = hexSum(b)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// INF-MP10: kit.json pins exactly the four kit files; a manifest of another
// form, another pin, or a file of other bytes is refused.
func TestManifest(t *testing.T) {
	files := map[string][]byte{ShimFile: []byte("dll"), XclbinFile: []byte("xclbin"), InstsFile: []byte("insts"), ShapeFile: []byte("{}")}
	dir := t.TempDir()
	raw := writeKitManifest(t, dir, files)
	m, err := readManifest(dir, hexSum(raw))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if err := m.checkDigest(name, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.checkDigest(ShimFile, []byte("other")); !errors.Is(err, ErrKit) {
		t.Fatalf("a changed shim: %v", err)
	}
	if _, err := readManifest(dir, strings.Repeat("0", 64)); !errors.Is(err, ErrKit) {
		t.Fatalf("another pin: %v", err)
	}
	delete(files, InstsFile)
	writeKitManifest(t, dir, files)
	if _, err := readManifest(dir, ""); !errors.Is(err, ErrKit) {
		t.Fatalf("three files pinned: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(`{"format":"oa-npu-kit","version":1,"sha256":{},"extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(dir, ""); !errors.Is(err, ErrKit) {
		t.Fatalf("an unknown field: %v", err)
	}
}

// INF-MP10: XRT reads configuration, which can load plugins, from
// XRT_INI_PATH, XILINX_XRT and an xrt.ini in the working directory; each is
// refused, with the reason in the error.
func TestXRTConfigRefused(t *testing.T) {
	if err := checkXRTConfig(); err != nil {
		t.Skipf("this test's own environment carries XRT configuration: %v", err)
	}
	for _, name := range []string{"XRT_INI_PATH", "XILINX_XRT"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "")
			if err := checkXRTConfig(); !errors.Is(err, ErrXRTConfig) || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s set: %v", name, err)
			}
		})
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "xrt.ini"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := checkXRTConfig(); !errors.Is(err, ErrXRTConfig) || !strings.Contains(err.Error(), "xrt.ini") {
		t.Fatalf("xrt.ini in the working directory: %v", err)
	}
}
