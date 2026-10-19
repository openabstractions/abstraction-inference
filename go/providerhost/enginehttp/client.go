// Package enginehttp connects a native provider to its own HTTP engine, on
// the engine's Unix socket (NewUnixTransport, NewUnixClient) or, for an
// engine that serves TCP only, on loopback (NewClient). It proves the
// server end belongs to the expected child before sending a request and
// rechecks that binding while reading the reply (inference CONTRACT.md
// INF-EL2).
package enginehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	identity "github.com/openabstractions/abstraction-identity"
)

var ErrEnginePeer = errors.New("enginehttp: unexpected engine peer")

// NewClient returns an HTTP client confined to base and expected process.
// expected.StartTime distinguishes the selected process from a reused PID.
// Selection may be owned by this provider or explicitly attached by an operator;
// this client does not authorize stopping a process.
// Each request gets a fresh bound connection. macOS refuses with the existing
// loopback identity error until a stronger local transport is available.
func NewClient(base string, expected identity.Process) (*http.Client, error) {
	if expected.PID <= 0 || expected.StartTime.IsZero() || expected.Recycled {
		return nil, fmt.Errorf("%w: child generation", ErrEnginePeer)
	}
	return NewClientWithVerifier(base, func(process identity.Process) (bool, error) {
		return process.PID == expected.PID && process.StartTime.Equal(expected.StartTime) && !process.Recycled, nil
	})
}

// NewClientWithVerifier also supports an engine listener started by a child of
// the provider-owned process. verify must prove both process generation and
// membership in the provider's contained process group before each request.
func NewClientWithVerifier(base string, verify func(identity.Process) (bool, error)) (*http.Client, error) {
	return newLoopbackClient(base, verify, identity.BindLoopbackServer)
}

func newLoopbackClient(base string, verify func(identity.Process) (bool, error), bind func(net.Conn, *identity.Options) (*identity.Binding, error)) (*http.Client, error) {
	address, err := loopbackAddress(base)
	if err != nil {
		return nil, err
	}
	if verify == nil {
		return nil, fmt.Errorf("%w: missing process verifier", ErrEnginePeer)
	}
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			if addr != address {
				return nil, ErrEnginePeer
			}
			// net/http detaches cancellation from its dial context. Values survive
			// WithoutCancel, so fixedHost supplies the actual request budget.
			requestCtx, _ := ctx.Value(requestContextKey{}).(context.Context)
			if requestCtx == nil {
				requestCtx = ctx
			}
			call, cancel := context.WithCancel(requestCtx)
			stop := context.AfterFunc(ctx, cancel)
			defer stop()
			defer cancel()
			conn, binding, err := dialLoopback(call, address, bind)
			if err != nil {
				return nil, err
			}
			guarded := &boundConn{Conn: conn, binding: binding, verify: verify}
			if err := guarded.check(); err != nil {
				guarded.Close()
				return nil, err
			}
			if err := call.Err(); err != nil {
				guarded.Close()
				return nil, err
			}
			return guarded, nil
		},
	}
	return &http.Client{
		Transport:     &fixedHost{address: address, next: transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

type fixedHost struct {
	address string
	next    http.RoundTripper
}

type requestContextKey struct{}

func (f *fixedHost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != f.address || req.URL.User != nil {
		return nil, ErrEnginePeer
	}
	ctx := context.WithValue(req.Context(), requestContextKey{}, req.Context())
	return f.next.RoundTrip(req.Clone(ctx))
}

type boundConn struct {
	net.Conn
	binding *identity.Binding
	verify  func(identity.Process) (bool, error)
	mu      sync.Mutex
}

func (c *boundConn) check() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	peer, err := c.binding.Peer()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEnginePeer, err)
	}
	process, proof := peer.Process.Get()
	if proof < identity.ProofBound || process.PID <= 0 || process.StartTime.IsZero() || process.Recycled {
		return ErrEnginePeer
	}
	alive, err := c.binding.Alive()
	if err != nil || !alive {
		return ErrEnginePeer
	}
	allowed, err := c.verify(process)
	if err != nil {
		return fmt.Errorf("%w: process membership: %w", ErrEnginePeer, err)
	}
	if !allowed {
		return ErrEnginePeer
	}
	return nil
}

func (c *boundConn) Write(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *boundConn) Read(p []byte) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if checkErr := c.check(); checkErr != nil {
		return 0, checkErr
	}
	return n, err
}

func (c *boundConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.binding.Close()
	return c.Conn.Close()
}
