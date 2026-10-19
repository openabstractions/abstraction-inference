package enginelaunch

import (
	"errors"
	"fmt"

	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// ErrSocketRoot reports a socket root the kit refuses: one that exists as
// something other than a directory, through a link, owned by another
// principal, or open to anyone beyond this account (and SYSTEM on Windows),
// or a platform with no per-user folder to put it in.
var ErrSocketRoot = errors.New("enginelaunch: engine socket root refused")

// ensurePrivateRoot makes root, the folder launch directories are created
// in, private to this account if it is absent, and refuses it unless it is a
// directory, reached through no link, owned by this account and open to
// nobody else (INF-EL2), through the provider kit's one private-folder
// helper. Its parent is created as an ordinary folder. A refusal wraps both
// ErrSocketRoot and the helper's privatedir.ErrNotPrivate.
func ensurePrivateRoot(root string) error {
	if err := privatedir.Ensure(root); err != nil {
		return fmt.Errorf("%w: %w", ErrSocketRoot, err)
	}
	return nil
}
