//go:build !windows

package enginelaunch

import (
	"errors"
	"os"
	"path/filepath"
)

// Other systems have no MAX_PATH limit for the gate to consider.
func longPathsEnabled() bool { return true }

func shortPathName(string) (string, error) {
	return "", errors.New("enginelaunch: 8.3 names exist only on Windows")
}

func finalPathName(folder string) (string, error) { return filepath.EvalSymlinks(folder) }

func isReparsePoint(p string) (bool, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}
