package enginefiles

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/openabstractions/abstraction-identity/go/trustedpath"

	identity "github.com/openabstractions/abstraction-identity"
)

// A Placed file is one pinned file proved where it already lies, in a store
// OA does not own: a model file in the person's model folder or another
// application's (VISION 2026-10-01, OA never copies a model for an engine;
// inference CONTRACT.md INF-F7). Path is absolute.
type Placed struct {
	Path   string
	Size   int64
	SHA256 string // lower-case hex
	// Owners are the owners the file and every folder above it may have for
	// the file to be trusted in place (trustedpath.CheckFile): a trusted
	// file is proved by its metadata record after its first full hash, and
	// any other is hashed in full at every launch. Zero trusts no owner.
	Owners trustedpath.Owners
}

// LaunchPlaced proves placed files where they lie and holds each open with
// read sharing only until the Hold is released, as Launch does for a tree:
// by its full SHA-256 at the first proof in this process and after any
// change, and otherwise by its file ID, size, last-write and change time
// against record. The metadata shortcut applies to a file only when it
// passes the trusted-in-place check for its Owners (trustedpath.CheckFile,
// identity ID-D2): no principal beyond them can change or remove the file
// or rename what holds it. A file that fails the check is hashed in full at
// every launch. Every folder on each file's path is opened first, from
// the volume's root down, refused when it is a reparse point (a junction,
// symbolic link or mount point) and held so that it cannot be renamed,
// deleted or replaced while the Hold lasts. Hold.Paths gives each file's
// final path, resolved from its held handle, for the engine to open. No
// folder is listed and no ACL is set: the folders are the store's, and
// other files beside a placed one are not this proof's. While held on
// Windows, the files cannot be written, renamed or deleted.
func (v *Verifier) LaunchPlaced(ctx context.Context, name string, files []Placed, record string) (*Hold, error) {
	t, err := placedTree(name, files, record)
	if err != nil {
		return nil, err
	}
	defer v.lock(record)()
	o := &opened{files: make([]*Handle, len(files))}
	fail := func(err error) (*Hold, error) {
		v.forget(record)
		//unchecked: the refusal is the error returned; a failed close leaves only read-shared handles
		closeAll(o.nodes)
		return nil, err
	}
	finals := make([]string, len(files))
	for i, f := range files {
		rel := t.Manifest.Files[i].Path
		folders, file, err := holdPath(filepath.Clean(f.Path))
		o.nodes = append(o.nodes, folders...)
		if err != nil {
			return fail(placedRefusal(name, rel, err))
		}
		o.nodes = append(o.nodes, file)
		o.files[i] = file
		if finals[i], err = file.finalPath(); err != nil {
			return fail(&UnreadableError{Tree: name, Op: "resolving", Path: rel, Err: err})
		}
	}
	metas := make([]meta, len(files))
	for i, h := range o.files {
		m, err := h.fileMeta()
		f := t.Manifest.Files[i]
		if err != nil {
			if errors.Is(err, errNotRegular) {
				return fail(&MismatchError{Tree: name, Kind: Special, Path: f.Path})
			}
			return fail(&UnreadableError{Tree: name, Op: "identifying", Path: f.Path, Err: err})
		}
		if m.size != f.Size {
			return fail(&MismatchError{Tree: name, Kind: Changed, Path: f.Path})
		}
		metas[i] = m
	}
	// The check reads the path while every folder on it is held.
	var untrusted []int
	for i, f := range files {
		if f.Owners.Account || len(f.Owners.SIDs) > 0 {
			if v.trust(filepath.Clean(f.Path), f.Owners) == nil {
				continue
			}
		}
		untrusted = append(untrusted, i)
	}
	pin := t.Manifest.Pin()
	id := rootID{final: "placed"}
	full := !v.recordMatches(t, pin, id, metas)
	hashed := len(files)
	if full {
		if err := v.hashAll(ctx, t, o); err != nil {
			return fail(err)
		}
		if err := v.remember(t, pin, id, metas); err != nil {
			return fail(err)
		}
	} else {
		read, err := v.hashSome(ctx, t, o, untrusted)
		if err != nil {
			return fail(err)
		}
		hashed = read
	}
	return &Hold{nodes: o.nodes, full: full || hashed == len(files), hashed: hashed, paths: finals}, nil
}

// CheckPlaced proves placed files as LaunchPlaced does and releases them at
// once.
func (v *Verifier) CheckPlaced(ctx context.Context, name string, files []Placed, record string) error {
	h, err := v.LaunchPlaced(ctx, name, files, record)
	if err != nil {
		return err
	}
	return h.Release()
}

// CheckPlainPath refuses an absolute file path that a reparse point along
// it (a junction, symbolic link or mount point), or the file itself being
// one, could send elsewhere, as LaunchPlaced does; a missing file wraps
// fs.ErrNotExist. It holds nothing once it returns.
func CheckPlainPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("enginefiles: %s is not an absolute path", p)
	}
	folders, file, err := holdPath(filepath.Clean(p))
	nodes := folders
	if file != nil {
		nodes = append(nodes, file)
	}
	if err == nil {
		if _, err = file.fileMeta(); errors.Is(err, errNotRegular) {
			err = errLinkOnPath
		}
	}
	//unchecked: the handles were opened for this check alone and are read-shared
	closeAll(nodes)
	return err
}

// HoldDirectory keeps dir and its ancestors open without delete sharing on
// Windows, refusing reparse points before opening through them. Contents may
// still be created or atomically replaced. It proves namespace lifetime only;
// callers must separately establish their directory and content authority.
// Outside Windows the descriptors prevent no rename (INF-F5).
func HoldDirectory(dir string) (*Hold, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("enginefiles: %s is not an absolute directory", dir)
	}
	if err := identity.LocalPath(dir); err != nil {
		return nil, err
	}
	dir = filepath.Clean(dir)
	paths := ancestors(dir)
	if filepath.Dir(dir) != dir {
		paths = append(paths, dir)
	}
	nodes, err := holdFolders(paths)
	if err != nil {
		closeAll(nodes)
		return nil, err
	}
	return &Hold{nodes: nodes}, nil
}

// errLinkOnPath is a placed file's path through a reparse point or a
// symbolic link, a folder on it or the file itself.
var errLinkOnPath = errors.New("the path passes through a link (reparse point or symbolic link)")

// holdPath opens every folder from p's volume root down to p's folder,
// refusing one that is a reparse point, and then p itself with read
// sharing only. Each folder is open before the next is resolved through
// it, so none can be swapped once checked. The folders opened are returned
// with any error, for the caller to close.
func holdPath(p string) ([]*Handle, *Handle, error) {
	folders, err := holdFolders(ancestors(p))
	if err != nil {
		return folders, nil, err
	}
	file, err := openHeld(p, false)
	if err != nil {
		return folders, nil, err
	}
	return folders, file, nil
}

func holdFolders(paths []string) ([]*Handle, error) {
	var folders []*Handle
	for _, dir := range paths {
		h, err := openAncestor(dir)
		if err != nil {
			return folders, err
		}
		folders = append(folders, h)
		if err := h.checkDir(); err != nil {
			if errors.Is(err, errNotRegular) {
				return folders, fmt.Errorf("%w: %s", errLinkOnPath, dir)
			}
			return folders, err
		}
		if afterFolder != nil {
			afterFolder(dir)
		}
	}
	return folders, nil
}

// afterFolder, set by tests only, runs after each folder on a placed path
// is held and checked, before the next is opened through it.
var afterFolder func(dir string)

// ancestors are the folders above the absolute path p, the volume's root
// first.
func ancestors(p string) []string {
	vol := filepath.VolumeName(p)
	sep := string(filepath.Separator)
	cur := vol + sep
	out := []string{cur}
	parts := strings.Split(strings.Trim(p[len(vol):], sep), sep)
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

// placedRefusal names why a placed file could not be held.
func placedRefusal(name, rel string, err error) error {
	if errors.Is(err, errLinkOnPath) {
		return &MismatchError{Tree: name, Kind: Special, Path: rel}
	}
	return unreadable(name, "opening", rel, err)
}

// placedTree is the manifest a placed proof records: each file by its
// absolute path, in the order given.
func placedTree(name string, files []Placed, record string) (Tree, error) {
	if len(files) == 0 || !filepath.IsAbs(record) {
		return Tree{}, errors.New("enginefiles: placed files need at least one file and an absolute record")
	}
	if name == "" {
		name = "placed files"
	}
	seen := map[string]bool{}
	t := Tree{Name: name, Record: record}
	for _, f := range files {
		if !filepath.IsAbs(f.Path) || f.Size < 0 || !lowerHex64(f.SHA256) {
			return Tree{}, fmt.Errorf("enginefiles: placed file %q needs an absolute path, a size and a lower-case SHA-256", f.Path)
		}
		// One placed file named twice: by its parent's file identity and its
		// name (identity.PlaceKey), so two spellings of one file collide.
		key, err := identity.PlaceKey(f.Path)
		if err != nil {
			key = "name:" + nameKey(filepath.Clean(f.Path))
		}
		if seen[key] {
			return Tree{}, fmt.Errorf("enginefiles: placed file %s is named twice", f.Path)
		}
		seen[key] = true
		t.Manifest.Files = append(t.Manifest.Files, File{Path: filepath.ToSlash(filepath.Clean(f.Path)), Size: f.Size, SHA256: f.SHA256})
	}
	return t, nil
}
