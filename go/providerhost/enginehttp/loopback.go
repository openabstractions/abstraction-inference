package enginehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

func loopbackAddress(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return "", fmt.Errorf("%w: private loopback base", ErrEnginePeer)
	}
	return u.Host, nil
}

// waitLoopbackProof retries only an initial zero-holder hint, before any
// identity has been captured. The original connection time remains fixed,
// and a captured binding is never retried. Neither lookup nor waiting sends
// anything on conn.
func waitLoopbackProof(ctx context.Context, conn net.Conn, connectedAt time.Time, bind func(net.Conn, *identity.Options) (*identity.Binding, error)) (*identity.Binding, error) {
	options := &identity.Options{ConnectedAt: connectedAt, SkipCodeSignature: true}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		binding, err := bind(conn, options)
		if err == nil {
			if err := ctx.Err(); err != nil {
				binding.Close()
				return nil, err
			}
			return binding, nil
		}
		if !errors.Is(err, identity.ErrPeerPending) {
			return nil, err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func dialLoopback(ctx context.Context, address string, bind func(net.Conn, *identity.Options) (*identity.Binding, error)) (net.Conn, *identity.Binding, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, nil, dialFailure(err)
	}
	connectedAt := time.Now()
	binding, err := waitLoopbackProof(ctx, conn, connectedAt, bind)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w: %w", ErrEnginePeer, err)
	}
	return conn, binding, nil
}

// InspectListener observes the bound process at an IPv4 loopback HTTP base
// without sending HTTP bytes. Dial and an initial pending-accept proof share
// ctx's budget. The connection and binding are closed before this returns;
// an HTTP client must bind each later connection independently.
func InspectListener(ctx context.Context, base string) (*identity.Peer, error) {
	address, err := loopbackAddress(base)
	if err != nil {
		return nil, err
	}
	conn, binding, err := dialLoopback(ctx, address, identity.BindLoopbackServer)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer binding.Close()
	return binding.Peer()
}
