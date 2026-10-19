//go:build windows

package enginefiles

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openArchive opens the archive for reading with FILE_SHARE_READ alone, so
// no process can write, rename or delete it between the hash and the last
// entry read, and the open fails while any process holds it for writing.
func openArchive(p string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(extended(p))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(h), p), nil
}

// heldElsewhere opens the tree's folder for DELETE with full sharing, the
// access a rename needs, and closes it at once. A launch holds the folder
// with read sharing only (INF-F4), and the open then fails with a sharing
// violation; that is reported. Any other failure is left to the rename,
// which the unlock may have made possible.
func heldElsewhere(root string) error {
	name, err := windows.UTF16PtrFromString(extended(root))
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(name, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return err
	}
	if err == nil {
		//unchecked: a handle opened only to probe; nothing was written through it
		windows.CloseHandle(h)
	}
	return nil
}
