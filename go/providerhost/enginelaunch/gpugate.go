package enginelaunch

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// MaxLibraryPath is the longest path the ROCm libraries open successfully
// without Windows long-path support: MAX_PATH 260 including the terminator
// (research/adoption/thenoise-gpu-crash/FINDINGS.md).
const MaxLibraryPath = 259

// ErrKernelPathTooLong is the refusal of a GPU launch whose kernel files a ROCm
// library could not open with Windows long paths off (inference CONTRACT.md
// INF-EL3). A provider names its own engine in the sentence it shows.
var ErrKernelPathTooLong = errors.New("enginelaunch: a GPU library's kernel files lie too deep to open with Windows long paths off")

// A GPULibrary is one ROCm library whose kernel files are opened by a path
// the library builds itself, with separators it inserts.
type GPULibrary struct {
	// Dir is the library's gfx kernel folder, an absolute path below the
	// engine's root.
	Dir string
	// EnvVar points the library at Dir when an 8.3 name must replace it,
	// such as HIPBLASLT_TENSILE_LIBPATH.
	EnvVar string
	// Extra is the number of separator characters the library adds beyond
	// one between Dir and a file name: hipBLASLt opens "gfx1151\/Tensile...",
	// one more; rocBLAS "...//Tensile...", two more (FINDINGS evidence 2 and
	// 5).
	Extra int
}

// GPUProbe holds the operating-system questions the gate asks. Production
// uses SystemGPUProbe; tests substitute answers.
type GPUProbe struct {
	LongPathsEnabled func() bool
	// ShortPath is GetShortPathNameW.
	ShortPath func(string) (string, error)
	// FinalPath opens a folder and returns GetFinalPathNameByHandleW of it.
	FinalPath func(string) (string, error)
	// ReparsePoint reports FILE_ATTRIBUTE_REPARSE_POINT of one path.
	ReparsePoint func(string) (bool, error)
}

// SystemGPUProbe answers the gate's questions from this machine.
func SystemGPUProbe() GPUProbe {
	return GPUProbe{LongPathsEnabled: longPathsEnabled, ShortPath: shortPathName, FinalPath: finalPathName, ReparsePoint: isReparsePoint}
}

// UTF16Len is the length of s in UTF-16 code units, the unit Windows paths
// are counted in.
func UTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// KernelFiles lists every file below dir as paths relative to dir: the
// mapping file selects a kernel library by GEMM type and layout, so every
// file in the folder is reachable.
func KernelFiles(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enginelaunch: listing GPU kernel files: %w", err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("enginelaunch: no GPU kernel files under %s", dir)
	}
	return names, nil
}

// LongestOpen is the longest path a library opens for files when its kernel
// folder is spelled base.
func LongestOpen(base string, extra int, files []string) int {
	longest := 0
	for _, rel := range files {
		if n := UTF16Len(base) + 1 + UTF16Len(rel) + extra; n > longest {
			longest = n
		}
	}
	return longest
}

// GPUPathGate is the path-length gate every provider runs before a launch
// that loads ROCm kernel files on Windows (inference CONTRACT.md INF-EL3).
// It returns the extra environment the launch needs: nothing when every
// kernel path fits or Windows long paths are on, or each library's EnvVar
// set to the verified 8.3 name of its folder. The 8.3 name must be shorter,
// resolve under root's own final path to the same folder, pass no reparse
// point from root down, and fit; otherwise the launch is refused with
// ErrKernelPathTooLong.
func GPUPathGate(root string, libraries []GPULibrary, probe GPUProbe) ([]string, error) {
	root = filepath.Clean(root)
	files := make([][]string, len(libraries))
	fits := true
	for i, lib := range libraries {
		names, err := KernelFiles(lib.Dir)
		if err != nil {
			return nil, err
		}
		files[i] = names
		if LongestOpen(lib.Dir, lib.Extra, names) > MaxLibraryPath {
			fits = false
		}
	}
	if fits || probe.LongPathsEnabled() {
		return nil, nil
	}
	bundle, err := probe.FinalPath(root)
	if err != nil || bundle == "" {
		return nil, ErrKernelPathTooLong
	}
	env := make([]string, 0, len(libraries))
	for i, lib := range libraries {
		long := filepath.Clean(lib.Dir)
		if err := noReparsePoint(root, long, probe); err != nil {
			return nil, err
		}
		short, err := probe.ShortPath(long)
		if err != nil || short == "" || !filepath.IsAbs(short) || UTF16Len(short) >= UTF16Len(long) {
			return nil, ErrKernelPathTooLong
		}
		// The 8.3 form must open the same folder under the root's own
		// resolved path; an alias that resolves elsewhere would load kernels
		// the engine's proof never covered.
		// Final paths are the kernel's one spelling of an existing folder, so
		// they compare exactly: the 8.3 name opens the library folder, and
		// that folder lies under root's own final path.
		resolved, err := probe.FinalPath(short)
		target, targetErr := probe.FinalPath(long)
		if err != nil || targetErr != nil || resolved != target || !strings.HasPrefix(target, strings.TrimRight(bundle, `\`)+`\`) {
			return nil, ErrKernelPathTooLong
		}
		if LongestOpen(short, lib.Extra, files[i]) > MaxLibraryPath {
			return nil, ErrKernelPathTooLong
		}
		env = append(env, lib.EnvVar+"="+short)
	}
	return env, nil
}

// noReparsePoint refuses a junction, symbolic link or other reparse point on
// the way from root down to folder, both included.
func noReparsePoint(root, folder string, probe GPUProbe) error {
	for p := folder; ; p = filepath.Dir(p) {
		reparse, err := probe.ReparsePoint(p)
		if err != nil || reparse {
			return ErrKernelPathTooLong
		}
		if p == root || filepath.Dir(p) == p {
			return nil
		}
	}
}
