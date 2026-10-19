// Package enginelaunch holds the launch hardening every provider that starts
// a third-party engine shares (inference CONTRACT.md INF-EL1, INF-EL2): the
// clean environment the engine starts with, built from the kernel, the
// process token and the registry and never from the provider's own
// environment, the private launch directory an engine's socket lives in, and
// the check after readiness that the engine's process tree holds no network
// socket (CheckSocketOnly) or, for an engine that serves TCP only, listens on
// loopback only (CheckLoopback). Each provider passes its own variable names and
// values; the forbidden-name lists of an engine family stay with that family
// (providerhost/llamacpp for llama.cpp).
package enginelaunch

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	identity "github.com/openabstractions/abstraction-identity"
)

// ErrEnvironment reports an environment the builder cannot make as asked: a
// relative or missing folder, an unknown name, an empty value or a name
// given twice.
var ErrEnvironment = errors.New("enginelaunch: engine environment refused")

// A RegistryValue reads one string value of the Windows registry. hive is
// "HKLM" or "HKCU"; the value is returned as stored, unexpanded.
type RegistryValue func(hive, path, name string) (string, error)

// CleanEnvironment describes the environment an engine starts with. Nothing
// is inherited from the provider's process.
//
// On Windows the environment holds SystemRoot and windir (the kernel's
// Windows folder), SystemDrive (its volume), PATH, TEMP and TMP (Temp), each
// of Names, and Set. Elsewhere it holds PATH=/usr/bin:/bin, TMPDIR (Temp) and
// Set; Names are Windows variables and are not delivered there.
type CleanEnvironment struct {
	// Temp is the engine's private temporary folder, an absolute path.
	Temp string
	// WindowsDefaultPath lists System32, the Windows folder and
	// System32\Wbem in PATH, the folders Windows' own system PATH starts
	// with. Without it PATH is System32 alone.
	WindowsDefaultPath bool
	// Names are the further Windows variables the engine receives, each
	// computed from the kernel, the token or the registry (Computed lists
	// them). A registry value is expanded only with variables the builder
	// computed. A variable that cannot be computed, or whose registry value
	// names another variable or no absolute folder, is left out, and
	// OnError receives one diagnostic naming it and never its value.
	Names []string
	// Set are the provider's own NAME=value entries: launch keys, reviewed
	// switches, private cache folders.
	Set []string
	// OnError receives each diagnostic of a variable left out.
	OnError func(error)
	// WindowsDir replaces the kernel's Windows folder; a provider's tests
	// set it to compare an exact environment.
	WindowsDir string
	// Registry replaces the registry reader; nil reads the registry.
	Registry RegistryValue
}

// computed are the Names the builder can compute, in their Windows spelling.
var computed = []string{
	"ALLUSERSPROFILE", "APPDATA", "COMPUTERNAME", "ComSpec",
	"CommonProgramFiles", "CommonProgramFiles(x86)", "CommonProgramW6432",
	"HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "NUMBER_OF_PROCESSORS", "OS",
	"PATHEXT", "PROCESSOR_ARCHITECTURE", "PROCESSOR_IDENTIFIER",
	"PROCESSOR_LEVEL", "PROCESSOR_REVISION", "ProgramData", "ProgramFiles",
	"ProgramFiles(x86)", "ProgramW6432", "PUBLIC", "USERDOMAIN", "USERNAME",
	"USERPROFILE",
}

// Computed lists the variable names CleanEnvironment.Names may hold.
func Computed() []string { return append([]string(nil), computed...) }

// Build returns the environment, sorted by name without letter case as
// CreateProcess expects an environment block.
func (e CleanEnvironment) Build() ([]string, error) {
	if !filepath.IsAbs(e.Temp) {
		return nil, fmt.Errorf("%w: the temporary folder %q is not absolute", ErrEnvironment, e.Temp)
	}
	spelled := map[string]string{}
	for _, name := range computed {
		spelled[strings.ToUpper(name)] = name
	}
	names := make([]string, 0, len(e.Names))
	for _, name := range e.Names {
		canonical, ok := spelled[strings.ToUpper(name)]
		if !ok {
			return nil, fmt.Errorf("%w: %s is no variable the builder computes", ErrEnvironment, name)
		}
		names = append(names, canonical)
	}
	var env []string
	if e.WindowsDir == "" && runtime.GOOS != "windows" {
		env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + e.Temp}
	} else {
		windowsDir := e.WindowsDir
		if windowsDir == "" {
			dir, err := kernelWindowsDir()
			if err != nil {
				return nil, fmt.Errorf("%w: Windows folder: %v", ErrEnvironment, err)
			}
			windowsDir = dir
		}
		volume := filepath.VolumeName(windowsDir)
		if !filepath.IsAbs(windowsDir) || volume == "" {
			return nil, fmt.Errorf("%w: the Windows folder is not an absolute path on a volume", ErrEnvironment)
		}
		system32 := filepath.Join(windowsDir, "System32")
		path := system32
		if e.WindowsDefaultPath {
			path = system32 + ";" + windowsDir + ";" + filepath.Join(system32, "Wbem")
		}
		env = []string{
			"SystemRoot=" + windowsDir,
			"windir=" + windowsDir,
			"SystemDrive=" + volume,
			"PATH=" + path,
			"TEMP=" + e.Temp,
			"TMP=" + e.Temp,
		}
		read := e.Registry
		if read == nil {
			read = SystemRegistry
		}
		f := newFacts(windowsDir, read)
		for _, name := range names {
			value, err := f.value(name)
			if err != nil {
				if e.OnError != nil {
					e.OnError(fmt.Errorf("enginelaunch: the engine starts without %s: %w", name, err))
				}
				continue
			}
			env = append(env, name+"="+value)
		}
	}
	env = append(env, e.Set...)
	seen := map[string]bool{}
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		if !ok || name == "" || value == "" || seen[upper] {
			return nil, fmt.Errorf("%w: %q is empty, malformed or given twice", ErrEnvironment, name)
		}
		seen[upper] = true
	}
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	return env, nil
}

// errForeignVariable reports a registry value that names a variable the
// builder does not compute, or expands to no absolute folder. It never
// carries the value.
var errForeignVariable = errors.New("its registry value names a variable this provider does not compute, or no absolute folder")

// expand replaces each %NAME% of a registry value with a variable the
// builder computed. Any other %NAME% is refused, so nothing in the
// provider's own environment decides a value; a folder must expand to an
// absolute path.
func expand(raw string, vars map[string]string, folder bool) (string, error) {
	out := raw
	for name, value := range vars {
		out = replaceFold(out, "%"+name+"%", value)
	}
	if out == "" || strings.Contains(out, "%") {
		return "", errForeignVariable
	}
	if folder {
		if !filepath.IsAbs(out) {
			return "", errForeignVariable
		}
		return filepath.Clean(out), nil
	}
	return out, nil
}

func replaceFold(s, old, value string) string {
	for {
		i := strings.Index(strings.ToUpper(s), strings.ToUpper(old))
		if i < 0 {
			return s
		}
		s = s[:i] + value + s[i+len(old):]
	}
}

// CheckLoopback applies abstraction-identity's loopback-only rule (ID-T7) to
// an engine's contained process tree: every socket the tree owns is on a
// loopback address, and the tree holds the TCP listener on 127.0.0.1 at
// port. Membership is proved by process id and creation time through
// contains, so a reused process id is never counted. sockets reads the
// socket owner tables; nil reads them through identity.Sockets, and a
// platform without them refuses. Every refusal wraps
// identity.ErrBeyondLoopback; the caller stops the tree.
func CheckLoopback(sockets func() ([]identity.Socket, error), port int, contains identity.TreeContains) error {
	if port < 1 || port > 65535 || contains == nil {
		return fmt.Errorf("%w: no port or process tree to check", identity.ErrBeyondLoopback)
	}
	if sockets == nil {
		sockets = identity.Sockets
	}
	owned, err := sockets()
	if err != nil {
		return fmt.Errorf("%w: owner tables: %v", identity.ErrBeyondLoopback, err)
	}
	listener := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(port))
	return identity.CheckLoopbackOnly(owned, identity.OwnedByTree(contains), listener)
}
