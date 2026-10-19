//go:build !windows && !linux && !darwin

package noreplace

// RenameNew moves from to to only if nothing stands at to, through a hard
// link (linkRename).
func RenameNew(from, to string) error { return linkRename(from, to) }
