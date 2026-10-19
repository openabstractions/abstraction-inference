//go:build !windows

package noreplace

import "os"

// linkRename moves from to to only if nothing stands at to: a hard link
// refuses an existing target (EEXIST, fs.ErrExist), and the temporary name
// is then removed. It is RenameNew where the platform has no exclusive
// rename and the fallback where a filesystem refuses one.
func linkRename(from, to string) error {
	if err := os.Link(from, to); err != nil {
		return err
	}
	return os.Remove(from)
}
