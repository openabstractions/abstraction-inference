//go:build windows

package enginelaunch

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// A dirLock is a launch directory's held file, open with no sharing, so no
// other open of it succeeds and the directory cannot be removed while a
// provider holds it.
type dirLock struct{ handle windows.Handle }

func lockDir(dir string) (*dirLock, error) {
	name, err := windows.UTF16PtrFromString(filepath.Join(dir, heldName))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return &dirLock{handle: h}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(l.handle)
	l.handle = 0
	return err
}

// held reports whether a provider holds dir's held file.
func held(dir string) bool {
	l, err := lockDir(dir)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true
	}
	if err != nil {
		return false
	}
	//unchecked: the lock was taken only to learn nobody held it
	l.release()
	return false
}
