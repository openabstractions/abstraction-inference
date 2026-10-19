//go:build windows

package enginelaunch

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// longPathsEnabled reports whether
// HKLM\SYSTEM\CurrentControlSet\Control\FileSystem\LongPathsEnabled is the
// DWORD 1. A missing key or value, another type or a read error count as off.
func longPathsEnabled() bool {
	keyPath, err := syscall.UTF16PtrFromString(`SYSTEM\CurrentControlSet\Control\FileSystem`)
	if err != nil {
		return false
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, keyPath, 0, syscall.KEY_READ, &key); err != nil {
		return false
	}
	defer syscall.RegCloseKey(key)
	name, err := syscall.UTF16PtrFromString("LongPathsEnabled")
	if err != nil {
		return false
	}
	var kind, value uint32
	size := uint32(unsafe.Sizeof(value))
	if err := syscall.RegQueryValueEx(key, name, nil, &kind, (*byte)(unsafe.Pointer(&value)), &size); err != nil {
		return false
	}
	return kind == syscall.REG_DWORD && size == 4 && value == 1
}

// shortPathName returns GetShortPathNameW of an existing path. On a volume
// without 8.3 names it returns the long path unchanged.
func shortPathName(long string) (string, error) {
	in, err := windows.UTF16PtrFromString(long)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 260)
	for {
		n, err := windows.GetShortPathName(in, &buf[0], uint32(len(buf)))
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", windows.ERROR_INVALID_NAME
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n)
	}
}

// Flags of GetFinalPathNameByHandleW.
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// finalPathName opens a folder and returns GetFinalPathNameByHandleW of the
// handle, normalized, with its DOS drive letter (\\?\C:\...).
func finalPathName(folder string) (string, error) {
	name, err := windows.UTF16PtrFromString(folder)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 300)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		buf = make([]uint16, n+1)
	}
}

// isReparsePoint reports whether the path itself carries
// FILE_ATTRIBUTE_REPARSE_POINT (a junction, symbolic link or mount point).
func isReparsePoint(p string) (bool, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
