//go:build windows

package enginefiles

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// junction makes link a directory junction to target, inside the test's own
// temporary folder, and removes the link (never the target) at cleanup.
func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	t.Cleanup(func() {
		//unchecked: test cleanup; a junction left behind is removed with the temporary folder's link entry
		os.Remove(link)
	})
}

// INF-F7, the review of da8e3b548: the path an engine opens is tied to the
// held file. A path through a junction is refused; once a file is held the
// folders on its path cannot be renamed, so no junction can take their
// place; Hold.Paths names the held file itself; and a junction swapped in
// after a release is refused at the next launch, so the engine never reads
// the other bytes the swap points at.
func TestPlacedPathCannotBeRedirectedByAJunction(t *testing.T) {
	root := plainTempDir(t)
	store := filepath.Join(root, "store")
	pinned := placedFile(t, filepath.Join(store, "models"), "weights.gguf", "the pinned weights")
	placedFile(t, filepath.Join(root, "elsewhere", "models"), "weights.gguf", "other weights!!!!!")
	record := filepath.Join(root, "state", "placed.files")
	v := NewVerifier(Options{})
	ctx := context.Background()

	link := filepath.Join(root, "link")
	junction(t, link, filepath.Join(store, "models"))
	via := pinned
	via.Path = filepath.Join(link, "weights.gguf")
	if _, err := v.LaunchPlaced(ctx, "test set", []Placed{via}, record); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a path through a junction: %v", err)
	}
	if err := CheckPlainPath(via.Path); err == nil {
		t.Fatal("CheckPlainPath passed a path through a junction")
	}
	if err := CheckPlainPath(pinned.Path); err != nil {
		t.Fatalf("a plain path: %v", err)
	}

	hold, err := v.LaunchPlaced(ctx, "test set", []Placed{pinned}, record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hold.Release() })
	paths := hold.Paths()
	if len(paths) != 1 || !strings.EqualFold(paths[0], pinned.Path) || strings.HasPrefix(paths[0], `\\?\`) {
		t.Fatalf("final paths %q for %s", paths, pinned.Path)
	}
	for _, dir := range []string{filepath.Join(store, "models"), store} {
		if err := os.Rename(dir, dir+".moved"); err == nil {
			t.Fatalf("%s was renamed while its file was held", dir)
		}
	}
	if data, err := os.ReadFile(paths[0]); err != nil || string(data) != "the pinned weights" {
		t.Fatalf("the final path read %q, %v", data, err)
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	// The reviewer's swap: the store's folder replaced by a junction to a
	// folder holding other bytes of the same size at the same name.
	if err := os.Rename(store, store+".moved"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	junction(t, store, filepath.Join(root, "elsewhere"))
	if data, err := os.ReadFile(pinned.Path); err != nil || string(data) != "other weights!!!!!" {
		t.Fatalf("the swap did not take: %q %v", data, err)
	}
	if _, err := v.LaunchPlaced(ctx, "test set", []Placed{pinned}, record); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a launch after the swap: %v", err)
	}
}

// The folder holds matter: between the check of a folder and the opening of
// the next one through it, the checked folder cannot be renamed away and
// replaced by a junction, because it is already held. Without the hold the
// rename succeeds here, as no file below the folder is open yet.
func TestPlacedFolderCannotBeSwappedBetweenCheckAndOpen(t *testing.T) {
	root := plainTempDir(t)
	store := filepath.Join(root, "store")
	pinned := placedFile(t, filepath.Join(store, "models"), "weights.gguf", "the pinned weights")
	placedFile(t, filepath.Join(root, "elsewhere", "models"), "weights.gguf", "other weights!!!!!")
	var renamed error = errors.New("the hook never saw the store folder")
	afterFolder = func(dir string) {
		if dir != store {
			return
		}
		renamed = os.Rename(store, store+".moved")
		if renamed == nil {
			junction(t, store, filepath.Join(root, "elsewhere"))
		}
	}
	t.Cleanup(func() { afterFolder = nil })
	hold, err := NewVerifier(Options{}).LaunchPlaced(context.Background(), "test set", []Placed{pinned}, filepath.Join(root, "state", "placed.files"))
	if hold != nil {
		t.Cleanup(func() { hold.Release() })
	}
	if renamed == nil {
		t.Fatal("a held folder was renamed between its check and the next open")
	}
	if err != nil {
		t.Fatal(err)
	}
	if paths := hold.Paths(); len(paths) != 1 || !strings.EqualFold(paths[0], pinned.Path) {
		t.Fatalf("final paths %q", paths)
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
}

// Use the owner's held-handle resolver for a controlled plain fixture root;
// TEMP may use an 8.3 account spelling while the kernel returns its long name.
func plainTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	held, err := HoldDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	final, err := held.nodes[len(held.nodes)-1].finalPath()
	if err != nil {
		t.Fatal(err)
	}
	return final
}
