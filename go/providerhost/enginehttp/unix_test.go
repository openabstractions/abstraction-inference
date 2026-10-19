//go:build windows || linux

package enginehttp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

// TestEngineUnixHelperProcess is the stand-in engine of the socket tests: it
// serves HTTP on the AF_UNIX path it is given, records each request in its
// marker file, and prints one line once it listens.
func TestEngineUnixHelperProcess(t *testing.T) {
	path := os.Getenv("OA_ENGINEHTTP_UNIX_PATH")
	if path == "" {
		return
	}
	marker := os.Getenv("OA_ENGINEHTTP_UNIX_MARKER")
	l, err := net.Listen("unix", path)
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString("listening\n")
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile(marker, []byte("request"), 0o600)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	_ = server.Serve(l)
}

// unixEngine starts the stand-in on a socket in a short private folder and
// returns the socket path, the process abstraction-identity binds for it,
// and its marker file.
func unixEngine(t *testing.T) (string, identity.Process, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "oa-eh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "engine.sock")
	marker := filepath.Join(dir, "request-seen")
	child := exec.Command(os.Args[0], "-test.run=^TestEngineUnixHelperProcess$")
	child.Env = append(os.Environ(), "OA_ENGINEHTTP_UNIX_PATH="+path, "OA_ENGINEHTTP_UNIX_MARKER="+marker)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || strings.TrimSpace(line) != "listening" {
		t.Fatalf("the stand-in did not listen: %q %v", line, err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bound, err := identity.BindUnixServer(conn, &identity.Options{ConnectedAt: time.Now(), SkipCodeSignature: true})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	peer, err := bound.Peer()
	if err != nil {
		t.Fatal(err)
	}
	process, proof := peer.Process.Get()
	if proof < identity.ProofBound || process.PID != child.Process.Pid {
		t.Fatalf("the stand-in bound as %+v at %s, wanted pid %d", process, proof, child.Process.Pid)
	}
	return path, process, marker
}

func sameProcess(want identity.Process) func(identity.Process) (bool, error) {
	return func(got identity.Process) (bool, error) {
		return got.PID == want.PID && got.StartTime.Equal(want.StartTime) && !got.Recycled, nil
	}
}

// INF-EL2: the stand-in engine serving on a Unix socket is reached, and its
// server process passes the peer check before the request is sent.
func TestUnixClientReachesTheEngineItBound(t *testing.T) {
	path, engine, marker := unixEngine(t)
	client, err := NewUnixClient(path, sameProcess(engine))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+UnixHost+"/health", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(data) != `{"status":"ok"}` {
		t.Fatalf("status %d body %q err %v", response.StatusCode, data, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the engine saw no request: %v", err)
	}
}

// INF-EL2: a different process listening on the engine's socket path is
// refused by its process id before any byte of the request is sent.
func TestUnixClientRefusesAnotherServerOnThePath(t *testing.T) {
	_, engine, _ := unixEngine(t)
	impostorPath, _, impostorMarker := unixEngine(t)
	client, err := NewUnixClient(impostorPath, sameProcess(engine))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://"+UnixHost+"/v1/chat/completions", strings.NewReader(`{"secret":"prompt"}`))
	if response, err := client.Do(request); !errors.Is(err, ErrEnginePeer) {
		if response != nil {
			response.Body.Close()
		}
		t.Fatalf("a request to another server's socket: %v, wanted ErrEnginePeer", err)
	}
	if _, err := os.Stat(impostorMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the other server received the request: %v", err)
	}
}

// INF-EL2: the transport reaches only the hosts its resolver names, over
// plain http with no port, user or other scheme.
func TestUnixTransportConfinesRequests(t *testing.T) {
	path, engine, marker := unixEngine(t)
	client := &http.Client{Transport: NewUnixTransport(func(host string) (UnixEndpoint, bool) {
		return UnixEndpoint{Path: path, Verify: sameProcess(engine)}, host == "engine0.modelhost.invalid"
	})}
	for _, target := range []string{
		"http://other.invalid/health",
		"https://engine0.modelhost.invalid/health",
		"http://engine0.modelhost.invalid:8080/health",
		"http://user@engine0.modelhost.invalid/health",
	} {
		request, _ := http.NewRequest(http.MethodGet, target, nil)
		if response, err := client.Do(request); !errors.Is(err, ErrEnginePeer) {
			if response != nil {
				response.Body.Close()
			}
			t.Fatalf("%s: %v, wanted ErrEnginePeer", target, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a confined request reached the engine: %v", err)
	}
	if _, err := NewUnixClient("", sameProcess(engine)); !errors.Is(err, ErrEnginePeer) {
		t.Fatalf("no path: %v", err)
	}
	if _, err := NewUnixClient(path, nil); !errors.Is(err, ErrEnginePeer) {
		t.Fatalf("no verifier: %v", err)
	}
}
