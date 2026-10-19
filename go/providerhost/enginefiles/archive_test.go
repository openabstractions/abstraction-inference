package enginefiles

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

// zipEntry is one archive entry; dir marks a folder entry.
type zipEntry struct {
	name, content string
	dir           bool
}

var archiveTime = time.Date(2026, 9, 6, 13, 22, 26, 0, time.UTC)

// writeArchive writes a ZIP of entries and returns its pin.
func writeArchive(t *testing.T, entries ...zipEntry) PinnedArchive {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: archiveTime}
		if e.dir {
			h.SetMode(0o755 | os.ModeDir)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if !e.dir {
			if _, err := f.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "engine.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return PinnedArchive{Path: path, Size: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:])}
}

// llamaLike is a flat archive of kept files and two the review excludes.
var llamaLike = []zipEntry{
	{name: "llama-server.exe", content: "server"},
	{name: "ggml.dll", content: "ggml"},
	{name: "llama-cli.exe", content: "cli"},
	{name: "ggml-rpc-server.exe", content: "rpc"},
}

func pinnedFiles(files map[string]string) Manifest {
	var m Manifest
	for name, content := range files {
		m.Files = append(m.Files, File{Path: name, Size: int64(len(content)), SHA256: digest(content)})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m
}

var llamaKept = map[string]string{"llama-server.exe": "server", "ggml.dll": "ggml", "version.txt": "b1\n"}

func llamaInstall(t *testing.T, a PinnedArchive) (ArchiveInstall, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "bin", "vulkan")
	return ArchiveInstall{
		Archive:  a,
		Tree:     Tree{Name: "llama", Root: root, Manifest: pinnedFiles(llamaKept), Record: filepath.Join(base, "state", "llama.files")},
		Excluded: []string{"llama-cli.exe", "ggml-rpc-server.exe"},
		Extra:    map[string][]byte{"version.txt": []byte("b1\n")},
	}, root
}

func names(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return strings.Join(out, ",")
}

func absent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", p, err)
		}
	}
}

// INF-F6: the kept entries are chosen by pinned name, the excluded ones are
// never written, the provider's extra file joins them, and the tree is
// installed with the read-only ACL and its record. Files keep the archive's
// modification time.
func TestInstallFromArchiveWritesOnlyThePinnedFiles(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	acl := newFakeACL()
	v := NewVerifier(Options{ACL: acl})
	var last, total int64
	in.Progress = func(done, all int64) error { last, total = done, all; return nil }
	m, err := v.InstallFromArchive(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Pin() != in.Tree.Manifest.Pin() {
		t.Fatal("the returned manifest is not the installed one")
	}
	if got := names(t, root); got != "ggml.dll,llama-server.exe,version.txt" {
		t.Fatalf("installed %s", got)
	}
	if !acl.restricted[root] {
		t.Fatal("the installed folder has no read-only ACL")
	}
	if _, err := os.Stat(in.Tree.Record); err != nil {
		t.Fatalf("no record: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "ggml.dll")); err != nil || !info.ModTime().Equal(archiveTime) {
		t.Fatalf("modification time not kept: %v", err)
	}
	if want := a.Size + int64(len("server")+len("ggml")); last != want || total != want {
		t.Fatalf("progress %d of %d, wanted %d", last, total, want)
	}
	absent(t, root+".partial", root+".previous")
	if h, err := v.Launch(t.Context(), in.Tree); err != nil {
		t.Fatalf("the installed tree does not launch: %v", err)
	} else {
		h.Release()
	}
}

// INF-F6: an archive other than the pinned one, an entry outside the pin,
// a missing, resized or changed entry, a duplicate, an unsafe name or a
// link refuses the install and leaves no folder, stage or backup.
func TestInstallFromArchiveRefusals(t *testing.T) {
	good := writeArchive(t, llamaLike...)
	// Names compare as the file system compares them (INF-F1,
	// identity.SamePathName): GGML.dll is ggml.dll a second time where names
	// ignore case (Windows) and a name outside the pin elsewhere.
	otherCase := "outside the pin"
	if identity.SamePathName("GGML.dll", "ggml.dll") {
		otherCase = "twice"
	}
	for name, c := range map[string]struct {
		archive PinnedArchive
		change  func(*ArchiveInstall)
		says    string
	}{
		"another digest":   {good, func(in *ArchiveInstall) { in.Archive.SHA256 = strings.Repeat("0", 64) }, "is not the pinned"},
		"another size":     {good, func(in *ArchiveInstall) { in.Archive.Size++ }, "is not the pinned"},
		"entry outside":    {writeArchive(t, append(llamaLike, zipEntry{name: "version.dll", content: "planted"})...), nil, "outside the pin"},
		"extra in archive": {writeArchive(t, append(llamaLike, zipEntry{name: "version.txt", content: "b1\n"})...), nil, "outside the pin"},
		"missing entry":    {writeArchive(t, llamaLike[1:]...), nil, "holds no llama-server.exe"},
		"changed entry":    {writeArchive(t, append([]zipEntry{{name: "llama-server.exe", content: "SERVER"}}, llamaLike[1:]...)...), nil, "wanted"},
		"resized entry":    {writeArchive(t, append([]zipEntry{{name: "llama-server.exe", content: "server!"}}, llamaLike[1:]...)...), nil, "wanted 6"},
		"duplicate":        {writeArchive(t, append(llamaLike, zipEntry{name: "ggml.dll", content: "ggml"})...), nil, "twice"},
		"other case":       {writeArchive(t, append(llamaLike, zipEntry{name: "GGML.dll", content: "ggml"})...), nil, otherCase},
		"no derive or pin": {good, func(in *ArchiveInstall) { in.Tree.Manifest = Manifest{}; in.Extra = nil }, "a manifest or Derive"},
		"extra not pinned": {good, func(in *ArchiveInstall) { in.Extra["version.txt"] = []byte("b2\n") }, "extra file"},
		"relative archive": {good, func(in *ArchiveInstall) { in.Archive.Path = "engine.zip" }, "absolute"},
	} {
		t.Run(name, func(t *testing.T) {
			in, root := llamaInstall(t, c.archive)
			if c.change != nil {
				c.change(&in)
			}
			_, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), in)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("%v, wanted a refusal saying %q", err, c.says)
			}
			absent(t, root, root+".partial", root+".previous")
		})
	}
	for _, bad := range []string{"../evil.dll", "./../evil.dll", "/abs.dll", "a/../../evil.dll", `a\..\evil.dll`, "C:/evil.dll",
		"a:stream", "NUL", "com1.dll", "Lib/aux", "Lib/Lpt9.tar.gz", "./con", "a /b", "a./b", "a\x01b"} {
		t.Run("name "+bad, func(t *testing.T) {
			a := writeArchive(t, zipEntry{name: bad, content: "x"})
			base := t.TempDir()
			root := filepath.Join(base, "e")
			_, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), ArchiveInstall{Archive: a,
				Tree: Tree{Root: root, Record: filepath.Join(base, "e.files")}, Derive: func(Manifest) error { return nil }})
			if !errors.Is(err, ErrArchive) {
				t.Fatalf("entry %q: %v", bad, err)
			}
			absent(t, root, filepath.Join(base, "evil.dll"), root+".partial")
		})
	}
}

// A non-empty folder that proves against the pin is kept without reading
// the archive; one that does not is refused, or replaced under Replace with
// the old folder set aside as .previous until the new tree is installed.
func TestInstallFromArchiveKeepsOrReplacesAnExistingFolder(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	v := NewVerifier(Options{ACL: newFakeACL()})
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	kept := in
	kept.Archive.Path = filepath.Join(t.TempDir(), "absent.zip")
	if _, err := v.InstallFromArchive(t.Context(), kept); err != nil {
		t.Fatalf("a proving folder was not kept: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("b0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.InstallFromArchive(t.Context(), in); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("another build was replaced without Replace: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "version.txt")); string(data) != "b0\n" {
		t.Fatal("a refused install changed the folder")
	}
	in.Replace = true
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "version.txt")); string(data) != "b1\n" {
		t.Fatal("the folder was not replaced")
	}
	absent(t, root+".previous", root+".partial")
}

// failingACL refuses to restrict, so Install fails after the new tree moved
// into place.
type failingACL struct{}

func (failingACL) Restrict([]string) error     { return errors.New("restrict refused") }
func (failingACL) Check(string, *Handle) error { return nil }

// A replacement whose install fails returns the old folder to its place,
// and a .previous left by a killed replacement refuses the next install.
func TestInstallFromArchiveRollsBackAFailedReplacement(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("b0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in.Replace = true
	if _, err := NewVerifier(Options{ACL: failingACL{}}).InstallFromArchive(t.Context(), in); err == nil || !strings.Contains(err.Error(), "restrict refused") {
		t.Fatalf("a failed install was reported as %v", err)
	}
	if got := names(t, root); got != "version.txt" {
		t.Fatalf("the old folder was not restored: %s", got)
	}
	absent(t, root+".previous", root+".partial")
	if err := os.Mkdir(root+".previous", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), in); !errors.Is(err, ErrArchive) || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("a leftover .previous did not refuse: %v", err)
	}
}

// A derived install takes every file entry under the prefix, skips folder
// entries, hands the recorded manifest to Derive before anything is
// published, and keeps a proving existing folder.
func TestInstallFromArchiveDerivesTheManifest(t *testing.T) {
	a := writeArchive(t,
		zipEntry{name: "./", dir: true}, zipEntry{name: "./Lib/", dir: true},
		zipEntry{name: "./thenoise.bat", content: "@echo off\r\n"},
		zipEntry{name: "./Lib/a.py", content: "print(1)\n"})
	base := t.TempDir()
	root := filepath.Join(base, "e")
	want := pinnedFiles(map[string]string{"thenoise.bat": "@echo off\r\n", "Lib/a.py": "print(1)\n"})
	in := ArchiveInstall{Archive: a, Prefix: "./", Tree: Tree{Root: root, Record: filepath.Join(base, "e.files")}}
	refused := errors.New("not the pinned manifest")
	in.Derive = func(m Manifest) error { return refused }
	v := NewVerifier(Options{ACL: newFakeACL()})
	if _, err := v.InstallFromArchive(t.Context(), in); !errors.Is(err, refused) {
		t.Fatalf("Derive's refusal: %v", err)
	}
	absent(t, root, root+".partial")
	var seen Manifest
	in.Derive = func(m Manifest) error { seen = m; return nil }
	m, err := v.InstallFromArchive(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Pin() != want.Pin() || seen.Pin() != want.Pin() {
		t.Fatalf("derived %v, wanted %v", m.Files, want.Files)
	}
	if data, err := os.ReadFile(filepath.Join(root, "Lib", "a.py")); err != nil || string(data) != "print(1)\n" {
		t.Fatalf("Lib/a.py: %q %v", data, err)
	}
	if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
		t.Fatalf("a proving folder was not kept: %v", err)
	}
	got, err := ArchiveManifest(t.Context(), a, "./")
	if err != nil || got.Pin() != want.Pin() {
		t.Fatalf("ArchiveManifest: %v %v", got.Files, err)
	}
	// A derived install replaces no folder: Replace is refused, and a folder
	// that does not prove is refused as it is.
	replace := in
	replace.Replace = true
	if _, err := v.InstallFromArchive(t.Context(), replace); err == nil || !strings.Contains(err.Error(), "replaces no folder") {
		t.Fatalf("a derived install with Replace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := v.InstallFromArchive(t.Context(), in); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a derived install over a stray file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "stray.txt")); err != nil {
		t.Fatal("a refused derived install changed the folder")
	}
}

// Progress names the archive's size as the total while the archive is
// hashed, before the entries are planned.
func TestProgressHasATotalWhileTheArchiveIsHashed(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, _ := llamaInstall(t, a)
	in.Archive.SHA256 = strings.Repeat("0", 64)
	var total int64 = -1
	in.Progress = func(done, all int64) error { total = all; return nil }
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), in); err == nil {
		t.Fatal("another digest was accepted")
	}
	if total != a.Size {
		t.Fatalf("total %d while hashing, wanted %d", total, a.Size)
	}
}

// A caller's stage must be an empty folder beside the tree, and is removed
// after the install.
func TestInstallFromArchiveUsesTheCallersStage(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".vulkan-stage-")
	if err != nil {
		t.Fatal(err)
	}
	in.Stage = stage
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	absent(t, stage)
	other, root2 := llamaInstall(t, a)
	other.Stage = t.TempDir()
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(t.Context(), other); err == nil || !strings.Contains(err.Error(), "beside") {
		t.Fatalf("a stage elsewhere was used: %v", err)
	}
	absent(t, root2)
}

// Unlock returns full control of root and its subfolders: the fake forgets
// that it restricted them.
func (f *fakeACL) Unlock(root string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for folder := range f.restricted {
		if folder == root || strings.HasPrefix(folder, root+string(filepath.Separator)) {
			delete(f.restricted, folder)
		}
	}
	return nil
}

// replacing is an install of a build whose version.txt differs from the
// installed one, with Replace.
func replacing(in ArchiveInstall, version string) ArchiveInstall {
	files := map[string]string{}
	for k, v := range llamaKept {
		files[k] = v
	}
	files["version.txt"] = version
	in.Tree.Manifest = pinnedFiles(files)
	in.Extra = map[string][]byte{"version.txt": []byte(version)}
	in.Replace = true
	return in
}

// failRename makes rename fail for every move whose source match picks.
func failRename(t *testing.T, match func(src, dst string) bool) {
	t.Helper()
	real := rename
	t.Cleanup(func() { rename = real })
	rename = func(src, dst string) error {
		if match(src, dst) {
			return errors.New("rename refused by the test")
		}
		return real(src, dst)
	}
}

// INF-F3, INF-F6: a replacement that fails after the old tree was unlocked
// leaves the old tree with its read-only folder ACL, whether the old tree
// could not move aside or the new one could not move in, and the old tree
// launches again.
func TestAFailedReplacementKeepsTheTreeReadOnly(t *testing.T) {
	for name, match := range map[string]func(src, dst string) bool{
		"old tree stays":     func(src, dst string) bool { return strings.HasSuffix(dst, ".previous") },
		"new tree stays out": func(src, dst string) bool { return strings.HasSuffix(src, ".partial") },
	} {
		t.Run(name, func(t *testing.T) {
			a := writeArchive(t, llamaLike...)
			in, root := llamaInstall(t, a)
			acl := newFakeACL()
			v := NewVerifier(Options{ACL: acl})
			if _, err := v.InstallFromArchive(t.Context(), in); err != nil {
				t.Fatal(err)
			}
			failRename(t, match)
			if _, err := v.InstallFromArchive(t.Context(), replacing(in, "b2\n")); err == nil {
				t.Fatal("a failed replacement reported success")
			}
			if !acl.restricted[root] {
				t.Fatal("the old tree lost its read-only folder ACL")
			}
			absent(t, root+".previous", root+".partial")
			h, err := v.Launch(t.Context(), in.Tree)
			if err != nil {
				t.Fatalf("the old tree does not launch: %v", err)
			}
			h.Release()
		})
	}
}

// Two installs of one tree through one verifier run one after the other:
// the second waits while the first extracts into its stage, then keeps the
// tree the first installed. Neither destroys the other's stage.
func TestConcurrentInstallsOfOneTreeAreSerialised(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	v := NewVerifier(Options{ACL: newFakeACL()})
	extracting, release := make(chan struct{}), make(chan struct{})
	first := in
	var once sync.Once
	first.Progress = func(done, total int64) error {
		if done > a.Size {
			once.Do(func() { close(extracting); <-release })
		}
		return nil
	}
	errs := make(chan error, 2)
	go func() { _, err := v.InstallFromArchive(context.Background(), first); errs <- err }()
	<-extracting
	go func() { _, err := v.InstallFromArchive(context.Background(), in); errs <- err }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a concurrent install failed: %v", err)
		}
	}
	if got := names(t, root); got != "ggml.dll,llama-server.exe,version.txt" {
		t.Fatalf("installed %s", got)
	}
	absent(t, root+".partial", root+".previous")
}

// Cancelling stops the install and leaves nothing behind.
func TestInstallFromArchiveStopsWhenCancelled(t *testing.T) {
	a := writeArchive(t, llamaLike...)
	in, root := llamaInstall(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled install: %v", err)
	}
	absent(t, root, root+".partial")
}
