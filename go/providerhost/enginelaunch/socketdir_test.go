package enginelaunch

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-identity/go/privatedir"

	identity "github.com/openabstractions/abstraction-identity"
)

// socketParent is a socket root not yet made, in a short folder of the
// temporary directory; NewSocketDir makes it private. The names t.TempDir
// makes can pass the 107-byte socket path limit.
func socketParent(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "oa-el")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// INF-EL2: each launch gets a fresh directory and a socket path in it that
// fits sun_path; Remove deletes the directory with the socket the engine
// left there.
func TestSocketDirIsFreshAndRemoved(t *testing.T) {
	parent := socketParent(t)
	first, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if first.Dir == second.Dir {
		t.Fatal("two launches share one directory")
	}
	if filepath.Dir(first.Socket) != first.Dir || filepath.Base(first.Socket) != SocketName || len(first.Socket) > MaxSocketPath {
		t.Fatalf("socket path %q in %q", first.Socket, first.Dir)
	}
	if entries, err := os.ReadDir(first.Dir); err != nil || len(entries) != 1 || entries[0].Name() != heldName {
		t.Fatalf("a new launch directory holds %v (%v), wanted its held file alone", entries, err)
	}
	l, err := net.Listen("unix", first.Socket)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := first.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(first.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the launch directory outlived Remove: %v", err)
	}
	if err := second.Remove(); err != nil {
		t.Fatal(err)
	}
}

// INF-EL2: a parent that is relative, a socket path past MaxSocketPath and
// one outside printable ASCII are refused before anything is created.
func TestSocketDirRefusesPathsTheEngineCannotBind(t *testing.T) {
	parent := socketParent(t)
	long := filepath.Join(parent, strings.Repeat("p", MaxSocketPath))
	for name, p := range map[string]string{
		"relative":  "engine-sockets",
		"too long":  long,
		"non-ASCII": filepath.Join(parent, "é"),
	} {
		if _, err := NewSocketDir(p); !errors.Is(err, ErrSocketPath) {
			t.Fatalf("%s: %v", name, err)
		}
		if err := SocketPathFits(p); !errors.Is(err, ErrSocketPath) {
			t.Fatalf("%s fits: %v", name, err)
		}
	}
	if _, err := os.Lstat(long); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused parent was created: %v", err)
	}
	if err := SocketPathFits(parent); err != nil {
		t.Fatal(err)
	}
}

// INF-EL2: a launch directory never takes over an existing path.
func TestSocketDirRefusesAnExistingDirectory(t *testing.T) {
	dir := filepath.Join(socketParent(t), "0123abcd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privatedir.Make(dir); !errors.Is(err, os.ErrExist) {
		t.Fatalf("an existing directory: %v", err)
	}
}

// INF-EL2: the sweep removes a launch directory whose socket no longer
// answers (a crashed engine's) and one with no socket, and keeps a directory
// whose socket still answers and every name the kit did not make.
func TestSweepRemovesStaleLaunchDirectories(t *testing.T) {
	parent := socketParent(t)
	stale, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := net.Listen("unix", stale.Socket)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()
	if _, err := os.Lstat(stale.Socket); err != nil {
		t.Fatalf("the crashed engine's socket file is missing: %v", err)
	}
	empty, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	live, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", live.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	foreign := filepath.Join(parent, "not-a-launch")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	// The providers of stale and empty stopped without cleaning up: the
	// system released their locks.
	if err := stale.lock.release(); err != nil {
		t.Fatal(err)
	}
	if err := empty.lock.release(); err != nil {
		t.Fatal(err)
	}
	if errs := SweepSocketDirs(parent); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, gone := range []string{stale.Dir, empty.Dir} {
		if _, err := os.Lstat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the sweep kept %s: %v", gone, err)
		}
	}
	for _, kept := range []string{live.Socket, foreign} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("the sweep removed %s: %v", kept, err)
		}
	}
	if errs := SweepSocketDirs(filepath.Join(parent, "missing")); len(errs) != 0 {
		t.Fatal(errs)
	}
}

// INF-EL2: one check serves both transports. A socket engine (port 0) is
// refused for a socket a loopback engine may hold; a loopback engine is
// refused when its own listener is missing.
func TestCheckEngineSocketsPicksTheTransportRule(t *testing.T) {
	self := func(pid int, _ time.Time) (bool, error) { return pid == os.Getpid(), nil }
	none := func() ([]identity.Socket, error) { return nil, nil }
	if err := CheckEngineSockets(none, none, self, 0); err != nil {
		t.Fatalf("a socket engine with no sockets: %v", err)
	}
	if err := CheckEngineSockets(none, none, self, 41234); !errors.Is(err, identity.ErrBeyondLoopback) {
		t.Fatalf("a loopback engine with no listener: %v", err)
	}
	if runtime.GOOS != "windows" {
		t.Skip("tree membership reads process creation times, which identity reads on Windows only")
	}
	listener := func() ([]identity.Socket, error) {
		return []identity.Socket{{Proto: "tcp", Addr: netip.MustParseAddr("127.0.0.1"), Port: 41234, PID: os.Getpid()}}, nil
	}
	if err := CheckEngineSockets(listener, none, self, 41234); err != nil {
		t.Fatalf("a loopback engine on its port: %v", err)
	}
	if err := CheckEngineSockets(none, listener, self, 0); !errors.Is(err, ErrNetworkSocket) {
		t.Fatalf("a socket engine holding a loopback listener: %v", err)
	}
}

// INF-EL2: an engine serving on a socket path owns no TCP listener and no
// bound UDP socket on any address, loopback included; an unreadable table or
// no tree refuses.
func TestCheckSocketOnly(t *testing.T) {
	self := func(pid int, _ time.Time) (bool, error) { return pid == os.Getpid(), nil }
	table := func(sockets ...identity.Socket) func() ([]identity.Socket, error) {
		return func() ([]identity.Socket, error) { return sockets, nil }
	}
	other := identity.Socket{Proto: "tcp", Addr: netip.MustParseAddr("127.0.0.1"), Port: 41234, PID: 0}
	if err := CheckSocketOnly(table(other), self); err != nil {
		t.Fatalf("a socket outside the tree: %v", err)
	}
	if err := CheckSocketOnly(table(), self); err != nil {
		t.Fatalf("no sockets: %v", err)
	}
	for name, c := range map[string]struct {
		sockets  func() ([]identity.Socket, error)
		contains identity.TreeContains
	}{
		"unreadable table": {func() ([]identity.Socket, error) { return nil, errors.New("no table") }, self},
		"no tree":          {table(), nil},
	} {
		if err := CheckSocketOnly(c.sockets, c.contains); !errors.Is(err, ErrNetworkSocket) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if runtime.GOOS != "windows" {
		t.Skip("tree membership reads process creation times, which identity reads on Windows only")
	}
	for name, s := range map[string]identity.Socket{
		"loopback listener": {Proto: "tcp", Addr: netip.MustParseAddr("127.0.0.1"), Port: 41234, PID: os.Getpid()},
		"wide udp":          {Proto: "udp", Addr: netip.MustParseAddr("0.0.0.0"), Port: 5353, PID: os.Getpid()},
		"ipv6 loopback":     {Proto: "tcp", Addr: netip.MustParseAddr("::1"), Port: 41234, PID: os.Getpid()},
	} {
		if err := CheckSocketOnly(table(other, s), self); !errors.Is(err, ErrNetworkSocket) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// INF-EL2: two providers share one socket root. A launch directory whose
// engine has not bound its socket yet is held by its provider, and the
// other provider's sweep keeps it.
func TestSweepKeepsADirectoryItsProviderHolds(t *testing.T) {
	parent := socketParent(t)
	starting, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if errs := SweepSocketDirs(parent); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := os.Lstat(starting.Dir); err != nil {
		t.Fatalf("the sweep removed a held launch directory: %v", err)
	}
	if !held(starting.Dir) {
		t.Fatal("a held launch directory reads free")
	}
	if err := starting.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(starting.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove left the launch directory: %v", err)
	}
}

// INF-EL2: a launch directory whose lock was released, its provider gone,
// stays while its socket still answers a dial: an engine that outlived its
// provider keeps its directory.
func TestSweepKeepsADirectoryWhoseSocketAnswers(t *testing.T) {
	parent := socketParent(t)
	orphan, err := NewSocketDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", orphan.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := orphan.lock.release(); err != nil {
		t.Fatal(err)
	}
	if errs := SweepSocketDirs(parent); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := os.Lstat(orphan.Socket); err != nil {
		t.Fatalf("the sweep removed a directory whose socket answers: %v", err)
	}
}
