package enginelaunch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// gateFixture is a root with one library folder holding one kernel file
// whose opened path is total characters long with extra separators.
func gateFixture(t *testing.T, extra, total int) (string, GPULibrary) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "r")
	dir := filepath.Join(root, "lib", "site-packages", "bin", "hipblaslt", "library", "gfx1151")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	n := total - UTF16Len(dir) - 1 - extra
	if n < 20 || n > 250 {
		t.Skipf("temporary folder length %d leaves no room for a %d-character path", UTF16Len(dir), total)
	}
	name := "TensileLibrary_" + strings.Repeat("x", n-len("TensileLibrary_")-4) + ".dat"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, GPULibrary{Dir: dir, EnvVar: "HIPBLASLT_TENSILE_LIBPATH", Extra: extra}
}

func offProbe() GPUProbe {
	return GPUProbe{
		LongPathsEnabled: func() bool { return false },
		ShortPath:        func(string) (string, error) { return "", errors.New("no 8.3 names") },
		FinalPath:        func(p string) (string, error) { return `\\?\` + p, nil },
		ReparsePoint:     func(string) (bool, error) { return false, nil },
	}
}

// INF-EL3: a kernel path of MaxLibraryPath passes and one character more is
// refused, counting the library's own separators.
func TestGPUPathGateLimitIsExact(t *testing.T) {
	root, lib := gateFixture(t, 1, MaxLibraryPath)
	if env, err := GPUPathGate(root, []GPULibrary{lib}, offProbe()); err != nil || env != nil {
		t.Fatalf("a %d-character path failed: %v %v", MaxLibraryPath, env, err)
	}
	root, lib = gateFixture(t, 2, MaxLibraryPath+1)
	if _, err := GPUPathGate(root, []GPULibrary{lib}, offProbe()); !errors.Is(err, ErrKernelPathTooLong) {
		t.Fatalf("a %d-character path passed: %v", MaxLibraryPath+1, err)
	}
	probe := offProbe()
	probe.LongPathsEnabled = func() bool { return true }
	if env, err := GPUPathGate(root, []GPULibrary{lib}, probe); err != nil || env != nil {
		t.Fatalf("long paths on: %v %v", env, err)
	}
}

// INF-EL3: an 8.3 name replaces a deep folder only when it is shorter,
// resolves to the same folder under the root and passes no reparse point.
func TestGPUPathGateShortNames(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The probe below answers in Windows final-path form (\\?\).
		t.Skip("8.3 names exist only on Windows; elsewhere the gate has no path limit (gpugate_other.go)")
	}
	root, lib := gateFixture(t, 1, MaxLibraryPath+20)
	short := lib.Dir[:len(lib.Dir)-40] + strings.Repeat("s", 10)
	probe := offProbe()
	probe.ShortPath = func(p string) (string, error) {
		if p == lib.Dir {
			return short, nil
		}
		return p, nil
	}
	probe.FinalPath = func(p string) (string, error) {
		if p == short {
			return `\\?\` + lib.Dir, nil
		}
		return `\\?\` + p, nil
	}
	env, err := GPUPathGate(root, []GPULibrary{lib}, probe)
	if err != nil || len(env) != 1 || env[0] != "HIPBLASLT_TENSILE_LIBPATH="+short {
		t.Fatalf("env %v err %v", env, err)
	}
	elsewhere := probe
	elsewhere.FinalPath = func(p string) (string, error) { return `\\?\C:\elsewhere`, nil }
	if _, err := GPUPathGate(root, []GPULibrary{lib}, elsewhere); !errors.Is(err, ErrKernelPathTooLong) {
		t.Fatalf("an 8.3 name resolving elsewhere passed: %v", err)
	}
	linked := probe
	linked.ReparsePoint = func(p string) (bool, error) { return p == filepath.Dir(lib.Dir), nil }
	if _, err := GPUPathGate(root, []GPULibrary{lib}, linked); !errors.Is(err, ErrKernelPathTooLong) {
		t.Fatalf("a reparse point on the way passed: %v", err)
	}
}

// An empty kernel folder is no GPU library at all.
func TestGPUPathGateNeedsKernelFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := GPUPathGate(root, []GPULibrary{{Dir: root, EnvVar: "X", Extra: 1}}, offProbe()); err == nil {
		t.Fatal("an empty kernel folder passed")
	}
}
