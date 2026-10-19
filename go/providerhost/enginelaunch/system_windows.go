//go:build windows

package enginelaunch

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func kernelWindowsDir() (string, error) { return windows.GetSystemWindowsDirectory() }

// SystemRegistry reads one string value from the registry of this process's
// account and machine, unexpanded.
func SystemRegistry(hive, path, name string) (string, error) {
	root := map[string]registry.Key{"HKLM": registry.LOCAL_MACHINE, "HKCU": registry.CURRENT_USER}[hive]
	if root == 0 {
		return "", fmt.Errorf("no registry hive %s", hive)
	}
	key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	defer key.Close()
	value, _, err := key.GetStringValue(name)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", path, name, err)
	}
	return value, nil
}

var procGetNativeSystemInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetNativeSystemInfo")

// systemInfo is SYSTEM_INFO (sysinfoapi.h).
type systemInfo struct {
	processorArchitecture     uint16
	reserved                  uint16
	pageSize                  uint32
	minimumApplicationAddress uintptr
	maximumApplicationAddress uintptr
	activeProcessorMask       uintptr
	numberOfProcessors        uint32
	processorType             uint32
	allocationGranularity     uint32
	processorLevel            uint16
	processorRevision         uint16
}

// processorArchitectures are the PROCESSOR_ARCHITECTURE values Windows
// itself writes, by SYSTEM_INFO.wProcessorArchitecture.
var processorArchitectures = map[uint16]string{0: "x86", 5: "ARM", 6: "IA64", 9: "AMD64", 12: "ARM64"}

func nativeSystemInfo() (systemInfo, error) {
	var info systemInfo
	if err := procGetNativeSystemInfo.Find(); err != nil {
		return info, err
	}
	//unchecked: GetNativeSystemInfo returns nothing; the fields are checked by the caller
	procGetNativeSystemInfo.Call(uintptr(unsafe.Pointer(&info)))
	return info, nil
}

// tokenProfile is this process token's profile directory (ProfileImagePath).
func tokenProfile() (string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", err
	}
	defer token.Close()
	return token.GetUserProfileDirectory()
}

// tokenAccount is this process token's account and domain names.
func tokenAccount() (string, string, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", "", err
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	return account, domain, err
}

const (
	profileList   = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
	shellFolders  = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	currentVer    = `SOFTWARE\Microsoft\Windows\CurrentVersion`
	sessionValues = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
)

// registryFolders are the variables Windows sets from one registry folder
// value: the hive, key and value name.
var registryFolders = map[string][3]string{
	"ProgramData":             {"HKLM", profileList, "ProgramData"},
	"ALLUSERSPROFILE":         {"HKLM", profileList, "ProgramData"},
	"PUBLIC":                  {"HKLM", profileList, "Public"},
	"LOCALAPPDATA":            {"HKCU", shellFolders, "Local AppData"},
	"APPDATA":                 {"HKCU", shellFolders, "AppData"},
	"ProgramFiles":            {"HKLM", currentVer, "ProgramFilesDir"},
	"ProgramFiles(x86)":       {"HKLM", currentVer, "ProgramFilesDir (x86)"},
	"ProgramW6432":            {"HKLM", currentVer, "ProgramW6432Dir"},
	"CommonProgramFiles":      {"HKLM", currentVer, "CommonFilesDir"},
	"CommonProgramFiles(x86)": {"HKLM", currentVer, "CommonFilesDir (x86)"},
	"CommonProgramW6432":      {"HKLM", currentVer, "CommonW6432Dir"},
	"ComSpec":                 {"HKLM", sessionValues, "ComSpec"},
}

// registryTexts are system variables the session manager writes from
// HKLM's Session Manager\Environment, which only administrators change.
var registryTexts = map[string]bool{"OS": true, "PATHEXT": true, "PROCESSOR_IDENTIFIER": true, "PROCESSOR_LEVEL": true, "PROCESSOR_REVISION": true}

// facts computes the Windows variables, each from the kernel, the token or
// the registry, never from this process's environment.
type facts struct {
	read    RegistryValue
	vars    map[string]string
	profile struct {
		value string
		err   error
		done  bool
	}
}

func newFacts(windowsDir string, read RegistryValue) *facts {
	return &facts{read: read, vars: map[string]string{
		"SystemRoot": windowsDir, "windir": windowsDir, "SystemDrive": filepath.VolumeName(windowsDir),
	}}
}

// userProfile is the token's profile folder; registry folders under it
// expand %USERPROFILE% with it once it is known.
func (f *facts) userProfile() (string, error) {
	if !f.profile.done {
		f.profile.done = true
		profile, err := tokenProfile()
		if err == nil {
			profile, err = expand(profile, f.vars, true)
		}
		f.profile.value, f.profile.err = profile, err
		if err == nil {
			f.vars["USERPROFILE"] = profile
		}
	}
	return f.profile.value, f.profile.err
}

func (f *facts) value(name string) (string, error) {
	//unchecked: a profile folder that cannot be read leaves %USERPROFILE% unexpanded, which expand refuses by name
	f.userProfile()
	if r, ok := registryFolders[name]; ok {
		raw, err := f.read(r[0], r[1], r[2])
		if err != nil {
			return "", err
		}
		return expand(raw, f.vars, true)
	}
	if registryTexts[name] {
		raw, err := f.read("HKLM", sessionValues, name)
		if err != nil {
			return "", err
		}
		return expand(raw, f.vars, false)
	}
	switch name {
	case "USERPROFILE":
		return f.userProfile()
	case "HOMEDRIVE", "HOMEPATH":
		profile, err := f.userProfile()
		if err != nil {
			return "", err
		}
		volume := filepath.VolumeName(profile)
		if volume == "" || !strings.HasPrefix(profile[len(volume):], `\`) {
			return "", errForeignVariable
		}
		if name == "HOMEDRIVE" {
			return volume, nil
		}
		return profile[len(volume):], nil
	case "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE":
		info, err := nativeSystemInfo()
		if err != nil {
			return "", err
		}
		if name == "NUMBER_OF_PROCESSORS" {
			if info.numberOfProcessors == 0 {
				return "", errors.New("GetNativeSystemInfo reports no processor")
			}
			return strconv.FormatUint(uint64(info.numberOfProcessors), 10), nil
		}
		architecture, ok := processorArchitectures[info.processorArchitecture]
		if !ok {
			return "", errors.New("GetNativeSystemInfo reports an unknown architecture")
		}
		return architecture, nil
	case "COMPUTERNAME":
		return windows.ComputerName()
	case "USERNAME", "USERDOMAIN":
		account, domain, err := tokenAccount()
		if err != nil {
			return "", err
		}
		if name == "USERNAME" {
			return account, nil
		}
		return domain, nil
	}
	return "", fmt.Errorf("%s is no variable the builder computes", name)
}
