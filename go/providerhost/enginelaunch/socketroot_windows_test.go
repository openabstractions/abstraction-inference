//go:build windows

package enginelaunch

import (
	"errors"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
)

// A process in an app's redirected profile must refuse the default root
// before a provider can make a socket there. The profile authority owns the
// probe; the engine kit consumes its answer.
func TestSocketRootRefusesVirtualizedProfile(t *testing.T) {
	previous := profileView
	t.Cleanup(func() { profileView = previous })
	profileView = func() (bootstrap.ProfileView, error) {
		return bootstrap.ProfileView{Virtualized: true, Family: "Fixture.Package_123"}, nil
	}
	root, err := SocketRoot()
	if root != "" || !errors.Is(err, ErrSocketRoot) || !strings.Contains(err.Error(), "Fixture.Package_123") {
		t.Fatalf("root=%q err=%v; want a typed refusal naming the package", root, err)
	}
}

func TestSocketRootRefusesUnknownProfileView(t *testing.T) {
	previous := profileView
	t.Cleanup(func() { profileView = previous })
	profileView = func() (bootstrap.ProfileView, error) { return bootstrap.ProfileView{}, errors.New("probe failed") }
	root, err := SocketRoot()
	if root != "" || !errors.Is(err, ErrSocketRoot) || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("root=%q err=%v; want a typed probe refusal", root, err)
	}
}

func TestSocketRootUsesRealProfileView(t *testing.T) {
	previous := profileView
	t.Cleanup(func() { profileView = previous })
	profileView = func() (bootstrap.ProfileView, error) { return bootstrap.ProfileView{}, nil }
	root, err := SocketRoot()
	if err != nil || !strings.HasSuffix(root, `\oa\s`) {
		t.Fatalf("root=%q err=%v; want the fixed account root", root, err)
	}
}
