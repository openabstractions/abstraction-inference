package enginefiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/openabstractions/abstraction-identity/go/trustedpath"
)

func placedFile(t *testing.T, dir, name, content string) Placed {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return Placed{Path: p, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

func TestHoldDirectoryPermitsDurableAtomicRecordUpdates(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hold, err := HoldDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	path := filepath.Join(dir, "record.json")
	for _, value := range []string{`{"phase":"approved"}`, `{"phase":"closed"}`} {
		file, err := os.CreateTemp(dir, "record-*")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString(value); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file.Name(), path); err != nil {
			t.Fatal(err)
		}
		if raw, err := os.ReadFile(path); err != nil || string(raw) != value {
			t.Fatalf("record update: %s %v", raw, err)
		}
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// INF-F7: files proved where they lie, in two folders that hold other
// files too, are hashed at the first proof and, trusted in place, proved by
// their metadata after it; a changed file is refused.
func TestPlacedFilesAreProvedWhereTheyLie(t *testing.T) {
	// Placed paths follow no symbolic link; macOS's temporary folder is
	// under one.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := placedFile(t, filepath.Join(root, "store-a"), "diffusion.gguf", "diffusion weights")
	b := placedFile(t, filepath.Join(root, "store-b", "nested"), "encoder.gguf", "encoder weights")
	placedFile(t, filepath.Join(root, "store-a"), "unrelated.bin", "someone else's file")
	a.Owners, b.Owners = trustedpath.Owners{Account: true}, trustedpath.Owners{Account: true}
	record := filepath.Join(root, "state", "placed.files")
	v := NewVerifier(Options{TrustPlaced: func(string, trustedpath.Owners) error { return nil }})
	hold, err := v.LaunchPlaced(context.Background(), "test set", []Placed{a, b}, record)
	if err != nil {
		t.Fatal(err)
	}
	if !hold.Full() {
		t.Fatal("the first proof did not hash")
	}
	if runtime.GOOS == "windows" {
		if f, err := os.OpenFile(a.Path, os.O_WRONLY, 0); err == nil {
			f.Close()
			t.Fatal("a held file opened for writing")
		}
		if err := os.Remove(b.Path); err == nil {
			t.Fatal("a held file was deleted")
		}
	}
	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}
	hold, err = v.LaunchPlaced(context.Background(), "test set", []Placed{a, b}, record)
	if err != nil || hold.Full() {
		t.Fatalf("second proof: full=%v %v", hold.Full(), err)
	}
	hold.Release()
	if err := os.WriteFile(a.Path, []byte("diffusion weightZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckPlaced(context.Background(), "test set", []Placed{a, b}, record); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a changed file: %v", err)
	}
	wrongSize := b
	wrongSize.Size++
	if err := v.CheckPlaced(context.Background(), "test set", []Placed{wrongSize}, record); !errors.Is(err, ErrMismatch) {
		t.Fatalf("a file of another size: %v", err)
	}
	if err := v.CheckPlaced(context.Background(), "test set", []Placed{{Path: filepath.Join(root, "missing.gguf"), Size: 1, SHA256: a.SHA256}}, record); err == nil {
		t.Fatal("a missing file was proved")
	}
	for _, bad := range [][]Placed{nil, {{Path: "relative.gguf", Size: 1, SHA256: a.SHA256}}, {a, a}, {{Path: a.Path, Size: a.Size, SHA256: "ABC"}}} {
		if err := v.CheckPlaced(context.Background(), "test set", bad, record); err == nil {
			t.Fatalf("%+v was proved", bad)
		}
	}
}

// launches proves files three times with v and returns how many files each
// launch read in full.
func launches(t *testing.T, v *Verifier, files []Placed, record string) [3]int {
	t.Helper()
	var out [3]int
	for i := range out {
		hold, err := v.LaunchPlaced(context.Background(), "test set", files, record)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = hold.Hashed()
		if err := hold.Release(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// INF-F7, the review of da8e3b548 items 4 and 5: the metadata shortcut is
// taken only for a placed file that passes the trusted-in-place check for
// its owners. A trusted file is read once; an untrusted one at every
// launch; in a mixed set only the untrusted file is read again; a file
// with no owners is never asked about and always read.
func TestPlacedTrustDecidesTheShortcut(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := placedFile(t, filepath.Join(root, "ours"), "a.gguf", "weights a")
	b := placedFile(t, filepath.Join(root, "theirs"), "b.gguf", "weights b")
	a.Owners, b.Owners = trustedpath.Owners{Account: true}, trustedpath.MachineOwners()
	var asked atomic.Int32
	only := func(trusted ...string) Options {
		return Options{TrustPlaced: func(p string, _ trustedpath.Owners) error {
			asked.Add(1)
			for _, ok := range trusted {
				if p == ok {
					return nil
				}
			}
			return trustedpath.ErrUntrusted
		}}
	}
	for _, c := range []struct {
		name string
		opts Options
		want [3]int
	}{
		{"trusted", only(a.Path, b.Path), [3]int{2, 0, 0}},
		{"untrusted", only(), [3]int{2, 2, 2}},
		{"mixed", only(a.Path), [3]int{2, 1, 1}},
	} {
		got := launches(t, NewVerifier(c.opts), []Placed{a, b}, filepath.Join(root, "state", c.name+".files"))
		if got != c.want {
			t.Fatalf("%s: files read per launch %v, want %v", c.name, got, c.want)
		}
	}
	asked.Store(0)
	bare := a
	bare.Owners = trustedpath.Owners{}
	if got := launches(t, NewVerifier(only(a.Path)), []Placed{bare}, filepath.Join(root, "state", "bare.files")); got != [3]int{1, 1, 1} || asked.Load() != 0 {
		t.Fatalf("a file with no owners: read %v, asked %d", got, asked.Load())
	}
	// The real check: a file this test made is never owned by SYSTEM alone.
	system := a
	system.Owners = trustedpath.Owners{SIDs: []string{"S-1-5-18"}}
	if got := launches(t, NewVerifier(Options{}), []Placed{system}, filepath.Join(root, "state", "system.files")); got != [3]int{1, 1, 1} {
		t.Fatalf("a file trustedpath refuses: read %v", got)
	}
}

// The real check on a file it passes: a system file that only SYSTEM,
// Administrators and TrustedInstaller can change, read in place and never
// written, is read in full once and then proved by its metadata.
func TestPlacedTrustedFileIsReadOnce(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("trustedpath checks owners and access on Windows only")
	}
	path := filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll")
	if err := trustedpath.CheckFile(path, trustedpath.MachineOwners()); err != nil {
		t.Skipf("%s is not trusted in place here: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	f := Placed{Path: path, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Owners: trustedpath.MachineOwners()}
	if got := launches(t, NewVerifier(Options{}), []Placed{f}, filepath.Join(t.TempDir(), "placed.files")); got != [3]int{1, 0, 0} {
		t.Fatalf("a trusted file: read %v, want once", got)
	}
}
