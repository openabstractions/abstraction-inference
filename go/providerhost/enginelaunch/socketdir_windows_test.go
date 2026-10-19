//go:build windows

package enginelaunch

import (
	"errors"
	"net"
	"os"
	"sort"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The explicit root is subject to the same transport proof as SocketRoot.
// A redirected profile may allow bind but refuse every dial; a failed proof
// leaves neither the launch directory nor its socket behind.
func TestExplicitSocketRootRefusesUnreachableTransport(t *testing.T) {
	previous := probeSocketTransport
	t.Cleanup(func() { probeSocketTransport = previous })
	probeSocketTransport = func(string) error { return errors.New("connect: invalid argument") }
	parent := socketParent(t)
	if _, err := NewSocketDir(parent); !errors.Is(err, ErrSocketTransport) {
		t.Fatalf("the explicit root: %v, want ErrSocketTransport", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed launch left entries %v: %v", entries, err)
	}
}

// daclEntries reads path's DACL as the SIDs of its entries, each with its
// ACE type, and whether the DACL is protected from inheritance.
func daclEntries(t *testing.T, path string) ([]string, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("no DACL on %s: %v", path, err)
	}
	var entries []string
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		kind := "allow"
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			kind = "other"
		}
		entries = append(entries, kind+" "+sid.String())
	}
	sort.Strings(entries)
	return entries, control&windows.SE_DACL_PROTECTED != 0
}

// INF-EL2: the launch directory and the socket the engine creates in it
// admit this account and SYSTEM alone, through a protected DACL, so another
// account can neither bind a socket there nor connect to the engine's.
func TestSocketDirAdmitsOnlyThisAccountAndSystem(t *testing.T) {
	d, err := NewSocketDir(socketParent(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"allow " + user.User.Sid.String(), "allow S-1-5-18"}
	sort.Strings(want)
	entries, protected := daclEntries(t, d.Dir)
	if !protected || len(entries) != 2 || entries[0] != want[0] || entries[1] != want[1] {
		t.Fatalf("launch directory DACL %v (protected %v), wanted exactly %v", entries, protected, want)
	}
	l, err := net.Listen("unix", d.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	entries, _ = daclEntries(t, d.Socket)
	if len(entries) != 2 || entries[0] != want[0] || entries[1] != want[1] {
		t.Fatalf("socket file DACL %v, wanted exactly %v", entries, want)
	}
}
