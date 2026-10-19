//go:build windows

package enginefiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// INF-F3, INF-F6 on Windows: replacing a tree a running engine holds is
// refused before anything changes. The held tree keeps its read-only
// folder ACL and launches again once the engine has exited.
func TestReplacingAHeldTreeIsRefusedWithoutUnlockingIt(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	t.Cleanup(func() { Unlock(root) })
	v := NewVerifier(Options{})
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	hold, err := v.Launch(t.Context(), in.Tree)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.InstallFromArchive(t.Context(), replacing(in, "b2\n"))
	if err == nil {
		hold.Release()
		t.Fatal("a held tree was replaced")
	}
	if !errors.Is(err, ErrArchive) || !strings.Contains(err.Error(), "in use") {
		hold.Release()
		t.Fatalf("the held tree was not refused by the probe: %v", err)
	}
	if ok, err := Installed(root); err != nil || !ok {
		hold.Release()
		t.Fatalf("the held tree lost its read-only folder ACL: %v", err)
	}
	absent(t, root+".previous", root+".partial")
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	h, err := v.Launch(t.Context(), in.Tree)
	if err != nil {
		t.Fatalf("the tree does not launch again: %v", err)
	}
	h.Release()
}

// A hold on one file of the tree passes the probe of its folder, and the
// unlocked folder then fails to move aside. The relock gives the folder its
// read-only ACL again: the refusal comes from the rename, and the tree stays
// installed.
func TestAReplacementBlockedByAHeldFileRelocksTheTree(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	t.Cleanup(func() { Unlock(root) })
	v := NewVerifier(Options{})
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	held, err := openHeld(filepath.Join(root, "llama-server.exe"), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.InstallFromArchive(t.Context(), replacing(in, "b2\n"))
	if cerr := held.close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil || errors.Is(err, ErrArchive) {
		t.Fatalf("wanted the rename's refusal, got %v", err)
	}
	if ok, err := Installed(root); err != nil || !ok {
		t.Fatalf("the tree lost its read-only folder ACL: %v", err)
	}
	absent(t, root+".previous", root+".partial")
	h, err := v.Launch(t.Context(), in.Tree)
	if err != nil {
		t.Fatalf("the tree does not launch again: %v", err)
	}
	h.Release()
}

// INF-F6 on Windows: the archive is held with read sharing only from the
// hash to the last entry read, so no process can rewrite it between the
// two, and a writer that already holds it makes the install fail.
func TestTheArchiveIsHeldWithoutWriteSharing(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, _ := llamaInstall(t, a)
	writable := false
	in.Progress = func(done, total int64) error {
		f, err := os.OpenFile(a.Path, os.O_WRONLY, 0)
		if err == nil {
			writable = true
			f.Close()
		}
		return nil
	}
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if writable {
		t.Fatal("the archive opened for writing during the install")
	}
	writer, err := os.OpenFile(a.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	other, root := llamaInstall(t, a)
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), other); err == nil {
		t.Fatal("an archive another handle holds for writing was installed")
	} else if errors.Is(err, ErrArchive) {
		t.Fatalf("the open, not the content, should refuse: %v", err)
	}
	absent(t, root)
}

// INF-F6: a replacement of a tree with a file another program holds open
// without sharing is refused as unreadable before the folder is touched, and
// the tree is kept.
func TestAReplacementOfAnUnreadableTreeIsRefused(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	v := NewVerifier(Options{ACL: newFakeACL()})
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(filepath.Join(root, "llama-server.exe"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.InstallFromArchive(t.Context(), replacing(in, "b2\n"))
	windows.CloseHandle(lock)
	if !errors.Is(err, ErrUnreadable) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, ErrMismatch) {
		t.Fatalf("a replacement of a tree with a locked file: %v, want the unreadable refusal", err)
	}
	absent(t, root+".previous", root+".partial")
	h, err := v.Launch(t.Context(), in.Tree)
	if err != nil {
		t.Fatalf("the kept tree does not launch: %v", err)
	}
	h.Release()
}
