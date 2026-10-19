// Package npudecode decodes exp-fixed3-v1 units (inference CONTRACT.md
// INF-MP8) on an AMD XDNA 2 NPU for a provider host's fill from a model
// archive (INF-MP10). It loads OA's shim over XRT, oa_npu_decode.dll, with
// LoadLibraryEx and calls it through syscall, no cgo; the shim loads the
// NPU driver's xrt_coreutil.dll. A Device is a modelpack.FixedDevice: two
// buffer sets of Units() unit slots in host memory the NPU reads and
// writes, one prebuilt design (an xclbin and its instruction stream) run
// once per batch under a time limit.
//
// Open returns a device only after modelpack.CheckFixedDevice decoded a
// known batch on it bit for bit; anything else, no NPU or no driver
// included, is an error, and the caller fills on the CPU. A device that
// fails or times out once refuses every later batch for the life of the
// process. The fill proves every byte the device writes (INF-MS8).
//
// The kit folder holds what native/make_kit.ps1 builds: oa_npu_decode.dll,
// fixed3.xclbin, fixed3.insts, fixed3.json and kit.json, their SHA-256.
// Whatever can write a kit file runs code in the provider host, so every
// file, kit.json included, must pass identity's ID-D2 (trustedpath) with
// the machine's owners and this account, and match kit.json before the
// shim loads; the shim's imports come from System32
// only, and XRT configuration of the process's own (XRT_INI_PATH,
// XILINX_XRT, an xrt.ini in the working directory or beside the
// executable) is refused, since it can load XRT plugins. Windows only;
// elsewhere Open reports errors.ErrUnsupported.
package npudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openabstractions/abstraction-inference/go/providerhost/modelpack"
)

// The kit's file names. ManifestFile pins the SHA-256 of the other four.
const (
	ShimFile     = "oa_npu_decode.dll"
	XclbinFile   = "fixed3.xclbin"
	InstsFile    = "fixed3.insts"
	ShapeFile    = "fixed3.json"
	ManifestFile = "kit.json"
)

// kitFiles are the files the manifest pins.
var kitFiles = []string{ShimFile, XclbinFile, InstsFile, ShapeFile}

// Manifest is kit.json: the lower-case hex SHA-256 of each kit file, as
// native/make_kit.ps1 writes it.
type Manifest struct {
	Format  string            `json:"format"`
	Version int               `json:"version"`
	SHA256  map[string]string `json:"sha256"`
}

// readManifest reads and checks dir's kit.json; pin, when not empty, is the
// SHA-256 the manifest's bytes must have.
func readManifest(dir, pin string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrKit, err)
	}
	return parseManifest(b, pin)
}

// parseManifest checks kit.json's bytes b; pin, when not empty, is the
// SHA-256 they must have.
func parseManifest(b []byte, pin string) (Manifest, error) {
	if pin != "" {
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != pin {
			return Manifest{}, fmt.Errorf("%w: %s is not the manifest pinned as %s", ErrKit, ManifestFile, pin)
		}
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %s: %v", ErrKit, ManifestFile, err)
	}
	if m.Format != "oa-npu-kit" || m.Version != 1 || len(m.SHA256) != len(kitFiles) {
		return Manifest{}, fmt.Errorf("%w: %s is %q version %d with %d files", ErrKit, ManifestFile, m.Format, m.Version, len(m.SHA256))
	}
	for _, f := range kitFiles {
		if d := m.SHA256[f]; len(d) != 64 || strings.ToLower(d) != d {
			return Manifest{}, fmt.Errorf("%w: %s pins %s as %q", ErrKit, ManifestFile, f, d)
		}
	}
	return m, nil
}

// checkDigest refuses b unless its SHA-256 is the manifest's for name.
func (m Manifest) checkDigest(name string, b []byte) error {
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != m.SHA256[name] {
		return fmt.Errorf("%w: %s differs from the SHA-256 %s pins", ErrKit, name, ManifestFile)
	}
	return nil
}

// ErrXRTConfig: XRT would read a configuration of this process's
// environment or folders, which can load XRT plugins into the provider
// host.
var ErrXRTConfig = errors.New("npudecode: XRT configuration outside OA's control")

// checkXRTConfig refuses when XRT_INI_PATH or XILINX_XRT is set, or an
// xrt.ini stands in the working directory or beside the executable, the
// places XRT reads its configuration from.
func checkXRTConfig() error {
	for _, name := range []string{"XRT_INI_PATH", "XILINX_XRT"} {
		if _, set := os.LookupEnv(name); set {
			return fmt.Errorf("%w: %s is set", ErrXRTConfig, name)
		}
	}
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		if _, err := os.Lstat(filepath.Join(d, "xrt.ini")); err == nil {
			return fmt.Errorf("%w: %s holds an xrt.ini", ErrXRTConfig, d)
		}
	}
	return nil
}

// DefaultTimeout bounds one batch's run on the NPU. A batch of 64 units
// (32 MiB decoded) takes 1 to 5 ms (GPU-AND-MATRIX-DECOMPRESSION.md §13).
const DefaultTimeout = 500 * time.Millisecond

// Sets is the number of buffer sets: one batch is read while the other
// decodes.
const Sets = 2

// Config locates the kit.
type Config struct {
	// Dir is the kit folder. Each kit file must pass trustedpath.CheckFile
	// (identity ID-D2) with the machine's owners and this account:
	// whatever can write one runs code in this process.
	Dir string
	// ManifestSHA256, when not empty, pins kit.json's own SHA-256, for a
	// caller that holds it from a signed provider manifest.
	ManifestSHA256 string
	// Timeout bounds one batch's run; zero is DefaultTimeout.
	Timeout time.Duration
}

// Shape is fixed3.json: the batch the compiled design decodes.
type Shape struct {
	Format    string `json:"format"`
	Version   int    `json:"version"`
	Kernel    string `json:"kernel"`
	Units     int    `json:"units"`
	Slot      int    `json:"slot"`
	UnitBytes int    `json:"unit_bytes"`
	Columns   int    `json:"columns"`
}

// ErrKit: the kit folder lacks a file, or its design does not fit
// modelpack's units.
var ErrKit = errors.New("npudecode: kit")

// readShape reads and checks dir's fixed3.json.
func readShape(dir string) (Shape, error) {
	b, err := os.ReadFile(filepath.Join(dir, ShapeFile))
	if err != nil {
		return Shape{}, fmt.Errorf("%w: %v", ErrKit, err)
	}
	return parseShape(b)
}

// parseShape checks fixed3.json's bytes.
func parseShape(b []byte) (Shape, error) {
	var s Shape
	if err := json.Unmarshal(b, &s); err != nil {
		return Shape{}, fmt.Errorf("%w: %s: %v", ErrKit, ShapeFile, err)
	}
	switch {
	case s.Format != "oa-npu-fixed3" || s.Version != 1:
		return Shape{}, fmt.Errorf("%w: %s is %q version %d", ErrKit, ShapeFile, s.Format, s.Version)
	case s.Slot != modelpack.FixedSlot || s.UnitBytes != modelpack.ChunkBytes:
		return Shape{}, fmt.Errorf("%w: the design reads slots of %d bytes into units of %d, modelpack's are %d and %d",
			ErrKit, s.Slot, s.UnitBytes, modelpack.FixedSlot, modelpack.ChunkBytes)
	case s.Units < 1 || s.Units > 256:
		return Shape{}, fmt.Errorf("%w: %d units per batch", ErrKit, s.Units)
	case s.Kernel == "" || len(s.Kernel) > 64:
		return Shape{}, fmt.Errorf("%w: kernel name %q", ErrKit, s.Kernel)
	}
	return s, nil
}
