//go:build darwin

package noreplace

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// RenameNew moves from to to only if nothing stands at to:
// renamex_np(RENAME_EXCL), and a hard link (linkRename) on a filesystem
// that refuses the flag.
func RenameNew(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.ENOSYS):
		return linkRename(from, to)
	}
	return &fs.PathError{Op: "rename", Path: to, Err: err}
}
