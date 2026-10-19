//go:build windows

package enginefiles

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
)

// A tree or record on a drive identity refuses is refused, not placed by
// spelling (ID-F1).
func TestTreeOnARefusedDriveIsRefused(t *testing.T) {
	restore := identity.TreatDriveAsNetwork('Y')
	defer restore()
	for _, tree := range []Tree{
		{Name: "net", Root: `Y:\engines\x`, Record: `Y:\state\x.json`},
		{Name: "net", Root: `Y:\engines\x`, Record: filepath.Join(t.TempDir(), "x.json")},
		{Name: "net", Root: filepath.Join(t.TempDir(), "x"), Record: `Y:\state\x.json`},
	} {
		if err := tree.validate(); !errors.Is(err, identity.ErrNotLocalPath) {
			t.Errorf("validate(%s, %s) = %v, want ErrNotLocalPath", tree.Root, tree.Record, err)
		}
	}
}

// A tree still to be installed is placed by spelling.
func TestRecordInsideAMissingTreeIsRefusedBySpelling(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	tree := Tree{Name: "new", Root: root, Record: filepath.Join(root, "rec.json")}
	err := tree.validate()
	if err == nil || !strings.Contains(err.Error(), "outside the tree") {
		t.Fatalf("validate = %v, want the outside-the-tree refusal", err)
	}
}
