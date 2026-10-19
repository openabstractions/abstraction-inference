package enginelaunch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
	"github.com/openabstractions/abstraction-identity/go/privatedir"
)

// INF-EL2: the socket root is a fixed per-user folder, independent of any
// provider's state folder, short enough for a launch socket. Only its path
// is computed; nothing is created.
func TestSocketRootIsAFixedPerUserFolder(t *testing.T) {
	root, err := SocketRoot()
	if err != nil {
		view, probeErr := bootstrap.CurrentProfileView()
		if runtime.GOOS == "windows" && probeErr == nil && view.Virtualized && errors.Is(err, ErrSocketRoot) {
			t.Skipf("this process has a virtualized profile view: %v", err)
		}
		t.Fatal(err)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("socket root %q is not absolute", root)
	}
	switch runtime.GOOS {
	case "windows":
		if !strings.HasSuffix(strings.ToLower(root), `\oa\s`) {
			t.Fatalf("socket root %q is not %%LOCALAPPDATA%%\\oa\\s", root)
		}
	default:
		if root != "/tmp/oa-"+strconv.Itoa(os.Getuid()) && filepath.Base(root) != "oa" {
			t.Fatalf("socket root %q is neither <runtime dir>/oa nor /tmp/oa-<uid>", root)
		}
	}
	if err := SocketPathFits(root); err != nil {
		t.Fatal(err)
	}
}

// INF-EL2: a root made by the kit is private, and a root that already
// exists open beyond this account is refused before a launch directory is
// made in it.
func TestASocketRootOpenBeyondTheAccountIsRefused(t *testing.T) {
	root := socketParent(t)
	if _, err := NewSocketDir(root); err != nil {
		t.Fatalf("a root the kit makes: %v", err)
	}
	if err := privatedir.CheckDir(root); err != nil {
		t.Fatalf("the kit's own root: %v", err)
	}
	wide := filepath.Join(filepath.Dir(root), "wide")
	// On Windows the folder inherits the temporary directory's entries,
	// Administrators among them; elsewhere it is made 0755.
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSocketDir(wide); !errors.Is(err, ErrSocketRoot) || !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a root open beyond the account: %v, want ErrSocketRoot and privatedir.ErrNotPrivate", err)
	}
	if entries, err := os.ReadDir(wide); err != nil || len(entries) != 0 {
		t.Fatalf("a launch directory was made in a refused root: %v %v", entries, err)
	}
}

// INF-EL2: a root that is a link is refused, whatever it points to.
func TestASocketRootThatIsALinkIsRefused(t *testing.T) {
	root := socketParent(t)
	if _, err := NewSocketDir(root); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(root), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("this account cannot make a link here: %v", err)
	}
	if _, err := NewSocketDir(link); !errors.Is(err, ErrSocketRoot) || !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a root that is a link: %v", err)
	}
}

// INF-EL2: a socket root that is a file is refused.
func TestASocketRootThatIsAFileIsRefused(t *testing.T) {
	root := socketParent(t)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSocketDir(root); !errors.Is(err, ErrSocketRoot) || !errors.Is(err, privatedir.ErrNotPrivate) {
		t.Fatalf("a root that is a file: %v", err)
	}
}
