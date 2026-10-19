//go:build windows

package enginelaunch

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"golang.org/x/sys/windows"
)

// Every computed variable comes from the kernel, the token or the registry:
// the same names planted in this process's environment never reach the
// engine. The test prints names, never values.
func TestBuildComputesEveryNameFromTheSystem(t *testing.T) {
	for _, name := range append(Computed(), "SystemDrive", "PATH", "GGML_BACKEND_PATH", "HTTPS_PROXY") {
		t.Setenv(name, "planted-by-test")
	}
	temp := t.TempDir()
	var diagnostics []error
	env, err := CleanEnvironment{Temp: temp, Names: Computed(), WindowsDefaultPath: true,
		OnError: func(err error) { diagnostics = append(diagnostics, err) }}.Build()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		if strings.Contains(value, "planted-by-test") {
			t.Fatalf("%s came from the provider's environment", name)
		}
		values[name] = value
	}
	for _, d := range diagnostics {
		t.Logf("left out: %v", d)
	}
	windowsDir, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }
	if !same(values["SystemRoot"], windowsDir) || !same(values["windir"], windowsDir) || values["SystemDrive"] != filepath.VolumeName(windowsDir) {
		t.Fatal("SystemRoot, windir and SystemDrive are not the kernel's Windows folder")
	}
	if values["PATH"] != filepath.Join(windowsDir, "System32")+";"+windowsDir+";"+filepath.Join(windowsDir, "System32", "Wbem") {
		t.Fatal("PATH is not Windows' default system PATH")
	}
	if values["TEMP"] != temp || values["TMP"] != temp {
		t.Fatal("TEMP and TMP are not the private folder")
	}
	for _, name := range []string{"USERPROFILE", "LOCALAPPDATA", "APPDATA", "ProgramData", "ALLUSERSPROFILE", "PUBLIC",
		"ProgramFiles", "ProgramW6432", "CommonProgramFiles", "CommonProgramW6432", "ComSpec"} {
		if !filepath.IsAbs(values[name]) {
			t.Fatalf("%s is not an absolute folder (diagnostics %d)", name, len(diagnostics))
		}
	}
	if values["HOMEDRIVE"]+values["HOMEPATH"] != values["USERPROFILE"] || values["ProgramData"] != values["ALLUSERSPROFILE"] {
		t.Fatal("HOMEDRIVE and HOMEPATH do not make USERPROFILE, or ALLUSERSPROFILE is not ProgramData")
	}
	if n, err := strconv.Atoi(values["NUMBER_OF_PROCESSORS"]); err != nil || n < 1 {
		t.Fatal("NUMBER_OF_PROCESSORS is not a count")
	}
	if values["OS"] != "Windows_NT" || values["COMPUTERNAME"] == "" || values["USERNAME"] == "" || values["USERDOMAIN"] == "" ||
		!strings.Contains(strings.ToUpper(values["PATHEXT"]), ".EXE") || values["PROCESSOR_ARCHITECTURE"] == "" {
		t.Fatal("a system variable is missing or malformed")
	}
	if _, ok := values["GGML_BACKEND_PATH"]; ok {
		t.Fatal("an inherited variable reached the environment")
	}
}

// A registry value that names a variable the builder does not compute (a
// redirected folder under %HOMESHARE%) is left out with one diagnostic that
// names the variable and never the value.
func TestAFolderNamingAnotherVariableIsLeftOut(t *testing.T) {
	read := func(hive, path, name string) (string, error) {
		if name == "Local AppData" {
			return `%HOMESHARE%\redirected\AppData\Local`, nil
		}
		if name == "AppData" {
			return "", errors.New("not set")
		}
		return SystemRegistry(hive, path, name)
	}
	var diagnostics []string
	env, err := CleanEnvironment{Temp: t.TempDir(), Names: []string{"LOCALAPPDATA", "APPDATA", "USERPROFILE"}, Registry: read,
		OnError: func(err error) { diagnostics = append(diagnostics, err.Error()) }}.Build()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(env), ",")
	if strings.Contains(got, "LOCALAPPDATA") || strings.Contains(got, "APPDATA") || !strings.Contains(got, "USERPROFILE") {
		t.Fatalf("environment names %s", got)
	}
	if len(diagnostics) != 2 || !strings.Contains(diagnostics[0], "LOCALAPPDATA") || !strings.Contains(diagnostics[1], "APPDATA") {
		t.Fatalf("diagnostics %q", diagnostics)
	}
	for _, d := range diagnostics {
		if strings.Contains(d, "HOMESHARE") || strings.Contains(d, "redirected") {
			t.Fatalf("a diagnostic carries the registry value: %q", d)
		}
	}
}

// CheckLoopback passes this process's own listener on 127.0.0.1 through the
// real owner tables, and refuses it once the tree also listens beyond
// loopback.
func TestCheckLoopbackReadsTheOwnerTables(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	self := func(pid int, _ time.Time) (bool, error) { return pid == os.Getpid(), nil }
	if err := CheckLoopback(nil, port, self); err != nil {
		t.Fatalf("a loopback listener was refused: %v", err)
	}
	wide := func() ([]identity.Socket, error) {
		sockets, err := identity.Sockets()
		return append(sockets, identity.Socket{Proto: "tcp", Addr: netip.MustParseAddr("0.0.0.0"), Port: port, PID: os.Getpid()}), err
	}
	if err := CheckLoopback(wide, port, self); !errors.Is(err, identity.ErrBeyondLoopback) {
		t.Fatalf("a listener beyond loopback passed: %v", err)
	}
}
