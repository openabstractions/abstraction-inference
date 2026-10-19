package enginehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

// A UnixEndpoint is one engine's socket path and the test its server process
// must pass. Verify receives the process abstraction-identity bound for the
// server end of each connection (ID-T8) and must prove it is the engine the
// provider started: its process id and creation time, or its membership in
// the provider's contained process tree.
type UnixEndpoint struct {
	Path   string
	Verify func(identity.Process) (bool, error)
}

// NewUnixTransport returns a round tripper that reaches each request's
// engine on the socket path resolve names for the request's host, binds the
// socket's server process before the request's first byte, and rechecks the
// binding on every read and write (inference CONTRACT.md INF-EL2). A host
// resolve does not know, a URL other than plain http, and a server that
// fails Verify are refused with ErrEnginePeer; the request then never
// leaves this process. It uses no proxy and keeps no idle connection, so
// each request is bound afresh.
func NewUnixTransport(resolve func(host string) (UnixEndpoint, bool)) http.RoundTripper {
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrEnginePeer, err)
			}
			endpoint, ok := resolve(host)
			if !ok || endpoint.Path == "" || endpoint.Verify == nil {
				return nil, fmt.Errorf("%w: no engine socket for %s", ErrEnginePeer, host)
			}
			return dialUnix(ctx, endpoint)
		},
	}
	return &unixHosts{resolve: resolve, next: transport}
}

// NewUnixClient returns an HTTP client confined to one engine socket, for
// requests to base "http://engine". The server's process must pass verify
// before each request.
func NewUnixClient(path string, verify func(identity.Process) (bool, error)) (*http.Client, error) {
	if path == "" || verify == nil {
		return nil, fmt.Errorf("%w: missing socket path or process verifier", ErrEnginePeer)
	}
	endpoint := UnixEndpoint{Path: path, Verify: verify}
	return &http.Client{
		Transport: NewUnixTransport(func(host string) (UnixEndpoint, bool) {
			return endpoint, host == UnixHost
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// UnixHost is the host of NewUnixClient's base URL, "http://engine".
const UnixHost = "engine"

func dialUnix(ctx context.Context, endpoint UnixEndpoint) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", endpoint.Path)
	if err != nil {
		return nil, dialFailure(err)
	}
	// The connection exists from here; a server created later is not the
	// one that accepted it.
	connected := time.Now()
	binding, err := identity.BindUnixServer(conn, &identity.Options{ConnectedAt: connected, SkipCodeSignature: true})
	if err != nil {
		//unchecked: the bind failure is the error returned
		conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrEnginePeer, err)
	}
	guarded := &boundConn{Conn: conn, binding: binding, verify: endpoint.Verify}
	if err := guarded.check(); err != nil {
		//unchecked: the check failure is the error returned
		guarded.Close()
		return nil, err
	}
	return guarded, nil
}

type unixHosts struct {
	resolve func(string) (UnixEndpoint, bool)
	next    http.RoundTripper
}

func (u *unixHosts) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "http" || req.URL.User != nil || req.URL.Port() != "" {
		return nil, ErrEnginePeer
	}
	if _, ok := u.resolve(req.URL.Hostname()); !ok {
		return nil, fmt.Errorf("%w: no engine socket for %s", ErrEnginePeer, req.URL.Hostname())
	}
	return u.next.RoundTrip(req)
}
