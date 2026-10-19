//go:build unix

package enginefiles

import (
	"fmt"
	"io"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Handle is one held file or folder, open for reading. Outside Windows an
// open descriptor blocks no write, rename or delete; it keeps the opened
// file's content reachable to the provider and nothing more.
type Handle struct {
	fd   int
	name string
}

// Name is the path the handle was opened by.
func (h *Handle) Name() string { return h.name }

func openHeld(p string, dir bool) (*Handle, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if dir {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Open(p, flags, 0)
	if err != nil {
		return nil, err
	}
	return &Handle{fd: fd, name: p}, nil
}

// openAncestor opens a folder above a placed file. O_NOFOLLOW refuses a
// symbolic link, which the refusal names as a link on the path (INF-F7):
// the open itself answers ENOTDIR (macOS) or ELOOP, as for /tmp or /var on
// macOS. The descriptor blocks no rename outside Windows (INF-F5).
func openAncestor(p string) (*Handle, error) {
	h, err := openHeld(p, true)
	if err == unix.ENOTDIR || err == unix.ELOOP {
		var st unix.Stat_t
		if unix.Lstat(p, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return nil, fmt.Errorf("%w: %s", errLinkOnPath, p)
		}
	}
	return h, err
}

func (h *Handle) close() error { return unix.Close(h.fd) }

// finalPath is the path the handle was opened by: openHeld and
// openAncestor follow no symbolic link along it.
func (h *Handle) finalPath() (string, error) { return h.name, nil }

// Read reads the file from its current position.
func (h *Handle) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, err := unix.Read(h.fd, p)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	}
}

func (h *Handle) stat() (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstat(h.fd, &st)
	return st, err
}

func (h *Handle) checkDir() error {
	st, err := h.stat()
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errNotRegular
	}
	return nil
}

func (h *Handle) fileMeta() (meta, error) {
	st, err := h.stat()
	if err != nil {
		return meta{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return meta{}, errNotRegular
	}
	m := meta{volume: uint64(st.Dev), size: st.Size, write: unix.TimespecToNsec(st.Mtim), change: unix.TimespecToNsec(st.Ctim)}
	ino := uint64(st.Ino)
	for i := 0; i < 8; i++ {
		m.id[15-i] = byte(ino >> (8 * i))
	}
	return m, nil
}

func (h *Handle) rootID() (rootID, error) {
	st, err := h.stat()
	if err != nil {
		return rootID{}, err
	}
	final, err := filepath.EvalSymlinks(h.name)
	if err != nil {
		return rootID{}, err
	}
	return rootID{final: final, volume: uint64(st.Dev)}, nil
}
