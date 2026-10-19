//go:build unix

package enginelaunch

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// A dirLock is an exclusive flock on a launch directory's held file; a
// second lock on it, from any process, fails while a provider holds it.
type dirLock struct{ file *os.File }

func lockDir(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, heldName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		//unchecked: the lock failure is the error returned
		f.Close()
		return nil, err
	}
	return &dirLock{file: f}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// held reports whether a provider holds dir's held file.
func held(dir string) bool {
	l, err := lockDir(dir)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		return false
	}
	//unchecked: the lock was taken only to learn nobody held it
	l.release()
	return false
}
