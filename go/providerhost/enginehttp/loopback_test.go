package enginehttp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
)

func TestPendingProofKeepsConnectionTimeAndStopsAtTerminalError(t *testing.T) {
	for _, terminal := range []error{identity.ErrPeerGone, identity.ErrPeerMoved, identity.ErrNoBinding, net.ErrClosed} {
		t.Run(terminal.Error(), func(t *testing.T) {
			calls := 0
			connected := time.Now().Add(-time.Second)
			_, err := waitLoopbackProof(context.Background(), nil, connected, func(_ net.Conn, opts *identity.Options) (*identity.Binding, error) {
				calls++
				if !opts.ConnectedAt.Equal(connected) || !opts.SkipCodeSignature {
					t.Fatal("connection proof options changed while waiting")
				}
				if calls < 3 {
					return nil, identity.ErrPeerPending
				}
				return nil, terminal
			})
			if !errors.Is(err, terminal) || calls != 3 {
				t.Fatalf("terminal proof: %v after %d calls", err, calls)
			}
		})
	}
}

func TestPendingProofHonorsCancellationWithoutAnotherLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := waitLoopbackProof(ctx, nil, time.Now(), func(net.Conn, *identity.Options) (*identity.Binding, error) {
		calls++
		cancel()
		return nil, identity.ErrPeerPending
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancel: %v after %d calls", err, calls)
	}
}
