package enginelaunch

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

func names(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		out = append(out, name)
	}
	return out
}

// The provider's own entries join the environment; an empty value, a
// malformed entry, a name given twice in any case, an unknown name and a
// relative temporary folder are refused.
func TestBuildRefusals(t *testing.T) {
	temp := t.TempDir()
	if _, err := (CleanEnvironment{Temp: temp, Set: []string{"KEY=1"}}).Build(); err != nil {
		t.Fatalf("a plain environment was refused: %v", err)
	}
	for name, e := range map[string]CleanEnvironment{
		"relative temp":  {Temp: "temp"},
		"empty value":    {Temp: temp, Set: []string{"KEY="}},
		"malformed":      {Temp: temp, Set: []string{"KEY"}},
		"twice":          {Temp: temp, Set: []string{"KEY=1", "key=2"}},
		"core again":     {Temp: temp, Set: []string{"path=x"}},
		"unknown name":   {Temp: temp, Names: []string{"HTTPS_PROXY"}},
		"inherited name": {Temp: temp, Names: []string{"GGML_BACKEND_PATH"}},
	} {
		if _, err := e.Build(); !errors.Is(err, ErrEnvironment) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Outside Windows the environment is PATH of the system program folders,
// TMPDIR and the provider's own entries.
func TestBuildOutsideWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows layout is TestBuildIsTheWindowsLayout's")
	}
	t.Setenv("HTTPS_PROXY", "planted-by-test")
	temp := t.TempDir()
	env, err := CleanEnvironment{Temp: temp, Names: []string{"LOCALAPPDATA"}, Set: []string{"KEY=1"}}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(env, []string{"KEY=1", "PATH=/usr/bin:/bin", "TMPDIR=" + temp}) {
		t.Fatalf("environment names %q", names(env))
	}
}

// CheckLoopback refuses a tree that owns a socket beyond loopback, one
// whose listener is not observed, an owner table that cannot be read, and a
// missing port or tree, each wrapping identity.ErrBeyondLoopback.
func TestCheckLoopbackRefusals(t *testing.T) {
	self := func(pid int, _ time.Time) (bool, error) { return pid == os.Getpid(), nil }
	table := func(sockets ...identity.Socket) func() ([]identity.Socket, error) {
		return func() ([]identity.Socket, error) { return sockets, nil }
	}
	listener := identity.Socket{Proto: "tcp", Addr: netip.MustParseAddr("127.0.0.1"), Port: 41234, PID: os.Getpid()}
	wide := identity.Socket{Proto: "udp", Addr: netip.MustParseAddr("0.0.0.0"), Port: 5353, PID: os.Getpid()}
	for name, c := range map[string]struct {
		sockets  func() ([]identity.Socket, error)
		port     int
		contains identity.TreeContains
	}{
		"beyond loopback":  {table(listener, wide), 41234, self},
		"no listener":      {table(), 41234, self},
		"another port":     {table(listener), 41235, self},
		"unreadable table": {func() ([]identity.Socket, error) { return nil, errors.New("no table") }, 41234, self},
		"no port":          {table(listener), 0, self},
		"no tree":          {table(listener), 41234, nil},
	} {
		if err := CheckLoopback(c.sockets, c.port, c.contains); !errors.Is(err, identity.ErrBeyondLoopback) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The provider's own tests compare an exact environment by naming the
// Windows folder; the layout is a pure function of it.
func TestBuildIsTheWindowsLayout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths")
	}
	temp := filepath.Join(t.TempDir(), "engine-temp")
	env, err := CleanEnvironment{Temp: temp, WindowsDir: `C:\Windows`, WindowsDefaultPath: true, Set: []string{"KEY=k"}}.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"KEY=k", `PATH=C:\Windows\System32;C:\Windows;C:\Windows\System32\Wbem`, "SystemDrive=C:",
		`SystemRoot=C:\Windows`, "TEMP=" + temp, "TMP=" + temp, `windir=C:\Windows`}
	if !slices.Equal(env, want) {
		t.Fatalf("environment names %q", names(env))
	}
	env, err = CleanEnvironment{Temp: temp, WindowsDir: `C:\Windows`}.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, `PATH=C:\Windows\System32`) {
		t.Fatal("PATH is not System32 alone by default")
	}
}
