package noreplace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// A file moves to a free name; an existing target is refused with
// fs.ErrExist and both files keep their bytes.
func TestRenameNewNeverReplaces(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "a.part"), filepath.Join(dir, "a")
	if err := os.WriteFile(from, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNew(from, to); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "new" {
		t.Fatalf("moved %q %v", data, err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("the temporary name stayed: %v", err)
	}
	if err := os.WriteFile(from, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameNew(from, to); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("an existing target: %v", err)
	}
	if data, _ := os.ReadFile(to); string(data) != "new" {
		t.Fatalf("the existing file was replaced: %q", data)
	}
	if data, _ := os.ReadFile(from); string(data) != "second" {
		t.Fatalf("the refused file changed: %q", data)
	}
}
