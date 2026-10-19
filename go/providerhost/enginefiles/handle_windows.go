//go:build windows

package enginefiles

import (
	"errors"
	"io"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Handle is one held file or folder: opened for reading with FILE_SHARE_READ
// alone, so while it is open Windows refuses to open the same file or folder
// for writing or deleting, which is what a write, rename or delete needs.
type Handle struct {
	h    windows.Handle
	name string
}

// Name is the path the handle was opened by.
func (h *Handle) Name() string { return h.name }

// extended gives an absolute path the \\?\ prefix, so paths beyond MAX_PATH
// open as Go's own os.Open would open them.
func extended(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\`):
		return p
	case strings.HasPrefix(p, `\\`):
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

func openHeld(p string, dir bool) (*Handle, error) {
	name, err := windows.UTF16PtrFromString(extended(p))
	if err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_SEQUENTIAL_SCAN)
	if dir {
		flags = windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, err
	}
	return &Handle{h: h, name: p}, nil
}

// openAncestor opens a folder above a placed file, sharing read and write
// but not delete: while it is open the folder cannot be renamed, deleted or
// replaced by a junction, and other programs still list it and add to it. A
// reparse point is opened as itself, and checkDir refuses it.
func openAncestor(p string) (*Handle, error) {
	name, err := windows.UTF16PtrFromString(extended(p))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	return &Handle{h: h, name: p}, nil
}

func (h *Handle) close() error { return windows.CloseHandle(h.h) }

// rawFinalPath is GetFinalPathNameByHandleW's normalized DOS path of the
// open handle, with its \\?\ prefix.
func (h *Handle) rawFinalPath() (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h.h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// finalPath is the path Windows resolves the open handle to, as a plain
// DOS or UNC path an engine opens.
func (h *Handle) finalPath() (string, error) {
	raw, err := h.rawFinalPath()
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(raw, `\\?\UNC\`):
		return `\\` + raw[len(`\\?\UNC\`):], nil
	case strings.HasPrefix(raw, `\\?\`):
		return raw[len(`\\?\`):], nil
	}
	return raw, nil
}

// Read reads the file from its current position.
func (h *Handle) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	if err := windows.ReadFile(h.h, p, &n, nil); err != nil {
		if errors.Is(err, windows.ERROR_HANDLE_EOF) {
			return 0, io.EOF
		}
		return int(n), err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

// FILE_BASIC_INFO, FILE_STANDARD_INFO and FILE_ID_INFO of
// GetFileInformationByHandleEx.
type fileBasicInfo struct {
	CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
	FileAttributes                                          uint32
	_                                                       uint32
}

type fileStandardInfo struct {
	AllocationSize, EndOfFile int64
	NumberOfLinks             uint32
	DeletePending, Directory  byte
	_                         [2]byte
}

type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func (h *Handle) info(class uint32, out unsafe.Pointer, size uintptr) error {
	return windows.GetFileInformationByHandleEx(h.h, class, (*byte)(out), uint32(size))
}

func (h *Handle) basic() (fileBasicInfo, error) {
	var b fileBasicInfo
	err := h.info(windows.FileBasicInfo, unsafe.Pointer(&b), unsafe.Sizeof(b))
	return b, err
}

func (h *Handle) checkDir() error {
	b, err := h.basic()
	if err != nil {
		return err
	}
	if b.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || b.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errNotRegular
	}
	return nil
}

func (h *Handle) fileMeta() (meta, error) {
	b, err := h.basic()
	if err != nil {
		return meta{}, err
	}
	if b.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DEVICE) != 0 {
		return meta{}, errNotRegular
	}
	var s fileStandardInfo
	if err := h.info(windows.FileStandardInfo, unsafe.Pointer(&s), unsafe.Sizeof(s)); err != nil {
		return meta{}, err
	}
	var id fileIDInfo
	if err := h.info(windows.FileIdInfo, unsafe.Pointer(&id), unsafe.Sizeof(id)); err != nil {
		return meta{}, err
	}
	return meta{id: id.FileID, volume: id.VolumeSerialNumber, size: s.EndOfFile, write: b.LastWriteTime, change: b.ChangeTime}, nil
}

func (h *Handle) rootID() (rootID, error) {
	var id fileIDInfo
	if err := h.info(windows.FileIdInfo, unsafe.Pointer(&id), unsafe.Sizeof(id)); err != nil {
		return rootID{}, err
	}
	final, err := h.rawFinalPath()
	if err != nil {
		return rootID{}, err
	}
	return rootID{final: final, volume: id.VolumeSerialNumber}, nil
}
