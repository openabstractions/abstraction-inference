//go:build !windows && !unix

package enginelaunch

import (
	"fmt"
	"runtime"
)

type dirLock struct{}

func lockDir(string) (*dirLock, error) {
	return nil, fmt.Errorf("no file lock on %s", runtime.GOOS)
}

func (*dirLock) release() error { return nil }

// held reads every directory as held, so the sweep removes nothing here.
func held(string) bool { return true }
