//go:build windows

package enginefiles

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Access is the account's right in a folder ACL.
type Access uint32

const (
	// ReadOnly is FILE_GENERIC_READ | FILE_GENERIC_EXECUTE: read, execute
	// and list.
	ReadOnly Access = 0x1200A9
	// FullControl is FILE_ALL_ACCESS, the SDDL right FA.
	FullControl Access = 0x1F01FF
)

const (
	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"
)

var account = sync.OnceValues(func() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
})

// AccountSID is the SID of the process's account.
func AccountSID() (string, error) { return account() }

// folderSDDL is a protected DACL of three entries inherited by files and
// folders: the account with access, SYSTEM and Administrators with full
// control.
func folderSDDL(sid string, access Access) string {
	return fmt.Sprintf("D:P(A;OICI;0x%x;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)", uint32(access), sid)
}

func folderDescriptor(sid string, access Access) (*windows.SECURITY_DESCRIPTOR, *windows.ACL, error) {
	sd, err := windows.SecurityDescriptorFromString(folderSDDL(sid, access))
	if err != nil {
		return nil, nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, nil, err
	}
	return sd, dacl, nil
}

// SetFolderACL gives folder the protected three-entry DACL and propagates
// its inherited entries to every file and folder below it.
func SetFolderACL(folder, sid string, access Access) error {
	_, dacl, err := folderDescriptor(sid, access)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(folder, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// setOwnFolderACL writes the same DACL to one folder through a handle,
// without propagating to its children.
func setOwnFolderACL(folder, sid string, access Access) error {
	sd, _, err := folderDescriptor(sid, access)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(extended(folder))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.SetKernelObjectSecurity(h, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sd)
}

// FolderACLIs reports whether folder's DACL is exactly the one SetFolderACL
// writes for sid and access.
func FolderACLIs(folder, sid string, access Access) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(folder, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	return descriptorIs(sd, sid, access)
}

func descriptorIs(sd *windows.SECURITY_DESCRIPTOR, sid string, access Access) (bool, error) {
	control, _, err := sd.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false, err
	}
	want := map[string]Access{sid: access, sidSystem: FullControl, sidAdministrators: FullControl}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false, err
		}
		// SidStart is where an ACCESS_ALLOWED_ACE keeps its SID; any other
		// type is refused before it is read.
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false, nil
		}
		who := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		mask, ok := want[who]
		if !ok || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || Access(ace.Mask) != mask {
			return false, nil
		}
		delete(want, who)
	}
	return len(want) == 0, nil
}

// OwnerSID is the owner SID of a file or folder.
func OwnerSID(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", err
	}
	return owner.String(), nil
}

// SystemACL sets and checks the read-only ACL: the account may read, execute
// and list; SYSTEM and Administrators have full control; every folder's DACL
// is protected from its parent's.
func SystemACL() ACL { return systemACL{} }

type systemACL struct{}

// Restrict writes the read-only DACL to the tree's folder, which propagates
// the inherited entries to every file and folder below it, and then to each
// subfolder as its own protected DACL.
func (systemACL) Restrict(folders []string) error {
	if len(folders) == 0 {
		return nil
	}
	sid, err := AccountSID()
	if err != nil {
		return err
	}
	if err := SetFolderACL(folders[0], sid, ReadOnly); err != nil {
		return fmt.Errorf("%s: %w", folders[0], err)
	}
	for _, folder := range folders[1:] {
		if err := setOwnFolderACL(folder, sid, ReadOnly); err != nil {
			return fmt.Errorf("%s: %w", folder, err)
		}
	}
	return nil
}

func (systemACL) Check(folder string, held *Handle) error {
	sid, err := AccountSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(held.h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("%s (%v): %w", folder, err, ErrFolderACL)
	}
	ok, err := descriptorIs(sd, sid, ReadOnly)
	if err != nil || !ok {
		return fmt.Errorf("%s: %w", folder, ErrFolderACL)
	}
	return nil
}

// Installed reports whether every folder of the tree at root has the
// read-only ACL Install sets. Setup probes use it to offer the install step
// again.
func Installed(root string) (bool, error) {
	sid, err := AccountSID()
	if err != nil {
		return false, err
	}
	installed := true
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type() != fs.ModeDir {
			return nil
		}
		ok, err := FolderACLIs(p, sid, ReadOnly)
		if err != nil {
			return err
		}
		if !ok {
			installed = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return installed, nil
}

// Unlock gives the tree at root back to the account so it can be replaced or
// removed. Every subfolder loses its own entries and its protection, and the
// tree's folder gets the account's full control, which propagates to every
// file and folder below it. A later Install's propagation from the tree's
// folder then reaches every file again, including files created after Unlock.
func Unlock(root string) error {
	sid, err := AccountSID()
	if err != nil {
		return err
	}
	var folders []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type() == fs.ModeDir {
			folders = append(folders, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(folders) == 0 {
		return nil
	}
	// Parents before children: each subfolder inherits from a parent that is
	// already open, and the propagation of each call stops at the subfolders
	// below it, which are still protected.
	if err := SetFolderACL(folders[0], sid, FullControl); err != nil {
		return fmt.Errorf("%s: %w", folders[0], err)
	}
	for _, folder := range folders[1:] {
		if err := inheritOnly(folder); err != nil {
			return fmt.Errorf("%s: %w", folder, err)
		}
	}
	return nil
}

// UnlockWithin gives back every read-only tree at or below root, as Unlock
// does for one tree: each folder whose DACL is the read-only one Install
// sets is unlocked with everything below it. A folder outside every tree
// keeps its DACL. A remover that deletes a folder which may hold an
// installed tree, such as an uninstaller or a retired package version,
// calls it first; a tree a killed Unlock left half open is found by its
// subfolders. A missing root has nothing to unlock.
func UnlockWithin(root string) error {
	sid, err := AccountSID()
	if err != nil {
		return err
	}
	var trees []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.Type() != fs.ModeDir {
			return nil
		}
		ok, err := FolderACLIs(p, sid, ReadOnly)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if ok {
			trees = append(trees, p)
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, tree := range trees {
		if err := Unlock(tree); err != nil {
			return err
		}
	}
	return nil
}

// inheritOnly writes one folder an unprotected DACL with no entries of its
// own; it then holds what its parent passes down, and so do its files.
func inheritOnly(folder string) error {
	sd, err := windows.SecurityDescriptorFromString("D:")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(folder, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
