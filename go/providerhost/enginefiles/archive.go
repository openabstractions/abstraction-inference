package enginefiles

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

// A PinnedArchive is a publisher's ZIP or gzip-compressed tar archive held
// locally, pinned by the size and SHA-256 its review recorded.
type PinnedArchive struct {
	Path   string // absolute
	Size   int64
	SHA256 string // lower-case hex
	// Name is the publisher's asset name refusals give; empty gives the
	// file's own name.
	Name string
}

func (a PinnedArchive) name() string {
	if a.Name != "" {
		return a.Name
	}
	return filepath.Base(a.Path)
}

// ArchiveInstall is one install of an engine tree from a pinned archive
// (inference CONTRACT.md INF-F6).
type ArchiveInstall struct {
	Archive PinnedArchive
	// Tree is the tree to install. Its Manifest pins every file the archive
	// gives it, by name, and every file Extra gives it. With Derive set the
	// Manifest is empty and recorded from the archive instead.
	Tree Tree
	// Prefix is put before a tree path to name its archive entry; TheNoise's
	// ZIP names every entry "./<path>".
	Prefix string
	// Excluded are archive entries, without Prefix, that the review keeps
	// out of the tree; they are never read. Every other archive file entry
	// must be a manifest file.
	Excluded []string
	// Extra are files the provider writes into the tree beside the
	// archive's, by tree path, such as Lemonade's version.txt. Each must be a
	// manifest file whose size and SHA-256 its bytes have, and no archive
	// entry.
	Extra map[string][]byte
	// Derive, set in place of a Manifest, takes every archive file entry
	// outside Excluded and receives the manifest recorded from them before
	// anything replaces the tree's folder; an error refuses the install.
	// The provider pins that manifest, as TheNoise pins its digest.
	Derive func(Manifest) error
	// Stage is an empty folder beside Root, on its volume, the archive is
	// extracted into; the install removes it whether it succeeds or not.
	// Empty selects Root+".partial", which the install first removes when an
	// earlier install was killed.
	Stage string
	// Replace lets the install replace a non-empty folder that does not
	// prove against the manifest. Without it such a folder is refused.
	Replace bool
	// Progress, when set, receives the bytes read from the archive and
	// written to the tree so far, and their total.
	Progress func(done, total int64) error
}

// ErrArchive is wrapped by every refusal of an archive or its entries.
var ErrArchive = errors.New("enginefiles: archive refused")

func archiveRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrArchive, fmt.Sprintf(format, args...))
}

// InstallFromArchive installs in.Tree from in.Archive and returns the
// manifest it installed (INF-F6):
//
//  1. A non-empty tree folder that proves against a pinned manifest is kept
//     and installed as it is (Install). One that does not prove is refused,
//     or replaced under Replace.
//  2. The archive is held without write sharing (on Windows) and its size
//     and SHA-256 must equal the pin before any entry is read.
//  3. Each kept entry is chosen by its pinned name, opened with O_EXCL in
//     the stage folder, read through a reader bounded by its pinned size and
//     refused unless its size and SHA-256 equal the pin; an entry the
//     manifest does not name and Excluded does not list refuses the
//     install. Each written file keeps the entry's modification time.
//  4. The proven stage replaces the tree's folder. An existing folder that
//     a running engine holds (INF-F4) is refused before anything changes.
//     Otherwise it is unlocked and renamed to Root+".previous"; it returns,
//     with its read-only folder ACL given again, when a rename or the
//     install fails, and is removed once the new tree is installed. A
//     leftover ".previous" refuses the install: an earlier replacement was
//     killed and the folder needs a person. A derived install replaces no
//     folder.
//  5. Install hashes the new tree in full, gives its folders the read-only
//     ACL and writes the record (INF-F1, INF-F3).
//
// Installs of one tree folder through one Verifier run one at a time.
func (v *Verifier) InstallFromArchive(ctx context.Context, in ArchiveInstall) (Manifest, error) {
	t := in.Tree
	if !filepath.IsAbs(in.Archive.Path) || !filepath.IsAbs(t.Root) || !filepath.IsAbs(t.Record) {
		return Manifest{}, errors.New("enginefiles: the archive, the tree folder and the record must be absolute paths")
	}
	if in.Archive.Size <= 0 || !lowerHex64(in.Archive.SHA256) {
		return Manifest{}, errors.New("enginefiles: the archive pin needs a size and a lower-case SHA-256")
	}
	if t.Name == "" {
		t.Name = filepath.Base(t.Root)
	}
	derive := in.Derive != nil
	if derive == (len(t.Manifest.Files) != 0) {
		return Manifest{}, errors.New("enginefiles: an archive install takes a manifest or Derive, and not both")
	}
	if derive && (len(in.Extra) != 0 || in.Replace) {
		return Manifest{}, errors.New("enginefiles: a derived manifest takes no extra files and replaces no folder")
	}
	if !derive {
		if err := t.validate(); err != nil {
			return Manifest{}, err
		}
		if err := checkExtra(t.Manifest, in.Extra); err != nil {
			return Manifest{}, err
		}
	}
	root := filepath.Clean(t.Root)
	// One install of a tree at a time in this provider process: a second
	// one would remove the first one's stage.
	defer v.installLock(root)()
	backup := root + ".previous"
	if _, err := os.Lstat(backup); err == nil {
		return Manifest{}, archiveRefusal("%s remains from a replacement that did not finish; restore it as %s or remove it", backup, root)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, err
	}
	state, err := folderState(root)
	if err != nil {
		return Manifest{}, err
	}
	// A pinned manifest proves an existing folder before the archive is
	// read; a derived one is recorded from the verified archive first.
	if state == folderFull && !derive {
		err := v.Install(ctx, t)
		if err == nil {
			return t.Manifest, nil
		}
		if !in.Replace || !errors.Is(err, ErrMismatch) {
			return Manifest{}, fmt.Errorf("enginefiles: %s: the folder %s is not empty and does not prove against its manifest: %w", t.Name, root, err)
		}
	}
	file, err := openArchive(in.Archive.Path)
	if err != nil {
		return Manifest{}, fmt.Errorf("enginefiles: opening the archive: %w", err)
	}
	defer file.Close()
	// The total is the archive alone until the plan names the entries.
	p := &progress{report: in.Progress, total: in.Archive.Size}
	reader, err := verifiedArchive(ctx, file, in.Archive, p)
	if err != nil {
		return Manifest{}, err
	}
	plan, err := planEntries(reader, in, t.Manifest)
	if err != nil {
		return Manifest{}, err
	}
	p.total = in.Archive.Size + plan.bytes
	if state == folderFull && derive {
		// A derived install keeps a folder that proves and refuses any
		// other; it never replaces one.
		m, err := extract(ctx, plan, "", p)
		if err == nil {
			err = in.Derive(m)
		}
		if err != nil {
			return Manifest{}, err
		}
		t.Manifest = m
		if err := v.Install(ctx, t); err != nil {
			return Manifest{}, fmt.Errorf("enginefiles: %s: the folder %s is not empty and does not prove against its manifest: %w", t.Name, root, err)
		}
		return m, nil
	}
	stage, err := makeStage(root, in.Stage)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		if stage != "" {
			//unchecked: best-effort removal of the install's own stage after a refusal the caller reports
			_ = os.RemoveAll(stage)
		}
	}()
	m, err := extract(ctx, plan, stage, p)
	if err != nil {
		return Manifest{}, err
	}
	for name, data := range in.Extra {
		if err := writeExclusive(filepath.Join(stage, filepath.FromSlash(name)), data); err != nil {
			return Manifest{}, err
		}
	}
	if derive {
		if err := m.Validate(); err != nil {
			return Manifest{}, err
		}
		if err := in.Derive(m); err != nil {
			return Manifest{}, err
		}
		t.Manifest = m
		if err := t.validate(); err != nil {
			return Manifest{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	old, err := v.setAside(root, backup, state)
	if err != nil {
		return Manifest{}, err
	}
	if err := rename(stage, root); err != nil {
		return Manifest{}, errors.Join(err, v.restore(root, backup, old))
	}
	stage = ""
	if err := v.Install(ctx, t); err != nil {
		//unchecked: the refused tree is the install's own; removal is best effort before the old folder returns
		_ = v.unlock(root)
		return Manifest{}, errors.Join(err, os.RemoveAll(root), v.restore(root, backup, old))
	}
	if old {
		//unchecked: the replaced folder was unlocked before it moved; a failed removal leaves .previous, which the next install names
		_ = os.RemoveAll(backup)
	}
	return t.Manifest, nil
}

// ArchiveManifest verifies a pinned archive and records the manifest of its
// file entries outside excluded, each path without prefix. It writes
// nothing; a review that pins a manifest records it with this.
func ArchiveManifest(ctx context.Context, a PinnedArchive, prefix string, excluded ...string) (Manifest, error) {
	if !filepath.IsAbs(a.Path) || a.Size <= 0 || !lowerHex64(a.SHA256) {
		return Manifest{}, errors.New("enginefiles: the archive pin needs an absolute path, a size and a lower-case SHA-256")
	}
	file, err := openArchive(a.Path)
	if err != nil {
		return Manifest{}, fmt.Errorf("enginefiles: opening the archive: %w", err)
	}
	defer file.Close()
	p := &progress{}
	reader, err := verifiedArchive(ctx, file, a, p)
	if err != nil {
		return Manifest{}, err
	}
	plan, err := planEntries(reader, ArchiveInstall{Prefix: prefix, Excluded: excluded, Derive: func(Manifest) error { return nil }}, Manifest{})
	if err != nil {
		return Manifest{}, err
	}
	m, err := extract(ctx, plan, "", p)
	if err != nil {
		return Manifest{}, err
	}
	return m, m.Validate()
}

// checkExtra refuses an extra file the manifest does not pin as written.
func checkExtra(m Manifest, extra map[string][]byte) error {
	pinned := make(map[string]File, len(m.Files))
	for _, f := range m.Files {
		pinned[f.Path] = f
	}
	for name, data := range extra {
		f, ok := pinned[name]
		sum := sha256.Sum256(data)
		if !ok || f.Size != int64(len(data)) || f.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("enginefiles: the extra file %s is not the manifest's", name)
		}
	}
	return nil
}

type folder int

const (
	folderAbsent folder = iota
	folderEmpty
	folderFull
)

func folderState(root string) (folder, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return folderAbsent, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("enginefiles: %s is not a folder", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return folderEmpty, nil
	}
	return folderFull, nil
}

// makeStage returns the empty stage folder, making the default one.
func makeStage(root, stage string) (string, error) {
	if stage != "" {
		stage = filepath.Clean(stage)
		if filepath.Dir(stage) != filepath.Dir(root) {
			return "", fmt.Errorf("enginefiles: the stage %s is not beside %s", stage, root)
		}
		entries, err := os.ReadDir(stage)
		if err != nil {
			return "", err
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("enginefiles: the stage %s is not empty", stage)
		}
		return stage, nil
	}
	stage = root + ".partial"
	//unchecked: a stage a killed install left may hold read-only folders; removal below reports what remains
	_ = Unlock(stage)
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(stage, 0o755); err != nil {
		return "", err
	}
	return stage, nil
}

// setAside moves a non-empty folder to backup, unlocked so it can be
// removed later, and removes an empty one. It reports whether a folder was
// set aside.
func (v *Verifier) setAside(root, backup string, state folder) (bool, error) {
	switch state {
	case folderAbsent:
		return false, os.MkdirAll(filepath.Dir(root), 0o755)
	case folderEmpty:
		return false, os.Remove(root)
	}
	// A tree a running engine holds (INF-F4) cannot move; it is refused
	// before its folder ACL changes.
	if err := heldElsewhere(root); err != nil {
		return false, archiveRefusal("%s is in use, by a running engine or another process; stop it before replacing the tree: %v", root, err)
	}
	// An installed tree's folders are read-only (INF-F3); the replaced one
	// must become removable.
	if err := v.unlock(root); err != nil {
		return false, errors.Join(err, v.relock(root))
	}
	if err := rename(root, backup); err != nil {
		return false, errors.Join(err, v.relock(root))
	}
	return true, nil
}

// restore returns a folder set aside to its place with the read-only folder
// ACL it had.
func (v *Verifier) restore(root, backup string, old bool) error {
	if !old {
		return nil
	}
	if err := rename(backup, root); err != nil {
		return err
	}
	return v.relock(root)
}

// relock gives every folder of root the read-only folder ACL again, after
// an unlock whose replacement did not happen.
func (v *Verifier) relock(root string) error {
	var folders []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			folders = append(folders, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("enginefiles: restoring the read-only folder ACL of %s: %w", root, err)
	}
	if err := v.acl.Restrict(folders); err != nil {
		return fmt.Errorf("enginefiles: restoring the read-only folder ACL of %s: %w", root, err)
	}
	return nil
}

// installLock serialises archive installs of one tree folder and returns the
// unlock. The folder is keyed by identity.PlaceKey, the file identity of its
// nearest existing parent and its name below it, so an install that creates
// the folder and one that finds it share one lock.
func (v *Verifier) installLock(root string) func() {
	key, err := identity.PlaceKey(root)
	if err != nil {
		key = "path:" + filepath.Clean(root)
	}
	v.mu.Lock()
	l, ok := v.installs[key]
	if !ok {
		l = &sync.Mutex{}
		v.installs[key] = l
	}
	v.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// rename is os.Rename; tests replace it to make a rename fail.
var rename = os.Rename

// unlocker is an ACL that also returns full control of a tree's folders to
// the account. Tests observe the unlock through theirs; an ACL without it,
// such as SystemACL, is unlocked through Unlock.
type unlocker interface{ Unlock(root string) error }

// unlock returns full control of root's folders to the account through the
// verifier's ACL when it can, and through Unlock otherwise.
func (v *Verifier) unlock(root string) error {
	if u, ok := v.acl.(unlocker); ok {
		return u.Unlock(root)
	}
	return Unlock(root)
}

// archiveEntry is one entry of a ZIP or tar archive: its name, what it is,
// its size and modification time, and where to read it.
type archiveEntry struct {
	name     string
	dir      bool
	regular  bool
	size     int64
	modified time.Time
	zip      *zip.File // a ZIP entry
	index    int       // a tar entry's position in the stream
}

// openedArchive is a verified archive and its entries in archive order.
type openedArchive struct {
	file    *os.File
	size    int64
	tarball bool
	entries []archiveEntry
}

// entryPlan is the archive's file entries that the install reads, each
// with the manifest file it must equal (zero in a derived install).
type entryPlan struct {
	archive *openedArchive
	entries []archiveEntry
	files   []File
	derive  bool
	bytes   int64
}

func planEntries(r *openedArchive, in ArchiveInstall, m Manifest) (entryPlan, error) {
	excluded := make(map[string]bool, len(in.Excluded))
	for _, name := range in.Excluded {
		excluded[nameKey(name)] = true
	}
	pinned := make(map[string]File, len(m.Files))
	for _, f := range m.Files {
		if _, extra := in.Extra[f.Path]; !extra {
			pinned[nameKey(f.Path)] = f
		}
	}
	plan := entryPlan{archive: r, derive: in.Derive != nil}
	seen := map[string]bool{}
	for _, e := range r.entries {
		name, ok := strings.CutPrefix(e.name, in.Prefix)
		if !ok {
			return entryPlan{}, archiveRefusal("entry %q lacks the prefix %q", e.name, in.Prefix)
		}
		if name == "" && e.dir {
			continue
		}
		if err := checkEntryName(name); err != nil {
			return entryPlan{}, err
		}
		if e.dir || strings.HasSuffix(name, "/") {
			// A folder entry writes nothing; folders come from file paths.
			continue
		}
		if !e.regular {
			return entryPlan{}, archiveRefusal("entry %q is not a regular file", e.name)
		}
		key := nameKey(name)
		if seen[key] {
			return entryPlan{}, archiveRefusal("entry %q occurs twice", e.name)
		}
		seen[key] = true
		if excluded[key] {
			continue
		}
		var want File
		if plan.derive {
			want = File{Path: name, Size: e.size}
			if want.Size < 0 {
				return entryPlan{}, archiveRefusal("entry %q has no size", e.name)
			}
		} else {
			f, ok := pinned[key]
			if !ok {
				return entryPlan{}, archiveRefusal("entry %q is outside the pin", e.name)
			}
			if e.size != f.Size {
				return entryPlan{}, archiveRefusal("%s is %d bytes in the archive, wanted %d", f.Path, e.size, f.Size)
			}
			want = f
			delete(pinned, key)
		}
		plan.entries = append(plan.entries, e)
		plan.files = append(plan.files, want)
		plan.bytes += want.Size
	}
	for _, f := range pinned {
		return entryPlan{}, archiveRefusal("the archive holds no %s", f.Path)
	}
	return plan, nil
}

// extract reads every planned entry through a reader bounded by its size,
// hashing it and, with a stage, writing it there with O_EXCL. A pinned entry
// must equal its manifest file; a derived one is recorded.
func extract(ctx context.Context, plan entryPlan, stage string, p *progress) (Manifest, error) {
	var m Manifest
	add := func(i int, src io.Reader) error {
		want := plan.files[i]
		got, err := extractOne(ctx, src, plan.entries[i].modified, want, stage, p)
		if err != nil {
			return err
		}
		if !plan.derive && got != want.SHA256 {
			return archiveRefusal("%s in the archive has SHA-256 %s, wanted %s", want.Path, got, want.SHA256)
		}
		want.SHA256 = got
		m.Files = append(m.Files, want)
		return nil
	}
	if plan.archive != nil && plan.archive.tarball {
		// A tar is read in one pass: each planned entry when its turn comes.
		next := 0
		err := walkTar(plan.archive.file, plan.archive.size, func(index int, _ *tar.Header, body io.Reader) error {
			if next < len(plan.entries) && plan.entries[next].index == index {
				next++
				return add(next-1, body)
			}
			return nil
		})
		if err != nil {
			return Manifest{}, err
		}
		if next != len(plan.entries) {
			return Manifest{}, archiveRefusal("the archive ended before every planned entry")
		}
	} else {
		for i, e := range plan.entries {
			src, err := e.zip.Open()
			if err != nil {
				return Manifest{}, archiveRefusal("reading %s: %v", plan.files[i].Path, err)
			}
			err = add(i, src)
			//unchecked: a ZIP entry reader's Close reports nothing the bounded read did not
			_ = src.Close()
			if err != nil {
				return Manifest{}, err
			}
		}
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	return m, nil
}

func extractOne(ctx context.Context, src io.Reader, modified time.Time, want File, stage string, p *progress) (string, error) {
	h := sha256.New()
	var sink io.Writer = h
	var out *os.File
	target := ""
	if stage != "" {
		target = filepath.Join(stage, filepath.FromSlash(want.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		var err error
		if out, err = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err != nil {
			return "", err
		}
		sink = io.MultiWriter(h, out)
	}
	n, copyErr := copyBounded(ctx, sink, io.LimitReader(src, want.Size+1), p)
	if out != nil {
		if err := out.Close(); err != nil && copyErr == nil {
			copyErr = err
		}
	}
	if copyErr != nil {
		return "", fmt.Errorf("enginefiles: reading %s from the archive: %w", want.Path, copyErr)
	}
	if n != want.Size {
		return "", archiveRefusal("%s is %d bytes in the archive, wanted %d", want.Path, n, want.Size)
	}
	if target != "" && !modified.IsZero() {
		// Keeps compiled files such as CPython's .pyc valid against their
		// sources.
		if err := os.Chtimes(target, modified, modified); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeExclusive(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return errors.Join(err, out.Close())
}

// maxTarEntries bounds the entries a tar listing reads.
const maxTarEntries = 1 << 16

// walkTar reads the gzip-compressed tar in file and calls visit with each
// entry's position, header and body, in stream order. An entry name loses a
// leading "./". A global PAX header is skipped.
func walkTar(file *os.File, size int64, visit func(index int, h *tar.Header, body io.Reader) error) error {
	gz, err := gzip.NewReader(io.NewSectionReader(file, 0, size))
	if err != nil {
		return archiveRefusal("reading the gzip stream: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for index := 0; ; index++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return archiveRefusal("reading the tar stream: %v", err)
		}
		if index >= maxTarEntries {
			return archiveRefusal("the archive holds more than %d entries", maxTarEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		for strings.HasPrefix(h.Name, "./") {
			h.Name = h.Name[2:]
		}
		if err := visit(index, h, tr); err != nil {
			return err
		}
	}
}

// tarEntries lists a gzip-compressed tar's entries without reading their
// bodies into memory.
func tarEntries(file *os.File, size int64) ([]archiveEntry, error) {
	var entries []archiveEntry
	err := walkTar(file, size, func(index int, h *tar.Header, _ io.Reader) error {
		if h.Name == "" || h.Name == "." {
			if h.Typeflag == tar.TypeDir {
				return nil
			}
			return archiveRefusal("an entry has no name")
		}
		entries = append(entries, archiveEntry{name: h.Name, dir: h.Typeflag == tar.TypeDir,
			regular: h.Typeflag == tar.TypeReg || h.Typeflag == '\x00', size: h.Size, modified: h.ModTime, index: index})
		return nil
	})
	return entries, err
}

// verifiedArchive checks the held archive's size and SHA-256 against a and
// returns its entries. A ZIP is read by its central directory; a gzip tar
// (as Lemonade's Kokoros backend ships) is listed in one pass and read in a
// second.
func verifiedArchive(ctx context.Context, file *os.File, a PinnedArchive, p *progress) (*openedArchive, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != a.Size {
		return nil, archiveRefusal("%s is not the pinned %s: %d bytes, wanted %d", a.Path, a.name(), info.Size(), a.Size)
	}
	h := sha256.New()
	n, err := copyBounded(ctx, h, io.NewSectionReader(file, 0, a.Size+1), p)
	if err != nil {
		return nil, fmt.Errorf("enginefiles: hashing the archive: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != a.Size || got != a.SHA256 {
		return nil, archiveRefusal("%s is not the pinned %s: %d bytes with SHA-256 %s, wanted %d bytes with %s", a.Path, a.name(), n, got, a.Size, a.SHA256)
	}
	opened := &openedArchive{file: file, size: a.Size}
	var magic [2]byte
	if _, err := file.ReadAt(magic[:], 0); err == nil && magic == [2]byte{0x1f, 0x8b} {
		opened.tarball = true
		if opened.entries, err = tarEntries(file, a.Size); err != nil {
			return nil, err
		}
		return opened, nil
	}
	reader, err := zip.NewReader(file, a.Size)
	if err != nil {
		return nil, archiveRefusal("reading %s: %v", a.Path, err)
	}
	for _, e := range reader.File {
		opened.entries = append(opened.entries, archiveEntry{name: e.Name, dir: e.Mode().IsDir(), regular: e.Mode().IsRegular(),
			size: int64(e.UncompressedSize64), modified: e.Modified, zip: e})
	}
	return opened, nil
}

// progress counts the bytes an install has read and written.
type progress struct {
	done, total int64
	report      func(done, total int64) error
}

// copyBounded copies src to dst in chunks, checking ctx and reporting
// progress after each.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, p *progress) (int64, error) {
	buf := make([]byte, 256<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		r, readErr := src.Read(buf)
		if r > 0 {
			if _, err := dst.Write(buf[:r]); err != nil {
				return n, err
			}
			n += int64(r)
			p.done += int64(r)
			if p.report != nil {
				if err := p.report(p.done, p.total); err != nil {
					return n, err
				}
			}
		}
		if readErr == io.EOF {
			return n, nil
		}
		if readErr != nil {
			return n, readErr
		}
	}
}

// checkEntryName accepts only a plain relative path inside the tree: no
// drive, no absolute path, no backslash, no . or .. element, no element
// ending in a space or a dot, no control or Windows-reserved character and
// no Windows device name.
func checkEntryName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:*?\"<>|") {
		return archiveRefusal("entry %q is not a plain relative path", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return archiveRefusal("entry %q contains a control character", name)
		}
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == "" || part == "." || part == ".." || strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
			return archiveRefusal("entry %q is not a plain relative path", name)
		}
		if deviceName(part) {
			return archiveRefusal("entry %q names a Windows device", name)
		}
	}
	return nil
}

// deviceName reports a Windows device name (NUL, CON, AUX, PRN, COM1-COM9,
// LPT1-LPT9) in any case, with or without an extension: the stem before the
// first dot decides.
func deviceName(part string) bool {
	stem, _, _ := strings.Cut(part, ".")
	switch stem = strings.ToUpper(stem); stem {
	case "NUL", "CON", "AUX", "PRN":
		return true
	}
	return len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9'
}
