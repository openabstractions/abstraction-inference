//go:build windows

package enginefiles

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// INF-F4: while a Hold is open, Windows refuses writes, renames and deletes
// of the tree's files and the rename of its folders; after Release they work.
func TestHeldFilesRefuseWriteRenameAndDelete(t *testing.T) {
	tree, v, _ := installed(t)
	h := launch(t, v, tree)
	file := filepath.Join(tree.Root, "lib", "ggml.dll")
	folder := filepath.Join(tree.Root, "lib", "python")
	attempts := map[string]func() error{
		"write": func() error {
			f, err := os.OpenFile(file, os.O_WRONLY, 0)
			if err == nil {
				f.Close()
			}
			return err
		},
		"rename":        func() error { return os.Rename(file, file+".old") },
		"delete":        func() error { return os.Remove(file) },
		"folder rename": func() error { return os.Rename(folder, folder+".old") },
	}
	for name, try := range attempts {
		if err := try(); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("%s during the hold: %v, want a sharing violation", name, err)
		}
	}
	// A reader that shares reading, as the loader and Python do, still opens
	// the held file.
	if data, err := os.ReadFile(file); err != nil || string(data) != "ggml" {
		t.Fatalf("reading a held file: %q %v", data, err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"write", "rename"} {
		if err := attempts[name](); err != nil {
			t.Fatalf("%s after release: %v", name, err)
		}
	}
}

// INF-F2: a file swapped for a new one of the same size, with its last-write
// and change times set back to the recorded ones, differs only by file ID.
// The ID sends the launch to the full hash, which refuses.
func TestMetadataCheckCatchesASwappedFileByID(t *testing.T) {
	tree, v, _ := installed(t)
	rel := "lib/ggml.dll"
	file := filepath.Join(tree.Root, filepath.FromSlash(rel))
	before := metaOf(t, v, tree, rel)
	write(t, file+".new", "GGML")
	if err := os.Rename(file+".new", file); err != nil {
		t.Fatal(err)
	}
	setTimes(t, file, before.write, before.change)
	after := metaOf(t, v, tree, rel)
	if after.size != before.size || after.write != before.write || after.change != before.change || after.id == before.id {
		t.Fatalf("the fixture must differ by file ID only: %+v %+v", before, after)
	}
	_, err := v.Launch(t.Context(), tree)
	mismatch(t, err, Changed, rel)
}

// INF-F2: robocopy /COPY:T, touch -r and archive extractors set a file's
// last-write time back through SetFileTime after rewriting it. A same-size
// rewrite in place with the last-write time restored differs from the record
// by change time alone; the launch hashes in full and refuses.
func TestMetadataCheckCatchesARewriteWithRestoredLastWriteTime(t *testing.T) {
	tree, v, _ := installed(t)
	rel := "lib/python/module.py"
	file := filepath.Join(tree.Root, filepath.FromSlash(rel))
	before, changedBefore := metaOf(t, v, tree, rel), basicInfo(t, file).ChangeTime
	time.Sleep(20 * time.Millisecond)
	write(t, file, "print(2)\n")
	setLastWriteTime(t, file, before.write)
	after, changedAfter := metaOf(t, v, tree, rel), basicInfo(t, file).ChangeTime
	if after.id != before.id || after.size != before.size || after.write != before.write || changedAfter == changedBefore {
		t.Fatalf("the fixture must differ by change time only: %+v %+v", before, after)
	}
	h, err := v.Launch(t.Context(), tree)
	h.Release()
	mismatch(t, err, Changed, rel)
}

// basicInfo reads FILE_BASIC_INFO directly, apart from the kit's own reader.
func basicInfo(t *testing.T, file string) fileBasicInfo {
	t.Helper()
	name, _ := windows.UTF16PtrFromString(file)
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var b fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		t.Fatal(err)
	}
	return b
}

// setLastWriteTime restores a file's last-write time through SetFileTime,
// which leaves the change time as the rewrite set it.
func setLastWriteTime(t *testing.T, file string, write int64) {
	t.Helper()
	name, _ := windows.UTF16PtrFromString(file)
	h, err := windows.CreateFile(name, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	ft := windows.Filetime{LowDateTime: uint32(write), HighDateTime: uint32(uint64(write) >> 32)}
	if err := windows.SetFileTime(h, nil, nil, &ft); err != nil {
		t.Fatal(err)
	}
}

// setTimes writes a file's last-write and change times, as any program with
// write access to it can.
func setTimes(t *testing.T, file string, write, change int64) {
	t.Helper()
	name, _ := windows.UTF16PtrFromString(file)
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var b fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		t.Fatal(err)
	}
	b.LastWriteTime, b.ChangeTime = write, change
	if err := windows.SetFileInformationByHandle(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&b)), uint32(unsafe.Sizeof(b))); err != nil {
		t.Fatal(err)
	}
}

// realInstall installs a fixture tree with the system ACL and unlocks it
// before the temporary folder is removed.
func realInstall(t *testing.T) (Tree, *Verifier) {
	t.Helper()
	tree := writeTree(t, smallTree())
	t.Cleanup(func() {
		if err := Unlock(tree.Root); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})
	v := NewVerifier(Options{})
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	return tree, v
}

// INF-F3: Install gives every folder the protected read-only DACL; the
// account can then read but neither change, add, rename nor delete a file.
func TestInstallSetsTheReadOnlyDACLOnEveryFolder(t *testing.T) {
	tree, v := realInstall(t)
	sid, err := AccountSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range foldersOf(t, tree.Root) {
		if ok, err := FolderACLIs(folder, sid, ReadOnly); err != nil || !ok {
			t.Fatalf("%s lacks the protected read-only DACL: %v %v", folder, ok, err)
		}
	}
	file := filepath.Join(tree.Root, "lib", "python", "module.py")
	if data, err := os.ReadFile(file); err != nil || string(data) != "print(1)\n" {
		t.Fatalf("the account cannot read an installed file: %v", err)
	}
	for name, try := range map[string]func() error{
		"write":  func() error { return os.WriteFile(file, []byte("print(2)\n"), 0o644) },
		"add":    func() error { return os.WriteFile(filepath.Join(tree.Root, "lib", "version.dll"), []byte("x"), 0o644) },
		"rename": func() error { return os.Rename(file, file+".old") },
		"delete": func() error { return os.Remove(file) },
	} {
		if err := accountAccess(t, try); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("%s under the read-only DACL: %v, want access denied", name, err)
		}
	}
	if h := launch(t, v, tree); h.Full() {
		t.Fatal("the launch after install hashed again")
	}
}

// INF-F3: a launch refuses a tree whose folder DACL is not the read-only one:
// a tree never installed, and an installed tree whose subfolder the account
// opened again.
func TestLaunchRefusesWithoutTheReadOnlyDACL(t *testing.T) {
	plain := writeTree(t, smallTree())
	if _, err := NewVerifier(Options{}).Launch(t.Context(), plain); !errors.Is(err, ErrFolderACL) || !strings.Contains(err.Error(), "install the engine again") {
		t.Fatalf("a tree without the read-only DACL launched, or the refusal names no remedy: %v", err)
	}
	if ok, err := Installed(plain.Root); err != nil || ok {
		t.Fatalf("a tree never installed counts as installed: %v %v", ok, err)
	}
	tree, v := realInstall(t)
	if ok, err := Installed(tree.Root); err != nil || !ok {
		t.Fatalf("an installed tree does not count as installed: %v %v", ok, err)
	}
	sid, _ := AccountSID()
	folder := filepath.Join(tree.Root, "lib", "python")
	if err := setOwnFolderACL(folder, sid, FullControl); err != nil {
		t.Fatal(err)
	}
	if ok, err := Installed(tree.Root); err != nil || ok {
		t.Fatalf("a tree with an opened subfolder counts as installed: %v %v", ok, err)
	}
	if _, err := v.Launch(t.Context(), tree); !errors.Is(err, ErrFolderACL) {
		t.Fatalf("a subfolder with the account's full control launched: %v", err)
	}
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	launch(t, v, tree)
}

// A junction, which the account may create without privilege, is refused
// where the manifest names a folder.
func TestAJunctionInTheTreeIsRefused(t *testing.T) {
	tree := writeTree(t, smallTree())
	data := filepath.Join(tree.Root, "lib", "python", "data")
	outside := filepath.Join(filepath.Dir(tree.Root), "outside")
	if err := os.Rename(data, outside); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/d", "/c", "mklink", "/J", data, outside).CombinedOutput(); err != nil {
		t.Skipf("no junction: %v %s", err, out)
	}
	err := Verify(t.Context(), "fixture", tree.Root, tree.Manifest)
	mismatch(t, err, Special, "lib/python/data")
}

// INF-F3: a file created under a subfolder after Unlock, as a replacement
// writes it, ends read-only after the next Install.
func TestAFileCreatedAfterUnlockEndsReadOnlyAfterInstall(t *testing.T) {
	tree, v := realInstall(t)
	if err := Unlock(tree.Root); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tree.Root, "lib", "python", "data", "k.dat")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	write(t, file, "kernel")
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	if err := accountAccess(t, func() error { return os.WriteFile(file, []byte("KERNEL"), 0o644) }); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("a file created after Unlock stayed writable after Install: %v", err)
	}
}

// An ACE of another type is refused before its SID is read: an object ACE
// keeps its SID behind flags and GUIDs, where the plain layout is wrong.
func TestADACLWithAnotherACETypeIsRefused(t *testing.T) {
	sid, err := AccountSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, sddl := range []string{
		"D:P(OA;OICI;0x1200a9;bf967a86-0de6-11d0-a285-00aa003049e2;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
		"D:P(D;OICI;0x1200a9;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
	} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatalf("%s: %v", sddl, err)
		}
		if ok, err := descriptorIs(sd, sid, ReadOnly); ok || err != nil {
			t.Fatalf("%s accepted: %v %v", sddl, ok, err)
		}
	}
}

// Unlock lets the account replace and remove an installed tree.
func TestUnlockAllowsRemoval(t *testing.T) {
	tree, _ := realInstall(t)
	if err := Unlock(tree.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tree.Root, tree.Root+".previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tree.Root + ".previous"); err != nil {
		t.Fatalf("an unlocked tree could not be removed: %v", err)
	}
	if err := os.MkdirAll(tree.Root, 0o755); err != nil { // for the cleanup's Unlock
		t.Fatal(err)
	}
}

// A remover that deletes a folder holding installed trees, such as a retired
// package version, gives back every read-only tree below it first: an
// installed tree and one a killed Unlock left half open, whose subfolders
// keep their own read-only DACL. Folders outside any tree keep their DACL.
func TestUnlockWithinAllowsRemovalOfTheFolderAround(t *testing.T) {
	tree, _ := realInstall(t)
	around := filepath.Dir(tree.Root)
	half := writeTree(t, smallTree())
	t.Cleanup(func() {
		if err := Unlock(half.Root); err != nil {
			t.Errorf("unlock: %v", err)
		}
	})
	if err := NewVerifier(Options{}).Install(t.Context(), half); err != nil {
		t.Fatal(err)
	}
	sid, err := AccountSID()
	if err != nil {
		t.Fatal(err)
	}
	if err := setOwnFolderACL(half.Root, sid, FullControl); err != nil {
		t.Fatal(err)
	}
	if err := accountAccess(t, func() error { return os.RemoveAll(half.Root) }); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("a half-open tree was removable before UnlockWithin: %v", err)
	}
	for _, root := range []string{around, half.Root} {
		if err := UnlockWithin(root); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{tree.Root, half.Root} {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("a tree was not removable after UnlockWithin: %v", err)
		}
		if err := os.MkdirAll(root, 0o755); err != nil { // for the cleanup's Unlock
			t.Fatal(err)
		}
	}
	if err := UnlockWithin(filepath.Join(around, "absent")); err != nil {
		t.Fatalf("a missing folder has nothing to unlock: %v", err)
	}
}

// A file another program holds open for writing cannot be opened with read
// sharing only. Verify, Install and Launch report it as unreadable, with the
// sharing violation as the cause, and never as a mismatch; once the file is
// changed instead, the refusal is the mismatch.
func TestALockedFileIsUnreadableAndAChangedFileIsAMismatch(t *testing.T) {
	tree := writeTree(t, smallTree())
	file := filepath.Join(tree.Root, "lib", "ggml.dll")
	writer, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	refusals := map[string]error{
		"verify":  Verify(t.Context(), tree.Name, tree.Root, tree.Manifest),
		"install": NewVerifier(Options{ACL: newFakeACL()}).Install(t.Context(), tree),
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(Options{ACL: newFakeACL()})
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	if writer, err = os.OpenFile(file, os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	}
	_, refusals["launch"] = v.Launch(t.Context(), tree)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for name, err := range refusals {
		var unread *UnreadableError
		if !errors.Is(err, ErrUnreadable) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, ErrMismatch) ||
			!errors.As(err, &unread) || unread.Path != "lib/ggml.dll" || !strings.Contains(err.Error(), "lib/ggml.dll") {
			t.Fatalf("%s of a locked file: %v, want ErrUnreadable with the sharing violation and no mismatch", name, err)
		}
	}
	write(t, file, "GGML")
	err = Verify(t.Context(), tree.Name, tree.Root, tree.Manifest)
	mismatch(t, err, Changed, "lib/ggml.dll")
	if errors.Is(err, ErrUnreadable) {
		t.Fatalf("a changed file is unreadable: %v", err)
	}
}

// accountAccess checks the account ACE alone. Hosted runners may also have
// enabled Administrators rights, which INF-F3 deliberately leaves unrestricted.
// The restricting SID adds a second access check without changing the DACL.
func accountAccess(t *testing.T, try func() error) error {
	t.Helper()
	var current windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &current); err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	restrict := []windows.SIDAndAttributes{{Sid: user.User.Sid}}
	var limited windows.Token
	proc := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	r, _, callErr := proc.Call(uintptr(current), 1, 0, 0, 0, 0,
		1, uintptr(unsafe.Pointer(&restrict[0])), uintptr(unsafe.Pointer(&limited)))
	if r == 0 {
		t.Fatalf("CreateRestrictedToken: %v", callErr)
	}
	defer limited.Close()
	var token windows.Token
	if err := windows.DuplicateTokenEx(limited, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	runtime.LockOSThread()
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			t.Fatalf("RevertToSelf: %v", err)
		}
		runtime.UnlockOSThread()
	}()
	if err := windows.SetThreadToken(nil, token); err != nil {
		t.Fatal(err)
	}
	return try()
}

// An additional enabled group can permit a write even when the account ACE
// is read-only. The denial assertions must exercise the account's rights.
func TestAccountAccessSeparatesTheAccountFromAdditionalGroupGrants(t *testing.T) {
	root := t.TempDir()
	sid, err := AccountSID()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;0x1200a9;;;" + sid + ")(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := SetFolderACL(root, sid, FullControl); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(root, "extra-group.txt")
	if err := os.WriteFile(file, []byte("readable"), 0600); err != nil {
		t.Fatalf("unrestricted extra-group control: %v", err)
	}
	if err := accountAccess(t, func() error { _, err := os.ReadFile(file); return err }); err != nil {
		t.Fatalf("restricted read: %v", err)
	}
	if err := accountAccess(t, func() error { return os.WriteFile(file, []byte("changed"), 0600) }); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("restricted account write: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "readable" {
		t.Fatalf("denied write changed bytes: %q %v", data, err)
	}
	// The caller's thread has reverted and still exercises the extra grant.
	if err := os.WriteFile(file, []byte("reverted"), 0600); err != nil {
		t.Fatalf("after impersonation: %v", err)
	}
}

// The first real move is denied by an exact held descendant. Releasing that
// handle after the refusal permits the setup move without recreating files.
func TestFixtureSetupMoveWaitsForReleasedDescendant(t *testing.T) {
	tree, _, _ := installed(t)
	file := filepath.Join(tree.Root, "engine.exe")
	held, err := openHeld(file, false)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			held.close()
		}
	})
	before, err := held.fileMeta()
	if err != nil {
		t.Fatal(err)
	}
	moved := tree.Root + "-setup"
	calls := 0
	rename := func(from, to string) error {
		calls++
		err := os.Rename(from, to)
		if calls == 1 {
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("held move: %v", err)
			}
			if closeErr := held.close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			released = true
		}
		return err
	}
	if err := moveFixtureBefore(t.Context(), tree.Root, moved, time.Now().Add(time.Second), rename); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("move calls=%d, want held refusal then success", calls)
	}
	afterHandle, err := openHeld(filepath.Join(moved, "engine.exe"), false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := afterHandle.fileMeta()
	afterHandle.close()
	if err != nil {
		t.Fatal(err)
	}
	if before.id != after.id {
		t.Fatal("setup move replaced the filesystem object")
	}
	if data, err := os.ReadFile(filepath.Join(moved, "engine.exe")); err != nil || string(data) != "engine" {
		t.Fatalf("moved bytes=%q err=%v", data, err)
	}
}
func TestFixtureSetupMoveRefusesAnUnreleasedDescendant(t *testing.T) {
	tree, _, _ := installed(t)
	held, err := openHeld(filepath.Join(tree.Root, "engine.exe"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.close()
	moved := tree.Root + "-still-held"
	err = moveFixtureBefore(t.Context(), tree.Root, moved, time.Now().Add(20*time.Millisecond), os.Rename)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("permanent hold: %v", err)
	}
	if _, err := os.Stat(tree.Root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Fatalf("denied move created target: %v", err)
	}
}

// A busy metadata record is a refusal, not a trusted new record. This tests
// the production operation directly; Install and writeAtomic gain no retries.
func TestBusyMetadataRecordRefusesUntilItsExactHolderCloses(t *testing.T) {
	tree, v, _ := installed(t)
	before, err := os.ReadFile(tree.Record)
	if err != nil {
		t.Fatal(err)
	}
	held, err := openHeld(tree.Record, false)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			held.close()
		}
	})
	for _, operation := range []string{"install", "launch"} {
		var err error
		if operation == "install" {
			err = v.Install(t.Context(), tree)
		} else {
			var h *Hold
			h, err = v.Launch(t.Context(), tree)
			if h != nil {
				h.Release()
			}
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			t.Fatalf("busy %s: %v", operation, err)
		}
		after, readErr := os.ReadFile(tree.Record)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("busy record changed: %v", readErr)
		}
		v.mu.Lock()
		_, trusted := v.known[tree.Record]
		v.mu.Unlock()
		if trusted {
			t.Fatalf("busy %s trusted a new record", operation)
		}
	}
	if err := held.close(); err != nil {
		t.Fatal(err)
	}
	released = true
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	if h := launch(t, v, tree); h.Full() {
		t.Fatal("successful install did not establish the record")
	}
}
