//go:build !windows && !unix

package enginelaunch

import (
	"fmt"
	"runtime"
)

// SocketRoot has no per-user folder on this platform.
func SocketRoot() (string, error) {
	return "", fmt.Errorf("%w: no per-user socket folder on %s", ErrSocketRoot, runtime.GOOS)
}
