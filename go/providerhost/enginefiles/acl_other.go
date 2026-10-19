//go:build !windows

package enginefiles

import "errors"

// Access is the account's right in a folder ACL; ACLs exist on Windows only.
type Access uint32

const (
	ReadOnly    Access = 0x1200A9
	FullControl Access = 0x1F01FF
)

var errNoACL = errors.New("enginefiles: folder ACLs exist on Windows only")

// AccountSID, SetFolderACL, FolderACLIs and OwnerSID report that folder ACLs
// exist on Windows only.
func AccountSID() (string, error)                      { return "", errNoACL }
func SetFolderACL(string, string, Access) error        { return errNoACL }
func FolderACLIs(string, string, Access) (bool, error) { return false, errNoACL }
func OwnerSID(string) (string, error)                  { return "", errNoACL }

// SystemACL outside Windows sets and checks nothing: no read-only folder ACL
// or share-mode equivalent is claimed there (INF-F5).
func SystemACL() ACL { return noACL{} }

// Installed is true outside Windows, where no folder ACL is set or checked.
func Installed(string) (bool, error) { return true, nil }

// Unlock and UnlockWithin have nothing to undo outside Windows.
func Unlock(string) error       { return nil }
func UnlockWithin(string) error { return nil }
