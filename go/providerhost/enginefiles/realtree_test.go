package enginefiles

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRealTreeTimes measures the full hash and the metadata check on an
// extracted engine named by OA_ENGINEFILES_TREE, for example TheNoise's
// 25,904 files. It only reads the tree: it hashes every file to build the
// manifest, holds each file open with read sharing during each launch, sets
// no ACL and writes its record to a temporary folder. Without the variable
// it is skipped.
func TestRealTreeTimes(t *testing.T) {
	root := os.Getenv("OA_ENGINEFILES_TREE")
	if root == "" {
		t.Skip("OA_ENGINEFILES_TREE names no extracted engine")
	}
	start := time.Now()
	m := manifestOf(t, root)
	t.Logf("manifest of %d files built in %v", len(m.Files), time.Since(start).Round(time.Millisecond))
	tree := Tree{Name: "real", Root: root, Manifest: m, Record: filepath.Join(t.TempDir(), "real.record")}
	v := NewVerifier(Options{ACL: noACL{}})

	start = time.Now()
	h, err := v.Launch(t.Context(), tree)
	if err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)
	if !h.Full() {
		t.Fatal("the first launch did not hash in full")
	}
	start = time.Now()
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	t.Logf("first launch (full hash, open and hold): %v; release %v", full.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	for i := 0; i < 3; i++ {
		start = time.Now()
		h, err := v.Launch(t.Context(), tree)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		if h.Full() {
			t.Fatal("a later launch hashed in full")
		}
		start = time.Now()
		if err := h.Release(); err != nil {
			t.Fatal(err)
		}
		t.Logf("metadata launch %d (walk, open and hold, metadata, record): %v; release %v", i+1, took.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	}

	sortedTree := tree
	sortedTree.Manifest = sorted(tree.Manifest)
	start = time.Now()
	o, err := v.open(t.Context(), sortedTree)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Since(start)
	start = time.Now()
	metas, err := v.metas(t.Context(), sortedTree, o)
	if err != nil {
		t.Fatal(err)
	}
	read := time.Since(start)
	start = time.Now()
	ok := v.recordMatches(sortedTree, sortedTree.Manifest.Pin(), o.id, metas)
	compared := time.Since(start)
	closeAll(o.nodes)
	if !ok {
		t.Fatal("the record did not match")
	}
	t.Logf("phases: walk and open %d folders and %d files %v; metadata %v; record read and compare %v",
		len(o.folders), len(o.files), opened.Round(time.Millisecond), read.Round(time.Millisecond), compared.Round(time.Millisecond))
}

// manifestOf hashes every regular file below root.
func manifestOf(t *testing.T, root string) Manifest {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files := make([]File, len(paths))
	err = parallel(t.Context(), 8, len(paths), func(i int) error {
		f, err := os.Open(paths[i])
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, paths[i])
		if err != nil {
			return err
		}
		files[i] = File{Path: filepath.ToSlash(rel), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return Manifest{Files: files}
}
