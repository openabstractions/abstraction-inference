//go:build linux

package enginehttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

type acceptGate struct {
	net.Listener
	permit, closed chan struct{}
	accepted       chan net.Conn
	closeOnce      sync.Once
	verified       atomic.Bool
	earlyBytes     atomic.Int64
	requests       atomic.Int64
	readErrors     chan error
}

func (g *acceptGate) Accept() (net.Conn, error) {
	select {
	case <-g.closed:
		return nil, net.ErrClosed
	case <-g.permit:
	}
	c, err := g.Listener.Accept()
	if err != nil {
		return nil, err
	}
	g.accepted <- c
	return &observedRequestConn{Conn: c, gate: g}, nil
}

func (g *acceptGate) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.Listener.Close()
}

type observedRequestConn struct {
	net.Conn
	gate *acceptGate
	end  sync.Once
}

func (c *observedRequestConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.gate.verified.Load() {
		c.gate.earlyBytes.Add(int64(n))
	}
	if err != nil {
		c.end.Do(func() { c.gate.readErrors <- err })
	}
	return n, err
}

func gatedHTTPServer(t *testing.T) (string, identity.Process, *acceptGate) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := &acceptGate{Listener: l, permit: make(chan struct{}, 8), closed: make(chan struct{}), accepted: make(chan net.Conn, 8), readErrors: make(chan error, 8)}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Listener.Close()
	server.Listener = g
	server.Start()
	t.Cleanup(func() { g.Close(); server.Close() })
	g.permit <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := InspectListener(ctx, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	<-g.accepted // The initial inspection is a separate connection.
	select {
	case <-g.readErrors:
	case <-ctx.Done():
		t.Fatal("initial inspection left its connection open")
	}
	process, proof := peer.Process.Get()
	if proof < identity.ProofBound || process.StartTime.IsZero() {
		t.Fatalf("initial listener proof: %+v at %v", process, proof)
	}
	return server.URL, process, g
}

func pendingClient(t *testing.T, base string, expected identity.Process, gate *acceptGate) (*http.Client, <-chan net.Conn) {
	t.Helper()
	pending := make(chan net.Conn, 8)
	var mu sync.Mutex
	first := map[net.Conn]time.Time{}
	observed := map[net.Conn]bool{}
	client, err := newLoopbackClient(base, func(process identity.Process) (bool, error) {
		valid := process.PID == expected.PID && process.StartTime.Equal(expected.StartTime) && !process.Recycled
		gate.verified.Store(valid)
		return valid, nil
	}, func(conn net.Conn, opts *identity.Options) (*identity.Binding, error) {
		mu.Lock()
		if first[conn].IsZero() {
			first[conn] = opts.ConnectedAt
		} else if !first[conn].Equal(opts.ConnectedAt) {
			t.Error("ConnectedAt reset during the actual request wait")
		}
		mu.Unlock()
		binding, err := identity.BindLoopbackServer(conn, opts)
		if errors.Is(err, identity.ErrPeerPending) {
			mu.Lock()
			if !observed[conn] {
				observed[conn] = true
				pending <- conn
			}
			mu.Unlock()
		}
		return binding, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, pending
}

func TestClientConcurrentRequestsKeepIndependentBudgets(t *testing.T) {
	base, expected, gate := gatedHTTPServer(t)
	client, pending := pendingClient(t, base, expected, gate)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()
	request := func(ctx context.Context) <-chan error {
		result := make(chan error, 1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
			response, err := client.Do(req)
			if response != nil {
				response.Body.Close()
			}
			result <- err
		}()
		return result
	}
	resultA := request(ctxA)
	select {
	case <-pending:
	case <-ctxB.Done():
		t.Fatal("first request did not enter pending proof")
	}
	resultB := request(ctxB)
	select {
	case <-pending:
	case <-ctxB.Done():
		t.Fatal("second request did not enter pending proof")
	}
	cancelA()
	if err := <-resultA; !errors.Is(err, context.Canceled) {
		t.Fatalf("first cancellation: %v", err)
	}
	gate.permit <- struct{}{}
	gate.permit <- struct{}{}
	if err := <-resultB; err != nil {
		t.Fatalf("one request canceled another: %v", err)
	}
	if gate.requests.Load() != 1 || gate.earlyBytes.Load() != 0 {
		t.Fatalf("requests=%d earlyBytes=%d", gate.requests.Load(), gate.earlyBytes.Load())
	}
}

func TestInspectListenerPendingDeadlineClosesWithoutHTTP(t *testing.T) {
	base, _, gate := gatedHTTPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := InspectListener(ctx, base); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inspection lost caller budget: %v", err)
	}
	gate.permit <- struct{}{}
	select {
	case <-gate.accepted:
	case <-time.After(time.Second):
		t.Fatal("inspection connection was not in the held Accept queue")
	}
	select {
	case err := <-gate.readErrors:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("inspection connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("inspection left a socket open after deadline")
	}
	if gate.requests.Load() != 0 || gate.earlyBytes.Load() != 0 {
		t.Fatal("inspection sent HTTP bytes")
	}
}

func TestClientWaitsForItsActualConnectionBeforeHTTP(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted", false: "wrong-generation"}[allow], func(t *testing.T) {
			base, expected, gate := gatedHTTPServer(t)
			if !allow {
				expected.StartTime = expected.StartTime.Add(time.Second)
			}
			client, pending := pendingClient(t, base, expected, gate)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
			result := make(chan error, 1)
			go func() {
				response, err := client.Do(request)
				if response != nil {
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
				result <- err
			}()
			var dialed net.Conn
			select {
			case dialed = <-pending:
			case <-ctx.Done():
				t.Fatal("actual request never reached pending proof")
			}
			gate.permit <- struct{}{}
			accepted := <-gate.accepted
			if accepted.RemoteAddr().String() != dialed.LocalAddr().String() {
				t.Fatal("proof wait opened a different connection")
			}
			err := <-result
			if allow && err != nil || !allow && !errors.Is(err, ErrEnginePeer) {
				t.Fatalf("actual request result: %v", err)
			}
			if gate.earlyBytes.Load() != 0 || gate.requests.Load() != map[bool]int64{true: 1, false: 0}[allow] {
				t.Fatalf("HTTP before verified proof: bytes=%d requests=%d", gate.earlyBytes.Load(), gate.requests.Load())
			}
		})
	}
}

func TestClientPendingProofUsesOriginalRequestCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			base, expected, gate := gatedHTTPServer(t)
			client, pending := pendingClient(t, base, expected, gate)
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			}
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
			result := make(chan error, 1)
			go func() { _, err := client.Do(request); result <- err }()
			var conn net.Conn
			select {
			case conn = <-pending:
			case err := <-result:
				t.Fatalf("request ended before pending: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("request did not enter proof wait")
			}
			closed := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); closed <- err }()
			if !deadline {
				cancel()
			}
			if err := <-result; !errors.Is(err, ctx.Err()) {
				t.Fatalf("request lost caller budget: %v, context %v", err, ctx.Err())
			}
			select {
			case err := <-closed:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("pending socket not closed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("detached transport dial left a proof waiter/socket alive")
			}
			if gate.requests.Load() != 0 || gate.earlyBytes.Load() != 0 {
				t.Fatal("canceled pending request sent HTTP")
			}
		})
	}
}
