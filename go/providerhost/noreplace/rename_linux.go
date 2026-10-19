//go:build linux

package noreplace

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// RenameNew moves from to to only if nothing stands at to:
// renameat2(RENAME_NOREPLACE), and a hard link (linkRename) on a kernel or
// filesystem that refuses the flag.
func RenameNew(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return linkRename(from, to)
	}
	return &fs.PathError{Op: "rename", Path: to, Err: err}
}
