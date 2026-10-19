//go:build windows || linux

package enginehttp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

func TestEngineHTTPHelperProcess(t *testing.T) {
	marker := os.Getenv("OA_ENGINEHTTP_TEST_MARKER")
	if marker == "" {
		return
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(l.Addr().String())
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = os.WriteFile(marker, []byte("request"), 0600)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})}
	_ = server.Serve(l)
}

func childServer(t *testing.T) (string, identity.Process, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "request-seen")
	child := exec.Command(os.Args[0], "-test.run=^TestEngineHTTPHelperProcess$")
	child.Env = append(os.Environ(), "OA_ENGINEHTTP_TEST_MARKER="+marker)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	address, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	address = strings.TrimSpace(address)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := InspectListener(ctx, "http://"+address)
	if err != nil {
		t.Fatal(err)
	}
	process, proof := peer.Process.Get()
	if proof < identity.ProofBound || process.PID != child.Process.Pid || process.StartTime.IsZero() {
		t.Fatalf("unexpected child generation: %+v", process)
	}
	return "http://" + address, process, marker
}

func TestClientBindsExpectedServerBeforeHTTP(t *testing.T) {
	base, expected, marker := childServer(t)
	client, err := NewClient(base, expected)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/api/v1/health", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || string(data) != `{"status":"ok"}` {
		t.Fatalf("bound body read: %q, %v", data, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected child saw no request: %v", err)
	}
}

func TestClientRefusesWrongGenerationBeforeHTTP(t *testing.T) {
	base, expected, marker := childServer(t)
	expected.StartTime = expected.StartTime.Add(time.Second)
	client, err := NewClient(base, expected)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/health", nil)
	if _, err := client.Do(request); !errors.Is(err, ErrEnginePeer) {
		t.Fatalf("wrong generation accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request reached unapproved listener: %v", err)
	}
}

func TestClientConfinesURLs(t *testing.T) {
	base, expected, marker := childServer(t)
	client, err := NewClient(base, expected)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/api/v1/health", nil)
	if _, err := client.Do(request); !errors.Is(err, ErrEnginePeer) {
		t.Fatalf("foreign address accepted: %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected request: %v", err)
	}
}

func TestClientVerifierRefusesUncontainedListener(t *testing.T) {
	base, expected, marker := childServer(t)
	var called atomic.Bool
	client, err := NewClientWithVerifier(base, func(process identity.Process) (bool, error) {
		called.Store(true)
		if process.PID != expected.PID || !process.StartTime.Equal(expected.StartTime) {
			return false, fmt.Errorf("unexpected process: %+v", process)
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, base+"/api/v1/health", nil)
	if _, err := client.Do(request); !errors.Is(err, ErrEnginePeer) || !called.Load() {
		t.Fatalf("uncontained listener accepted: %v, verifier called: %v", err, called.Load())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request reached uncontained listener: %v", err)
	}
}
