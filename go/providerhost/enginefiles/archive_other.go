//go:build !windows

package enginefiles

import "os"

// openArchive opens the archive for reading. Outside Windows the open
// descriptor blocks no writer; the hash and every entry's own SHA-256 are
// the guarantee there (INF-F5, INF-F6).
func openArchive(p string) (*os.File, error) { return os.Open(p) }

// heldElsewhere reports nothing outside Windows, where an open descriptor
// blocks no rename (INF-F5).
func heldElsewhere(string) error { return nil }
