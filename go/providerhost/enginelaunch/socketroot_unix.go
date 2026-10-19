//go:build unix

package enginelaunch

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// SocketRoot is the fixed per-user folder engine launch directories live
// in, whatever state folder a provider uses (INF-EL2): on Linux
// $XDG_RUNTIME_DIR/oa, or /tmp/oa-<uid> when the runtime directory is unset,
// relative, or not an owner-only directory of this account; on macOS
// $TMPDIR/oa, the per-user temporary folder, with the same fallback.
func SocketRoot() (string, error) {
	uid := os.Getuid()
	variable := "XDG_RUNTIME_DIR"
	if runtime.GOOS == "darwin" {
		variable = "TMPDIR"
	}
	if base := os.Getenv(variable); filepath.IsAbs(base) {
		if info, err := os.Stat(base); err == nil && privatedir.OwnerOnly(info) == nil {
			return filepath.Join(filepath.Clean(base), "oa"), nil
		}
	}
	return filepath.Join("/tmp", "oa-"+strconv.Itoa(uid)), nil
}
