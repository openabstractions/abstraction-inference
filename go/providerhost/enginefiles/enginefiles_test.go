package enginefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

// fakeACL records which folders Install restricted; a test opens one to
// stand for a changed ACL.
type fakeACL struct {
	mu         sync.Mutex
	restricted map[string]bool
	opened     map[string]bool
}

func newFakeACL() *fakeACL {
	return &fakeACL{restricted: map[string]bool{}, opened: map[string]bool{}}
}

func (f *fakeACL) Restrict(folders []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, folder := range folders {
		f.restricted[folder] = true
		delete(f.opened, folder)
	}
	return nil
}

func (f *fakeACL) Check(folder string, _ *Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.restricted[folder] || f.opened[folder] {
		return fmt.Errorf("%w: %s", ErrFolderACL, folder)
	}
	return nil
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// smallTree is a miniature engine: a launcher, a library and a nested
// Python file.
func smallTree() map[string]string {
	return map[string]string{
		"engine.exe":            "engine",
		"lib/ggml.dll":          "ggml",
		"lib/python/module.py":  "print(1)\n",
		"lib/python/data/k.dat": "kernel",
	}
}

// writeTree writes files below a fresh folder and returns the tree with its
// manifest and a record path in a separate state folder.
func writeTree(t *testing.T, files map[string]string) Tree {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "engine")
	var m Manifest
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, File{Path: name, Size: int64(len(content)), SHA256: digest(content)})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return Tree{Name: "fixture", Root: root, Manifest: m, Record: filepath.Join(base, "state", "fixture.record")}
}

func installed(t *testing.T) (Tree, *Verifier, *fakeACL) {
	t.Helper()
	tree := writeTree(t, smallTree())
	acl := newFakeACL()
	v := NewVerifier(Options{ACL: acl})
	if err := v.Install(t.Context(), tree); err != nil {
		t.Fatal(err)
	}
	return tree, v, acl
}

func launch(t *testing.T, v *Verifier, tree Tree) *Hold {
	t.Helper()
	h, err := v.Launch(t.Context(), tree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Release() })
	return h
}

func mismatch(t *testing.T, err error, kind, path string) {
	t.Helper()
	var m *MismatchError
	if !errors.As(err, &m) || m.Kind != kind || m.Path != path || !errors.Is(err, ErrMismatch) {
		t.Fatalf("want a %s refusal of %s, got %v", kind, path, err)
	}
}

// INF-F1, INF-F2: the install hashes in full; later launches of the same
// process check metadata only.
func TestInstallHashesAndLaterLaunchesCheckMetadata(t *testing.T) {
	tree, v, _ := installed(t)
	h := launch(t, v, tree)
	if h.Full() {
		t.Fatal("a launch after the install hashed again")
	}
	h.Release()
	if h := launch(t, v, tree); h.Full() {
		t.Fatal("a second launch hashed again")
	}
	data, err := os.ReadFile(tree.Record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "pin\t"+tree.Manifest.Pin()+"\n") || !strings.Contains(string(data), "\nvolume\t") {
		t.Fatalf("record not bound to the pin and volume:\n%s", data)
	}
}

// INF-F1: the first launch of each provider process hashes in full, whatever
// record an earlier process left.
func TestFirstLaunchOfEachProcessHashesInFull(t *testing.T) {
	tree, _, acl := installed(t)
	next := NewVerifier(Options{ACL: acl})
	if h := launch(t, next, tree); !h.Full() {
		t.Fatal("a new process trusted a record it did not write")
	}
}

// INF-F1: a provider killed while its engine ran re-verifies on relaunch.
// The dead process's handles are gone; its record was rewritten to vouch for
// a file changed in place. The relaunched process hashes and refuses.
func TestKilledProviderReverifiesOnRelaunch(t *testing.T) {
	tree, v, acl := installed(t)
	h := launch(t, v, tree)
	h.Release() // the process died; Windows closed its handles
	file := filepath.Join(tree.Root, "lib", "ggml.dll")
	if err := os.WriteFile(file, []byte("GGML"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgeRecord(t, v, tree)
	relaunched := NewVerifier(Options{ACL: acl})
	_, err := relaunched.Launch(t.Context(), tree)
	mismatch(t, err, Changed, "lib/ggml.dll")
}

// forgeRecord rewrites the record to match the tree as it is now, as a
// writer of the provider's state folder could.
func forgeRecord(t *testing.T, v *Verifier, tree Tree) {
	t.Helper()
	tree.Manifest = sorted(tree.Manifest)
	o, err := v.open(t.Context(), tree)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll(o.nodes)
	metas, err := v.metas(t.Context(), tree, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tree.Record, encodeRecord(tree, tree.Manifest.Pin(), o.id, metas), 0o600); err != nil {
		t.Fatal(err)
	}
}

// INF-F2: the process remembers the record it wrote; a record rewritten by
// anyone else triggers the full hash within the same process too.
func TestRewrittenRecordTriggersTheFullHash(t *testing.T) {
	tree, v, _ := installed(t)
	file := filepath.Join(tree.Root, "engine.exe")
	if err := os.WriteFile(file, []byte("ENGINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	forgeRecord(t, v, tree)
	_, err := v.Launch(t.Context(), tree)
	mismatch(t, err, Changed, "engine.exe")
}

// INF-F2: a file of another size after a metadata-only pass.
func TestMetadataCheckCatchesASizeChange(t *testing.T) {
	tree, v, _ := installed(t)
	if h := launch(t, v, tree); h.Full() {
		t.Fatal("expected a metadata-only pass")
	} else {
		h.Release()
	}
	if err := os.WriteFile(filepath.Join(tree.Root, "lib", "ggml.dll"), []byte("ggml-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := v.Launch(t.Context(), tree)
	mismatch(t, err, Changed, "lib/ggml.dll")
}

// INF-F2: a same-size change in place moves the last-write and change times;
// the metadata check sends the launch to the full hash, which refuses.
func TestMetadataCheckCatchesATimeChange(t *testing.T) {
	tree, v, _ := installed(t)
	before := metaOf(t, v, tree, "lib/python/module.py")
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(tree.Root, "lib", "python", "module.py"), []byte("print(2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after := metaOf(t, v, tree, "lib/python/module.py")
	if after.id != before.id || after.size != before.size || after.write == before.write && after.change == before.change {
		t.Fatalf("the fixture must differ in time only: %+v %+v", before, after)
	}
	_, err := v.Launch(t.Context(), tree)
	mismatch(t, err, Changed, "lib/python/module.py")
}

func metaOf(t *testing.T, v *Verifier, tree Tree, rel string) meta {
	t.Helper()
	h, err := openHeld(filepath.Join(tree.Root, filepath.FromSlash(rel)), false)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	m, err := h.fileMeta()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A refused launch forgets the record: after the file is restored, the next
// launch hashes in full again.
func TestAMismatchSendsTheNextLaunchToTheFullHash(t *testing.T) {
	tree, v, _ := installed(t)
	file := filepath.Join(tree.Root, "engine.exe")
	if err := os.WriteFile(file, []byte("ENGINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Launch(t.Context(), tree); err == nil {
		t.Fatal("changed launcher passed")
	}
	if err := os.WriteFile(file, []byte("engine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := launch(t, v, tree); !h.Full() {
		t.Fatal("the launch after a mismatch trusted the record")
	}
}

// INF-F2: the record is bound to the tree's final path; a moved tree is
// hashed in full.
func TestRecordIsBoundToTheTreesPath(t *testing.T) {
	tree, v, _ := installed(t)
	moved := tree
	moved.Root = tree.Root + "-moved"
	if err := moveInstalledFixture(t, tree.Root, moved.Root); err != nil {
		t.Fatal(err)
	}
	acl := newFakeACL()
	v.acl = acl
	if err := acl.Restrict(foldersOf(t, moved.Root)); err != nil {
		t.Fatal(err)
	}
	if h := launch(t, v, moved); !h.Full() {
		t.Fatal("a moved tree was trusted by its old record")
	}
}

func foldersOf(t *testing.T, root string) []string {
	t.Helper()
	var folders []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			folders = append(folders, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return folders
}

// INF-F1: a tree folder that does not exist is a Missing mismatch at ".",
// never an unreadable tree.
func TestAMissingTreeFolderIsAMissingMismatch(t *testing.T) {
	tree := writeTree(t, smallTree())
	tree.Root = filepath.Join(filepath.Dir(tree.Root), "absent")
	refusals := map[string]error{"verify": Verify(t.Context(), tree.Name, tree.Root, tree.Manifest)}
	_, refusals["launch"] = NewVerifier(Options{ACL: newFakeACL()}).Launch(t.Context(), tree)
	for name, err := range refusals {
		mismatch(t, err, Missing, ".")
		if errors.Is(err, ErrUnreadable) {
			t.Fatalf("%s of a missing folder is unreadable: %v", name, err)
		}
	}
}

func TestTreeRefusals(t *testing.T) {
	for _, c := range []struct {
		name, kind, path string
		mutate           func(t *testing.T, root string)
	}{
		{"extra file", Extra, "lib/version.dll", func(t *testing.T, root string) {
			write(t, filepath.Join(root, "lib", "version.dll"), "planted")
		}},
		{"extra folder", Extra, "lib/plugins", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "lib", "plugins"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing file", Missing, "lib/python/data/k.dat", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "lib", "python", "data", "k.dat")); err != nil {
				t.Fatal(err)
			}
		}},
		{"same-size change", Changed, "engine.exe", func(t *testing.T, root string) {
			write(t, filepath.Join(root, "engine.exe"), "ENGINE")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tree := writeTree(t, smallTree())
			c.mutate(t, tree.Root)
			err := Verify(t.Context(), "fixture", tree.Root, tree.Manifest)
			mismatch(t, err, c.kind, c.path)
			v := NewVerifier(Options{ACL: newFakeACL()})
			if err := v.Install(t.Context(), tree); err == nil {
				t.Fatal("install accepted the tree")
			}
		})
	}
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestALinkInTheTreeIsRefused(t *testing.T) {
	tree := writeTree(t, smallTree())
	outside := filepath.Join(filepath.Dir(tree.Root), "outside.dll")
	write(t, outside, "x")
	if err := os.Symlink(outside, filepath.Join(tree.Root, "lib", "link.dll")); err != nil {
		t.Skipf("no symbolic link: %v", err)
	}
	err := Verify(t.Context(), "fixture", tree.Root, tree.Manifest)
	mismatch(t, err, Special, "lib/link.dll")
}

// INF-F3: every launch checks every folder's ACL.
func TestLaunchRefusesAFolderWithAnotherACL(t *testing.T) {
	tree, v, acl := installed(t)
	acl.opened[filepath.Join(tree.Root, "lib", "python")] = true
	_, err := v.Launch(t.Context(), tree)
	if !errors.Is(err, ErrFolderACL) {
		t.Fatalf("a folder with a changed ACL launched: %v", err)
	}
	fresh := writeTree(t, smallTree())
	if _, err := v.Launch(t.Context(), fresh); !errors.Is(err, ErrFolderACL) {
		t.Fatalf("a tree that was never installed launched: %v", err)
	}
}

func TestManifestValidation(t *testing.T) {
	good := File{Path: "a.dll", Size: 1, SHA256: digest("a")}
	for _, m := range []Manifest{
		{},
		{Files: []File{{Path: "../a.dll", Size: 1, SHA256: good.SHA256}}},
		{Files: []File{{Path: "/a.dll", Size: 1, SHA256: good.SHA256}}},
		{Files: []File{{Path: `a\b.dll`, Size: 1, SHA256: good.SHA256}}},
		{Files: []File{{Path: "a.dll", Size: -1, SHA256: good.SHA256}}},
		{Files: []File{{Path: "a.dll", Size: 1, SHA256: strings.ToUpper(good.SHA256)}}},
		{Files: []File{good, good}},
	} {
		if m.Validate() == nil {
			t.Fatalf("manifest %+v accepted", m)
		}
	}
	// A name listed again in another case is the same file where names
	// ignore case (Windows) and another file elsewhere (identity.SamePathName).
	otherCase := Manifest{Files: []File{good, {Path: "A.DLL", Size: 1, SHA256: good.SHA256}}}
	if refused := otherCase.Validate() != nil; refused != identity.SamePathName("a.dll", "A.DLL") {
		t.Fatalf("manifest %+v refused %t; names compare as one: %t", otherCase, refused, identity.SamePathName("a.dll", "A.DLL"))
	}
	if err := (Manifest{Files: []File{good}}).Validate(); err != nil {
		t.Fatal(err)
	}
	a := Manifest{Files: []File{good, {Path: "b.dll", Size: 1, SHA256: digest("b")}}}
	b := Manifest{Files: []File{a.Files[1], a.Files[0]}}
	if a.Pin() != b.Pin() {
		t.Fatal("the pin depends on entry order")
	}
}

// An admission check and a launch of the same tree may run at once; each
// proof holds its own handles and the record is written by one at a time.
func TestConcurrentLaunchesOfOneTree(t *testing.T) {
	tree := writeTree(t, smallTree())
	acl := newFakeACL()
	if err := acl.Restrict(foldersOf(t, tree.Root)); err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(Options{ACL: acl})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- v.Check(t.Context(), tree)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h := launch(t, v, tree); h.Full() {
		t.Fatal("the record of concurrent proofs was not trusted")
	}
}

func TestTheRecordMustLieOutsideTheTree(t *testing.T) {
	tree := writeTree(t, smallTree())
	tree.Record = filepath.Join(tree.Root, "lib", "fixture.record")
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).Launch(t.Context(), tree); err == nil {
		t.Fatal("a record inside the tree was accepted")
	}
}

// Moving the installed fixture establishes the changed-path scenario; it is
// not a held-object denial assertion. Windows can briefly refuse this setup
// move while a descendant is open elsewhere. Keep the original objects and
// fail if sharing-compatible refusal persists within the small setup budget.
func moveInstalledFixture(t *testing.T, from, to string) error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	if end, ok := t.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	return moveFixtureBefore(t.Context(), from, to, deadline, os.Rename)
}
func moveFixtureBefore(ctx context.Context, from, to string, deadline time.Time, rename func(string, string) error) error {
	for {
		err := rename(from, to)
		if err == nil || runtime.GOOS != "windows" || (!errors.Is(err, syscall.Errno(5)) && !errors.Is(err, syscall.Errno(32))) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		delay := 5 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func TestFixtureSetupMoveReturnsOtherErrorsImmediately(t *testing.T) {
	sentinel := errors.New("unrelated failure")
	calls := 0
	err := moveFixtureBefore(t.Context(), "from", "to", time.Now().Add(time.Second), func(string, string) error { calls++; return sentinel })
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("unrelated error=%v calls=%d", err, calls)
	}
}
