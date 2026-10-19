//go:build windows

package noreplace

import (
	"errors"
	"io/fs"
	"syscall"
)

// RenameNew moves from to to only if nothing stands at to: MoveFileW
// without MOVEFILE_REPLACE_EXISTING refuses an existing target.
func RenameNew(from, to string) error {
	src, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	if err := syscall.MoveFile(src, dst); err != nil {
		if errors.Is(err, syscall.ERROR_ALREADY_EXISTS) || errors.Is(err, syscall.ERROR_FILE_EXISTS) {
			return &fs.PathError{Op: "rename", Path: to, Err: fs.ErrExist}
		}
		return &fs.PathError{Op: "rename", Path: to, Err: err}
	}
	return nil
}
