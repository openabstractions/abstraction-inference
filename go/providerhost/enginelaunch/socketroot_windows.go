//go:build windows

package enginelaunch

import (
	"fmt"
	"path/filepath"

	"github.com/openabstractions/abstraction-facade/go-core/bootstrap"
	"golang.org/x/sys/windows"
)

// profileView is this process's view of the account's profile folders,
// probed once (bootstrap.CurrentProfileView); tests replace it.
var profileView = bootstrap.CurrentProfileView

// SocketRoot is the fixed per-user folder engine launch directories live
// in, whatever state folder a provider uses: %LOCALAPPDATA%\oa\s, with the
// local application data folder read from the token's known folder and
// never from the environment (INF-EL2).
//
// A process descended from a packaged app with file virtualization, such as
// a shell of the Claude desktop app, sees a private copy of LocalAppData: a
// socket made there lands in Packages\<family>\LocalCache, and a Go AF_UNIX
// connect to it fails with WSAEINVAL. SocketRoot refuses such a view, and a
// view it could not read, before any folder is made (VISION 2026-09-17,
// "the runtime refuses a virtualized profile view"). A provider run there
// names its own socket folder outside the redirection.
func SocketRoot() (string, error) {
	view, err := profileView()
	if err != nil {
		return "", fmt.Errorf("%w: this process's profile view could not be read: %v", ErrSocketRoot, err)
	}
	if view.Virtualized {
		return "", fmt.Errorf("%w: this process runs inside a packaged app (%s), whose private copy of the local application data folder no engine socket can be reached in; name a socket folder outside it",
			ErrSocketRoot, view.Family)
	}
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", fmt.Errorf("%w: the local application data folder: %v", ErrSocketRoot, err)
	}
	if !filepath.IsAbs(local) {
		return "", fmt.Errorf("%w: the local application data folder %q is not absolute", ErrSocketRoot, local)
	}
	return filepath.Join(local, "oa", "s"), nil
}
