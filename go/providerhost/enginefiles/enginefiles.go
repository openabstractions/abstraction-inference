// Package enginefiles proves a third-party engine's files at launch and holds
// them unchanged while the engine runs (VISION 2026-10-01; inference
// CONTRACT.md INF-F1..INF-F5). Every provider that launches an engine it did
// not build uses it.
//
// A Tree names the engine folder, the manifest that pins each of its files and
// the metadata record the provider keeps in its private state. A Verifier,
// one per provider process, answers three calls:
//
//   - Install hashes the tree, gives every folder the read-only ACL and
//     records each file's metadata.
//   - Launch opens every file and folder with read sharing only, checks the
//     folder ACLs and proves the files: by their full SHA-256 at the first
//     launch of the process and after any mismatch, and otherwise by each
//     file's ID, size, last-write and change time against the record. The
//     returned Hold keeps the handles until the engine exits.
//   - Check is a Launch whose handles close at once.
//
// Verify is the stateless full hash for probes that launch nothing.
//
// A metadata launch trusts the account's own processes not to have set file
// timestamps back: SetFileTime restores a last-write time, and
// SetFileInformationByHandle restores a change time as well. The full hash at
// the first launch of each provider process is the guarantee against such a
// writer.
//
// On Windows the held handles make writes, renames and deletes of the tree's
// files and folders fail while the engine runs, and the folder ACL stops the
// account's ordinary software from changing the tree between runs. Elsewhere
// the same calls run the full hash and the metadata check, and the handles
// stay open without blocking anything; no ACL is set or checked.
package enginefiles

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/go/trustedpath"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// File is one pinned file of an engine tree.
type File struct {
	Path   string // slash-separated, relative to the tree's folder
	Size   int64
	SHA256 string // lower-case hex
}

// Manifest pins every file of an engine tree. Folders are the ancestors of
// the files; the tree may hold nothing else.
type Manifest struct {
	Files []File
}

// Validate refuses a manifest with no files, a path that is not a plain
// relative slash path, a negative size, a digest that is not 64 lower-case
// hex characters, or two paths that differ only in letter case.
func (m Manifest) Validate() error {
	if len(m.Files) == 0 {
		return errors.New("enginefiles: manifest lists no files")
	}
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if f.Path == "" || !fs.ValidPath(f.Path) || f.Path == "." || strings.ContainsAny(f.Path, "\\:\x00") {
			return fmt.Errorf("enginefiles: manifest path %q is not a plain relative path", f.Path)
		}
		if f.Size < 0 || !lowerHex64(f.SHA256) {
			return fmt.Errorf("enginefiles: manifest entry %q is malformed", f.Path)
		}
		key := nameKey(f.Path)
		if seen[key] {
			return fmt.Errorf("enginefiles: manifest lists %q twice", f.Path)
		}
		seen[key] = true
	}
	return nil
}

// Pin is the SHA-256 of the manifest's canonical form: one
// "<path>\t<sha256>\t<size>\n" line per file in path order after a version
// line. The metadata record is bound to it.
func (m Manifest) Pin() string {
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	io.WriteString(h, "enginefiles manifest v1\n")
	for _, f := range files {
		io.WriteString(h, f.Path+"\t"+f.SHA256+"\t"+strconv.FormatInt(f.Size, 10)+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func lowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// nameKey keys a name a manifest or an archive gives, for a file that may not
// exist yet: a name listed twice, or an archive entry matched to its pinned
// name, compares as the file system compares names (identity.PathNameKey).
// Files that exist are matched by file identity (identity.FileKey).
func nameKey(p string) string { return identity.PathNameKey(p) }

// Tree names one engine folder, its manifest and its metadata record.
type Tree struct {
	// Name labels the tree in refusals, for example "sd-cpp/vulkan".
	Name string
	// Root is the absolute path of the tree's folder. The folder itself must
	// not be a link.
	Root     string
	Manifest Manifest
	// Record is the absolute path of the metadata record in the provider's
	// private state, outside Root.
	Record string
}

func (t Tree) validate() error {
	if !filepath.IsAbs(t.Root) || !filepath.IsAbs(t.Record) {
		return errors.New("enginefiles: tree folder and record must be absolute paths")
	}
	root, record := filepath.Clean(t.Root), filepath.Clean(t.Record)
	// By file identity once the tree exists and by spelling before it does,
	// as an install names a folder still to come (identity.InsideRel). A
	// path identity refuses or a tree it cannot read is refused. The name
	// comparisons also refuse a record the file system would take for one
	// inside the tree.
	_, inside, err := identity.InsideRel(record, root)
	if err != nil {
		return fmt.Errorf("enginefiles: cannot place the metadata record against the tree: %w", err)
	}
	if identity.PathNameWithin(record, root) || identity.SamePathName(record, root) {
		inside = true
	} else if same, err := identity.SameFile(record, root); err == nil && same {
		inside = true
	}
	if inside {
		return errors.New("enginefiles: the metadata record must be outside the tree")
	}
	return t.Manifest.Validate()
}

// ErrMismatch is wrapped by every refusal of a tree that differs from its
// manifest.
var ErrMismatch = errors.New("enginefiles: tree differs from its manifest")

// ErrFolderACL is wrapped by the refusal of a tree folder whose ACL is not
// the read-only engine ACL Install sets.
var ErrFolderACL = errors.New("enginefiles: folder ACL is not the read-only engine ACL; install the engine again")

// Mismatch kinds.
const (
	Extra   = "extra"   // a file or folder the manifest does not list
	Missing = "missing" // a manifest file absent from the tree
	Changed = "changed" // a file whose size or SHA-256 differs
	Special = "special" // a link, device or other entry that is neither a regular file nor a folder
)

// MismatchError names the first entry that differs from the manifest.
type MismatchError struct {
	Tree string
	Kind string // Extra, Missing, Changed or Special
	Path string // slash-separated, relative to the tree's folder
}

func (e *MismatchError) Error() string {
	switch e.Kind {
	case Extra:
		return fmt.Sprintf("%s holds %s, which its manifest does not list", e.Tree, e.Path)
	case Missing:
		return fmt.Sprintf("%s: file %s is missing", e.Tree, e.Path)
	case Changed:
		return fmt.Sprintf("%s: file %s differs from its manifest", e.Tree, e.Path)
	default:
		return fmt.Sprintf("%s holds %s, which is neither a regular file nor a folder", e.Tree, e.Path)
	}
}

func (e *MismatchError) Unwrap() error { return ErrMismatch }

// ErrUnreadable is wrapped by every refusal of a tree entry the verifier
// could not open, read or identify, such as a file another program holds
// open for writing. Such a refusal keeps the system error: errors.Is finds
// both ErrUnreadable and the cause, for example ERROR_SHARING_VIOLATION. A
// refusal wraps ErrUnreadable or ErrMismatch, never both; an entry that no
// longer exists is a Missing mismatch.
var ErrUnreadable = errors.New("enginefiles: a tree entry could not be read")

// UnreadableError names the entry the verifier could not open, read or
// identify, and the system error.
type UnreadableError struct {
	Tree string
	Op   string // what failed: "opening", "reading", "listing", "identifying"
	Path string // slash-separated, relative to the tree's folder; "." is the folder
	Err  error
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("enginefiles: %s: %s %s: %v", e.Tree, e.Op, e.Path, e.Err)
}

func (e *UnreadableError) Unwrap() []error { return []error{ErrUnreadable, e.Err} }

// unreadable classifies a failure to open, read or identify rel: an entry
// that no longer exists is missing, and anything else is unreadable.
func unreadable(tree, op, rel string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return &MismatchError{Tree: tree, Kind: Missing, Path: rel}
	}
	return &UnreadableError{Tree: tree, Op: op, Path: rel, Err: err}
}

// ACL sets and checks the read-only ACL of a tree's folders. SystemACL is the
// platform's; tests substitute their own.
type ACL interface {
	// Restrict gives each folder the read-only ACL. folders[0] is the
	// tree's folder; the others are its subfolders, parents before children.
	Restrict(folders []string) error
	// Check refuses a held folder whose ACL is not the read-only one, with
	// an error that wraps ErrFolderACL.
	Check(folder string, held *Handle) error
}

// Options configures a Verifier.
type Options struct {
	// ACL is nil for SystemACL.
	ACL ACL
	// Workers bounds concurrent opens and hashes; zero selects the CPU count
	// up to 8.
	Workers int
	// TrustPlaced decides whether a placed file may be proved by its
	// metadata record (INF-F7); nil is trustedpath.CheckFile. Tests
	// substitute their own.
	TrustPlaced func(path string, owners trustedpath.Owners) error
}

// Verifier proves engine trees for one provider process. It remembers which
// trees this process has hashed in full and the SHA-256 of the record it
// wrote for each, so a record rewritten by anyone else triggers the full hash.
type Verifier struct {
	acl     ACL
	workers int
	trust   func(string, trustedpath.Owners) error
	mu      sync.Mutex
	known   map[string][sha256.Size]byte // record path -> SHA-256 of the record this process wrote
	trees   map[string]*sync.Mutex       // record path -> one proof of the tree at a time
	// installs serialises archive installs of one tree folder (INF-F6).
	installs map[string]*sync.Mutex
}

// lock serializes proofs of one tree, so two launches never write its
// record at once.
func (v *Verifier) lock(record string) func() {
	v.mu.Lock()
	l, ok := v.trees[record]
	if !ok {
		l = &sync.Mutex{}
		v.trees[record] = l
	}
	v.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// NewVerifier returns a Verifier that has hashed nothing yet.
func NewVerifier(opts Options) *Verifier {
	acl := opts.ACL
	if acl == nil {
		acl = SystemACL()
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
		if workers > 8 {
			workers = 8
		}
	}
	trust := opts.TrustPlaced
	if trust == nil {
		trust = trustedpath.CheckFile
	}
	return &Verifier{acl: acl, workers: workers, trust: trust, known: map[string][sha256.Size]byte{}, trees: map[string]*sync.Mutex{},
		installs: map[string]*sync.Mutex{}}
}

// Hold keeps a proven tree's files and folders open with read sharing only.
// Release closes them once the engine has exited.
type Hold struct {
	nodes  []*Handle
	full   bool
	hashed int
	paths  []string
	once   sync.Once
	err    error
}

// Full reports whether this launch hashed every file.
func (h *Hold) Full() bool { return h != nil && h.full }

// Hashed is how many files this launch read in full: every file when Full,
// and otherwise the placed files that failed the trusted-in-place check
// (INF-F7).
func (h *Hold) Hashed() int {
	if h == nil {
		return 0
	}
	return h.hashed
}

// Paths are the final paths of the files LaunchPlaced holds, in the order
// given, resolved from each held handle (GetFinalPathNameByHandleW on
// Windows). A provider passes these, never the paths it was given, to the
// engine. A tree's Hold has none.
func (h *Hold) Paths() []string {
	if h == nil {
		return nil
	}
	return append([]string(nil), h.paths...)
}

// Release closes every handle. It is safe on a nil Hold and more than once.
func (h *Hold) Release() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() { h.err = closeAll(h.nodes) })
	return h.err
}

// closeAll closes every handle, several at a time: closing 28,000 handles
// one by one takes most of a second.
func closeAll(nodes []*Handle) error {
	var mu sync.Mutex
	var errs []error
	parallel(context.Background(), 8, len(nodes), func(i int) error {
		if nodes[i] != nil {
			if err := nodes[i].close(); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}
		return nil
	})
	return errors.Join(errs...)
}

// Launch proves the tree and returns the Hold the caller keeps until the
// engine exits. Every file and folder is opened before any check, so the
// checks read what stays held.
func (v *Verifier) Launch(ctx context.Context, t Tree) (*Hold, error) {
	return v.run(ctx, t, modeLaunch)
}

// Check proves the tree as Launch does and releases its handles at once.
func (v *Verifier) Check(ctx context.Context, t Tree) error {
	h, err := v.run(ctx, t, modeLaunch)
	if err != nil {
		return err
	}
	return h.Release()
}

// Install hashes the tree in full, gives every folder the read-only ACL and
// writes the metadata record. It holds the files while it works and releases
// them before it returns.
func (v *Verifier) Install(ctx context.Context, t Tree) error {
	h, err := v.run(ctx, t, modeInstall)
	if err != nil {
		return err
	}
	return h.Release()
}

// Verify hashes the tree against m in full. It checks no ACL and reads or
// writes no record; probes that launch nothing use it.
func Verify(ctx context.Context, name, root string, m Manifest) error {
	if !filepath.IsAbs(root) {
		return errors.New("enginefiles: tree folder must be an absolute path")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	v := NewVerifier(Options{ACL: noACL{}})
	m = sorted(m)
	if name == "" {
		name = filepath.Base(root)
	}
	o, err := v.open(ctx, Tree{Name: name, Root: root, Manifest: m})
	if err != nil {
		return err
	}
	defer closeAll(o.nodes)
	return v.hashAll(ctx, Tree{Name: name, Manifest: m}, o)
}

type noACL struct{}

func (noACL) Restrict([]string) error     { return nil }
func (noACL) Check(string, *Handle) error { return nil }

type mode int

const (
	modeLaunch mode = iota
	modeInstall
)

// opened is a tree with every folder and file held.
type opened struct {
	folders []string  // absolute, the tree's folder first, parents before children
	dirs    []*Handle // parallel to folders
	files   []*Handle // parallel to the sorted manifest
	nodes   []*Handle // every handle, for release
	id      rootID
}

func (v *Verifier) run(ctx context.Context, t Tree, m mode) (*Hold, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	if t.Name == "" {
		t.Name = filepath.Base(t.Root)
	}
	t.Manifest = sorted(t.Manifest)
	defer v.lock(t.Record)()
	o, err := v.open(ctx, t)
	if err != nil {
		v.forget(t.Record)
		return nil, err
	}
	hold := &Hold{nodes: o.nodes}
	fail := func(err error) (*Hold, error) {
		v.forget(t.Record)
		hold.Release()
		return nil, err
	}
	checkACL := func() error {
		for i, folder := range o.folders {
			if err := v.acl.Check(folder, o.dirs[i]); err != nil {
				return fmt.Errorf("%s: %w", t.Name, err)
			}
		}
		return nil
	}
	if m == modeLaunch {
		if err := checkACL(); err != nil {
			return fail(err)
		}
	}
	metas, err := v.metas(ctx, t, o)
	if err != nil {
		return fail(err)
	}
	pin := t.Manifest.Pin()
	full := m == modeInstall || !v.recordMatches(t, pin, o.id, metas)
	if full {
		if err := v.hashAll(ctx, t, o); err != nil {
			return fail(err)
		}
	}
	if m == modeInstall {
		if err := v.acl.Restrict(o.folders); err != nil {
			return fail(fmt.Errorf("enginefiles: %s: setting the read-only folder ACL: %w", t.Name, err))
		}
		if err := checkACL(); err != nil {
			return fail(err)
		}
		// A changed ACL can move a file's change time; the record takes the
		// metadata after it.
		if metas, err = v.metas(ctx, t, o); err != nil {
			return fail(err)
		}
	}
	if full {
		if err := v.remember(t, pin, o.id, metas); err != nil {
			return fail(err)
		}
	}
	hold.full = full
	return hold, nil
}

func sorted(m Manifest) Manifest {
	files := append([]File(nil), m.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return Manifest{Files: files}
}

// open walks the tree, refuses any entry the manifest does not allow and
// opens every folder and file with read sharing only.
func (v *Verifier) open(ctx context.Context, t Tree) (*opened, error) {
	root := filepath.Clean(t.Root)
	info, err := os.Lstat(root)
	if err != nil {
		return nil, unreadable(t.Name, "reading", ".", err)
	}
	if !info.IsDir() || info.Mode()&fs.ModeType != fs.ModeDir {
		return nil, &MismatchError{Tree: t.Name, Kind: Special, Path: "."}
	}
	// Disk entries are matched to the manifest by file identity: each listed
	// file and folder that exists is keyed by identity.FileKey, and so is each
	// entry the walk finds. A listed file that does not exist stays unfound.
	want := make(map[string]int, len(t.Manifest.Files))
	folders := map[string]bool{}
	if key, err := identity.FileKey(root); err == nil {
		folders[key] = true
	}
	for i, f := range t.Manifest.Files {
		if key, err := identity.FileKey(filepath.Join(root, filepath.FromSlash(f.Path))); err == nil {
			want[key] = i
		}
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if key, err := identity.FileKey(filepath.Join(root, filepath.FromSlash(d))); err == nil {
				folders[key] = true
			}
		}
	}
	o := &opened{files: make([]*Handle, len(t.Manifest.Files))}
	diskFiles := make([]string, len(t.Manifest.Files))
	found := make([]bool, len(t.Manifest.Files))
	seenFolders := map[string]bool{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return unreadable(t.Name, "listing", relSlash(root, p), err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := "."
		if p != root {
			r, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(r)
		}
		switch typ := d.Type(); {
		case typ == fs.ModeDir:
			key, err := identity.FileKey(p)
			if err != nil {
				return unreadable(t.Name, "identifying", rel, err)
			}
			if !folders[key] || seenFolders[key] {
				return &MismatchError{Tree: t.Name, Kind: Extra, Path: rel}
			}
			seenFolders[key] = true
			o.folders = append(o.folders, p)
			return nil
		case typ.IsRegular():
			key, err := identity.FileKey(p)
			if err != nil {
				return unreadable(t.Name, "identifying", rel, err)
			}
			i, ok := want[key]
			if !ok || found[i] {
				return &MismatchError{Tree: t.Name, Kind: Extra, Path: rel}
			}
			found[i] = true
			diskFiles[i] = p
			return nil
		default:
			return &MismatchError{Tree: t.Name, Kind: Special, Path: rel}
		}
	})
	if err != nil {
		return nil, err
	}
	for i, ok := range found {
		if !ok {
			return nil, &MismatchError{Tree: t.Name, Kind: Missing, Path: t.Manifest.Files[i].Path}
		}
	}
	o.dirs = make([]*Handle, len(o.folders))
	fail := func(err error) (*opened, error) {
		closeAll(o.dirs)
		closeAll(o.files)
		return nil, err
	}
	for i, folder := range o.folders {
		h, err := openHeld(folder, true)
		if err != nil {
			return fail(unreadable(t.Name, "opening", relSlash(root, folder), err))
		}
		o.dirs[i] = h
		if err := h.checkDir(); err != nil {
			return fail(&MismatchError{Tree: t.Name, Kind: Special, Path: relSlash(root, folder)})
		}
	}
	if o.id, err = o.dirs[0].rootID(); err != nil {
		return fail(&UnreadableError{Tree: t.Name, Op: "identifying", Path: ".", Err: err})
	}
	err = parallel(ctx, v.workers, len(diskFiles), func(i int) error {
		h, err := openHeld(diskFiles[i], false)
		if err != nil {
			return unreadable(t.Name, "opening", t.Manifest.Files[i].Path, err)
		}
		o.files[i] = h
		return nil
	})
	if err != nil {
		return fail(err)
	}
	o.nodes = append(append([]*Handle(nil), o.dirs...), o.files...)
	return o, nil
}

func relSlash(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// metas reads every held file's metadata and refuses one that is not a
// regular file, lies on another volume, or has another size than its
// manifest entry.
func (v *Verifier) metas(ctx context.Context, t Tree, o *opened) ([]meta, error) {
	out := make([]meta, len(o.files))
	err := parallel(ctx, v.workers, len(o.files), func(i int) error {
		m, err := o.files[i].fileMeta()
		f := t.Manifest.Files[i]
		if err != nil {
			if errors.Is(err, errNotRegular) {
				return &MismatchError{Tree: t.Name, Kind: Special, Path: f.Path}
			}
			return &UnreadableError{Tree: t.Name, Op: "identifying", Path: f.Path, Err: err}
		}
		if m.volume != o.id.volume {
			return &MismatchError{Tree: t.Name, Kind: Special, Path: f.Path}
		}
		if m.size != f.Size {
			return &MismatchError{Tree: t.Name, Kind: Changed, Path: f.Path}
		}
		out[i] = m
		return nil
	})
	return out, err
}

// hashAll compares every held file's SHA-256 with its manifest entry.
func (v *Verifier) hashAll(ctx context.Context, t Tree, o *opened) error {
	all := make([]int, len(o.files))
	for i := range all {
		all[i] = i
	}
	_, err := v.hashSome(ctx, t, o, all)
	return err
}

// hashSome hashes the files at the indices given and returns how many it
// read to the end and proved.
func (v *Verifier) hashSome(ctx context.Context, t Tree, o *opened, indices []int) (int, error) {
	var read atomic.Int64
	err := parallel(ctx, v.workers, len(indices), func(k int) error {
		i := indices[k]
		f := t.Manifest.Files[i]
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(o.files[i], f.Size+1))
		if err != nil {
			return &UnreadableError{Tree: t.Name, Op: "reading", Path: f.Path, Err: err}
		}
		if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return &MismatchError{Tree: t.Name, Kind: Changed, Path: f.Path}
		}
		read.Add(1)
		return nil
	})
	return int(read.Load()), err
}

// parallel runs fn for 0..n-1 on up to workers goroutines and returns the
// first error; later items are skipped once one fails.
func parallel(ctx context.Context, workers, n int, fn func(int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var once sync.Once
	var first error
	jobs := make(chan int)
	var wg sync.WaitGroup
	if workers > n {
		workers = n
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ctx.Err() != nil {
					continue
				}
				if err := fn(i); err != nil {
					once.Do(func() { first = err; cancel() })
				}
			}
		}()
	}
	for i := 0; i < n && ctx.Err() == nil; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// The metadata record: a header binding it to the manifest pin, the tree's
// final path and its volume, then one line per file in manifest order:
// "<path>\t<file ID>\t<size>\t<last-write>\t<change>".
const recordHeader = "enginefiles record v1\n"

func encodeRecord(t Tree, pin string, id rootID, metas []meta) []byte {
	var b bytes.Buffer
	b.WriteString(recordHeader)
	fmt.Fprintf(&b, "pin\t%s\nroot\t%s\nvolume\t%x\n", pin, id.final, id.volume)
	for i, f := range t.Manifest.Files {
		m := metas[i]
		fmt.Fprintf(&b, "%s\t%x\t%d\t%d\t%d\n", f.Path, m.id, m.size, m.write, m.change)
	}
	return b.Bytes()
}

// recordMatches reports whether this process wrote the record now on disk
// for this tree, and every held file's metadata equals it.
func (v *Verifier) recordMatches(t Tree, pin string, id rootID, metas []meta) bool {
	v.mu.Lock()
	wrote, ok := v.known[t.Record]
	v.mu.Unlock()
	if !ok {
		return false
	}
	data, err := readLimited(t.Record, 64<<20)
	if err != nil || sha256.Sum256(data) != wrote {
		return false
	}
	return bytes.Equal(data, encodeRecord(t, pin, id, metas))
}

func (v *Verifier) remember(t Tree, pin string, id rootID, metas []meta) error {
	data := encodeRecord(t, pin, id, metas)
	if err := writeAtomic(t.Record, data); err != nil {
		return fmt.Errorf("enginefiles: %s: writing the metadata record: %w", t.Name, err)
	}
	v.mu.Lock()
	v.known[t.Record] = sha256.Sum256(data)
	v.mu.Unlock()
	return nil
}

func (v *Verifier) forget(record string) {
	v.mu.Lock()
	delete(v.known, record)
	v.mu.Unlock()
}

// meta is what the metadata check compares for one file.
type meta struct {
	id     [16]byte // FILE_ID_INFO's 128-bit file ID, or the inode number
	volume uint64   // FILE_ID_INFO's volume serial, or the device number
	size   int64
	write  int64 // last-write time
	change int64 // change time: any write, rename, link or attribute change
}

// rootID binds the record to where the tree is: its final path and volume.
type rootID struct {
	final  string
	volume uint64
}

var errNotRegular = errors.New("not a regular file")

func readLimited(file string, limit int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(bufio.NewReader(io.LimitReader(f, limit+1)))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("enginefiles: record too large")
	}
	return data, nil
}

func writeAtomic(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, file); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
