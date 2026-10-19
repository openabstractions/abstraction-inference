//go:build windows || linux

package enginehttp

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
)

// get asks the engine at path once through a socket client.
func get(t *testing.T, path string) error {
	t.Helper()
	client, err := NewUnixClient(path, func(identity.Process) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+UnixHost+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		t.Fatalf("a request to %s was answered", path)
	}
	return err
}

// A socket file the engine has not made yet is the engine still starting;
// the connect error is returned as it is.
func TestUnixDialOfAMissingSocketIsNotUnreachable(t *testing.T) {
	dir, err := os.MkdirTemp("", "oa-eh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := get(t, filepath.Join(dir, "e.sock")); errors.Is(err, ErrEngineUnreachable) {
		t.Fatalf("a missing socket read unreachable: %v", err)
	}
}

// A socket path the client can never connect to is unreachable and names
// the system's own error. On Windows a folder in the socket's place reads
// WSAEINVAL, the error a packaged app's redirected folder gives
// (2026-10-03); elsewhere a launch folder that is gone reads ENOTDIR.
func TestUnixDialThatCannotSucceedIsUnreachable(t *testing.T) {
	dir, err := os.MkdirTemp("", "oa-eh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path, want := dir, "invalid argument"
	if runtime.GOOS != "windows" {
		file := filepath.Join(dir, "plain")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path, want = filepath.Join(file, "e.sock"), "not a directory"
	}
	err = get(t, path)
	if !errors.Is(err, ErrEngineUnreachable) {
		t.Fatalf("got %v, want ErrEngineUnreachable", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), want) {
		t.Fatalf("%q does not name the system error %q", err, want)
	}
}

// dialFailure keeps the not-yet-listening errors as they are and wraps
// every other system error.
func TestDialFailureClassifiesSystemErrors(t *testing.T) {
	for _, errno := range notListeningErrnos {
		if err := dialFailure(errno); errors.Is(err, ErrEngineUnreachable) {
			t.Errorf("%d (%v) read unreachable", uintptr(errno), errno)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, errors.New("no system error")} {
		if errors.Is(dialFailure(err), ErrEngineUnreachable) {
			t.Errorf("%v read unreachable", err)
		}
	}
}
