package service

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// decidedPeer is one Decide call: the subject decided and the peer its ctx
// carried.
type decidedPeer struct {
	subject inference.Subject
	peer    *identity.Peer
}

// capturedFrame records the request frame a client sends and refuses it.
type capturedFrame struct{ frame []byte }

func (c *capturedFrame) ExchangeFrame(frame []byte) ([]byte, error) {
	c.frame = append([]byte(nil), frame...)
	return nil, errors.New("captured")
}

func peerProvider(t *testing.T) (*inference.Provider, *router.Router, func() []decidedPeer) {
	t.Helper()
	up, _, _ := fakeUpstream(t, false)
	r := router.New(router.NewHosted("openrouter", up.URL+"/api/v1", router.WireOpenAICompatible, "openrouter"))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer " + secret}, nil
	})
	var mu sync.Mutex
	var decided []decidedPeer
	p, err := inference.New(inference.Config{Router: r,
		Decide: func(ctx context.Context, s inference.Subject, _, _ string) (string, error) {
			mu.Lock()
			decided = append(decided, decidedPeer{subject: s, peer: inference.PeerFromContext(ctx)})
			mu.Unlock()
			return "not_granted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		Idle: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, r, func() []decidedPeer {
		mu.Lock()
		defer mu.Unlock()
		return append([]decidedPeer(nil), decided...)
	}
}

// TestNativeRouteDecidesWithTheBoundPeer: the native route passes Decide the
// peer it bound the subject from, the evidence a first-use question takes its
// publisher from (ASK-A6).
func TestNativeRouteDecidesWithTheBoundPeer(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	p, _, decided := peerProvider(t)
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	client := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint}.WithDefaults(35*time.Second, MaxFrameBytes))
	if a, err := client.Start(request()); err != nil || a.Outcome != wire.StartOutcomeNotPermitted {
		t.Fatalf("start %+v %v", a, err)
	}
	calls := decided()
	if len(calls) != 1 {
		t.Fatalf("decisions %+v", calls)
	}
	if calls[0].peer == nil {
		t.Fatal("the native route decided without the peer it bound")
	}
	bound, err := SubjectFromPeer(calls[0].peer)
	if err != nil || bound != calls[0].subject {
		t.Fatalf("the peer binds %+v %v, the decision was for %+v", bound, err, calls[0].subject)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !identity.SameSubjectProgram(bound.Program, identity.CanonicalProgramPath(filepath.Clean(exe))) {
		t.Fatalf("the peer is %s, not this test program", bound.Program)
	}
}

// TestRemoteRouteDecidesWithoutAPeer: a remote caller is a certificate mapped
// to a subject, with no local peer behind it. The remote route passes Decide
// no peer, and drops one its transport context carried.
func TestRemoteRouteDecidesWithoutAPeer(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	p, routes, decided := peerProvider(t)
	// A real peer to leak: this test program bound over the native route.
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	native := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint}.WithDefaults(35*time.Second, MaxFrameBytes))
	if _, err := native.Start(request()); err != nil {
		t.Fatal(err)
	}
	leaked := decided()[0].peer
	if leaked == nil {
		t.Fatal("no native peer to leak")
	}
	var frame capturedFrame
	wire.NewChatClient(&frame).Start(request())
	handler := RemoteHandler(p, routes, func(context.Context, remote.Peer) (inference.Subject, string, error) {
		return inference.Subject{Account: "remote-namespace", Program: decided()[0].subject.Program}, "peer.example", nil
	})
	reply, err := handler(inference.WithPeer(context.Background(), leaked), remote.Peer{Certificate: &x509.Certificate{}}, frame.frame)
	if err != nil {
		t.Fatal(err)
	}
	var answered capturedReply
	answered.reply = reply
	if a, err := wire.NewChatClient(&answered).Start(request()); err != nil || a.Outcome != wire.StartOutcomeNotPermitted {
		t.Fatalf("remote start %+v %v", a, err)
	}
	calls := decided()
	if len(calls) != 2 || calls[1].subject.Account != "remote-namespace" {
		t.Fatalf("decisions %+v", calls)
	}
	if calls[1].peer != nil {
		t.Fatal("the remote route decided with a local peer")
	}
}

// capturedReply answers every exchange with one recorded reply frame.
type capturedReply struct{ reply []byte }

func (c *capturedReply) ExchangeFrame([]byte) ([]byte, error) { return c.reply, nil }
