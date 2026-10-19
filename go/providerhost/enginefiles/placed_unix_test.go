//go:build unix

package enginefiles

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// INF-F7: a placed file below a symbolic-link folder is refused as a link on
// its path, as macOS's /tmp and /var are, whatever errno the open gave.
func TestPlainPathRefusesALinkedFolderAsALink(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.safetensors"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPlainPath(filepath.Join(real, "a.safetensors")); err != nil {
		t.Fatalf("a plain path refused: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckPlainPath(filepath.Join(link, "a.safetensors")); !errors.Is(err, errLinkOnPath) {
		t.Fatalf("a path through a linked folder: %v, want %v", err, errLinkOnPath)
	}
}
