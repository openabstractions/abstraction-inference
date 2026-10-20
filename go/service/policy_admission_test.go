package service

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestHostPolicyMustExistBeforeListening(t *testing.T) {
	apply := func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
		t.Fatal("missing policy reached credentials")
		return nil, ""
	}
	if p, err := inference.New(inference.Config{Router: router.New(), Apply: apply}); err == nil || p != nil {
		t.Fatal("provider accepted an absent decision function")
	}
	endpoint := endpointFor(t)
	if h, err := Listen(endpoint, nil); err == nil || h != nil {
		t.Fatal("host accepted an absent provider")
	}
	if c, err := listen.Dial(endpoint); err == nil {
		c.Close()
		t.Fatal("refused host left a listener")
	}
}

func TestHostRemoteReceivingPolicyMustAdmit(t *testing.T) {
	spki := []byte("inert receiving-policy fixture")
	peer := remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}
	for _, config := range []RemoteConfig{
		{},
		{Account: "receiving-owner"},
		{Account: "receiving-owner", Admit: func(remote.Peer) error { return errors.New("denied") }},
	} {
		config.Domain = func(context.Context, remote.Peer) (string, error) {
			t.Error("unadmitted peer reached receiving composition")
			return "", nil
		}
		if services, err := RemoteServices(context.Background(), peer, nil, nil, config); err == nil || len(services) != 0 {
			t.Fatalf("unadmitted remote services=%v error=%v", services, err)
		}
	}
}

// The declaration supplies an inert model inventory. Its endpoint is never
// served: policy must refuse before credentials, readiness or downstream IPC.
func TestHostPolicyRefusesNativeAndRemoteBeforeWork(t *testing.T) {
	for _, route := range []string{"native", "remote"} {
		for _, tc := range []struct {
			name, word, reason string
			err                error
			outcome            wire.StartOutcome
		}{
			{"denied", "denied", "rights:denied", nil, wire.StartOutcomeNotPermitted},
			{"not granted", "not_granted", "rights:not_granted", nil, wire.StartOutcomeNotPermitted},
			{"unavailable", "unavailable", "rights:unavailable", nil, wire.StartOutcomeUnavailable},
			{"empty", "", "rights:unavailable", nil, wire.StartOutcomeUnavailable},
			{"unknown", "future_decision", "rights:unavailable", nil, wire.StartOutcomeUnavailable},
			{"failed", "permitted", "rights:unavailable", errors.New("policy unavailable"), wire.StartOutcomeUnavailable},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				if route == "native" {
					if err := identity.CanEver(listen.Program); err != nil {
						t.Skip(err)
					}
				}
				model := "fixture-model"
				host, err := router.NewNative("fixture", endpointFor(t)+"-unserved", listen.ServerExpectation{}, []string{model}, []string{router.ProfileChat})
				if err != nil {
					t.Fatal(err)
				}
				var decisions atomic.Int32
				p, err := inference.New(inference.Config{Router: router.New(host),
					Decide: func(ctx context.Context, caller inference.Subject, action, resource string) (string, error) {
						decisions.Add(1)
						if caller.Account == "" || caller.Program == "" || action != inference.ActionComplete || resource != "host:fixture" {
							t.Error("policy lost the receiving subject or action")
						}
						if (inference.PeerFromContext(ctx) != nil) != (route == "native") {
							t.Error("policy received the wrong peer evidence")
						}
						return tc.word, tc.err
					},
					Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
						t.Error("refused policy reached credentials")
						return nil, "not_permitted"
					},
					Admit: func(context.Context, *router.Host, string) (string, bool) {
						t.Error("refused policy reached provider readiness")
						return "unexpected", false
					}})
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
				var transport wire.ChatTransport
				if route == "native" {
					endpoint := endpointFor(t)
					h, err := Listen(endpoint, p)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					done := make(chan error, 1)
					go func() { done <- h.Serve(ctx) }()
					defer func() { cancel(); <-done }()
					transport = listen.FrameClient{Endpoint: endpoint}
				} else {
					spki := []byte("inert receiving-policy fixture")
					peer := remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}
					transport = remoteRouterFrames{handler: RemoteHandler(p, nil, RemoteConfig{Account: "receiving-owner", Admit: func(remote.Peer) error { return nil }}), peer: peer}
				}
				request := wire.Request{Model: model, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}, Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "inert"}}}}}
				result, err := wire.NewChatClient(transport).Start(request)
				if err != nil || result.Outcome != tc.outcome || result.Reason != tc.reason || result.Operation != "" || decisions.Load() != 1 {
					t.Fatalf("admission=%+v error=%v decisions=%d", result, err, decisions.Load())
				}
			})
		}
	}
}
