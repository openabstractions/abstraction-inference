package enginefiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	name, content string
	kind          byte
}

// writeTarball writes a gzip-compressed tar of entries and returns its pin.
func writeTarball(t *testing.T, entries ...tarEntry) PinnedArchive {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		kind := e.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: kind, Mode: 0o644, ModTime: archiveTime}
		switch kind {
		case tar.TypeReg:
			h.Size = int64(len(e.content))
		case tar.TypeSymlink:
			h.Linkname = e.content
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "engine.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return PinnedArchive{Path: path, Size: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:])}
}

// kokoroKept is a Kokoros-like tree: the executable and a data folder under
// one top-level folder the install strips as Prefix, and Lemonade's
// version.txt written beside them.
var kokoroKept = map[string]string{"koko.exe": "koko", "espeak-ng-data/phondata": "ph", "espeak-ng-data/voices/!v/Mr serious": "mr", "version.txt": "b17\n"}

func kokoroInstall(t *testing.T, a PinnedArchive) (ArchiveInstall, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bin", "kokoro", "cpu")
	return ArchiveInstall{Archive: a, Prefix: "windows-x86_64/", Extra: map[string][]byte{"version.txt": []byte("b17\n")},
		Tree: Tree{Name: "kokoro/cpu", Root: root, Manifest: pinnedFiles(kokoroKept), Record: filepath.Join(filepath.Dir(root), "kokoro.files")}}, root
}

// INF-F6 for a gzip tar: the archive is pinned, every kept entry is chosen by
// name and checked against its pin, a "./" before an entry name is the same
// entry, and folder entries write nothing.
func TestInstallFromTarballWritesOnlyThePinnedFiles(t *testing.T) {
	a := writeTarball(t,
		tarEntry{name: "./windows-x86_64/", kind: tar.TypeDir},
		tarEntry{name: "./windows-x86_64/koko.exe", content: "koko"},
		tarEntry{name: "windows-x86_64/espeak-ng-data/", kind: tar.TypeDir},
		tarEntry{name: "windows-x86_64/espeak-ng-data/phondata", content: "ph"},
		tarEntry{name: "windows-x86_64/espeak-ng-data/voices/!v/Mr serious", content: "mr"},
	)
	in, root := kokoroInstall(t, a)
	v := NewVerifier(Options{ACL: newFakeACL()})
	if _, err := v.InstallFromArchive(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for name, content := range kokoroKept {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(got) != content {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	m, err := ArchiveManifest(context.Background(), a, "windows-x86_64/")
	if err != nil || len(m.Files) != 3 {
		t.Fatalf("recorded manifest %+v %v", m, err)
	}
}

func TestInstallFromTarballRefusals(t *testing.T) {
	good := []tarEntry{{name: "windows-x86_64/koko.exe", content: "koko"}, {name: "windows-x86_64/espeak-ng-data/phondata", content: "ph"},
		{name: "windows-x86_64/espeak-ng-data/voices/!v/Mr serious", content: "mr"}}
	for name, entries := range map[string][]tarEntry{
		"symlink":    append(append([]tarEntry{}, good...), tarEntry{name: "windows-x86_64/mbrola.dll", content: `C:\x`, kind: tar.TypeSymlink}),
		"extra file": append(append([]tarEntry{}, good...), tarEntry{name: "windows-x86_64/mbrola.dll", content: "x"}),
		"traversal":  append(append([]tarEntry{}, good...), tarEntry{name: "windows-x86_64/../evil.dll", content: "x"}),
		"changed":    {{name: "windows-x86_64/koko.exe", content: "KOKO"}, good[1], good[2]},
		"missing":    good[:2],
		"no prefix":  append(append([]tarEntry{}, good...), tarEntry{name: "other/koko.exe", content: "koko"}),
		"twice":      append(append([]tarEntry{}, good...), good[0]),
		"case twice": append(append([]tarEntry{}, good...), tarEntry{name: "windows-x86_64/KOKO.exe", content: "koko"}),
	} {
		in, root := kokoroInstall(t, writeTarball(t, entries...))
		if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(context.Background(), in); !errors.Is(err, ErrArchive) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the tree folder exists after a refusal", name)
		}
	}
	// A file that is neither ZIP nor gzip is refused after its pin matches.
	path := filepath.Join(t.TempDir(), "engine.tar.gz")
	if err := os.WriteFile(path, []byte("plain bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("plain bytes"))
	in, _ := kokoroInstall(t, PinnedArchive{Path: path, Size: 11, SHA256: hex.EncodeToString(sum[:])})
	if _, err := NewVerifier(Options{ACL: newFakeACL()}).InstallFromArchive(context.Background(), in); !errors.Is(err, ErrArchive) {
		t.Errorf("plain file: %v", err)
	}
}
