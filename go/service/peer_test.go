package service

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	fwire "github.com/openabstractions/abstraction-facade/go-core/go/abstraction/facade"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	"github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
	routerservice "github.com/openabstractions/abstraction-router/go/service"
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
	handler := RemoteHandler(p, routes, RemoteConfig{Account: "remote-namespace", Admit: func(remote.Peer) error { return nil }, Domain: func(context.Context, remote.Peer) (string, error) { return "peer.example", nil }})
	spki := []byte("fixture verified remote key")
	reply, err := handler(inference.WithPeer(context.Background(), leaked), remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}, frame.frame)
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

type remoteRouterFrames struct {
	handler remote.Handler
	peer    remote.Peer
}

func (f remoteRouterFrames) ExchangeFrame(frame []byte) ([]byte, error) {
	return f.handler(context.Background(), f.peer, frame)
}

func remoteRouterFixture(t *testing.T, policy routerservice.RemotePolicy) (*routerwire.RouterClient, *router.Router, *int) {
	t.Helper()
	spki := []byte("remote router fixture")
	peer := remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}
	admitted := new(int)
	r := router.New()
	h := RemoteHandler(nil, r, RemoteConfig{Account: "receiving-owner", Admit: func(remote.Peer) error { *admitted++; return nil }, Domain: func(context.Context, remote.Peer) (string, error) { return "display-domain", nil }, RouterPolicy: policy})
	return routerwire.NewRouterClient(remoteRouterFrames{handler: h, peer: peer}), r, admitted
}

func TestRemoteRouterViewsRefusedWithoutPolicy(t *testing.T) {
	c, r, mapped := remoteRouterFixture(t, nil)
	_, err := c.Models(true)
	var service *routerwire.ServiceError
	if !errors.As(err, &service) || service.Code != routerwire.ServiceErrorCodeUnknownService {
		t.Fatalf("unserved router: %v", err)
	}
	if *mapped != 1 || len(r.Asked()) != 0 {
		t.Fatal("mapping repeated or router dispatched")
	}
}

func TestRemoteRouterViewsDecidedPerMethod(t *testing.T) {
	var actions []string
	policy := func(_ context.Context, caller routerservice.RemoteCaller, action, resource string) error {
		if caller.Peer.Program() == "" || caller.Domain != "display-domain" {
			t.Fatal("remote caller lost")
		}
		actions = append(actions, action+" "+resource)
		if action == routerservice.ActionInventory {
			return nil
		}
		return errors.New("no route rule")
	}
	c, r, mapped := remoteRouterFixture(t, policy)
	if _, err := c.Models(false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Hosts(false); err != nil {
		t.Fatal(err)
	}
	// Offer reads under inventory.read (ROUTE-A2); only Pick needs route.
	o, err := c.Offer(routerwire.OfferRequest{Placement: routerwire.PlacementAny})
	if err != nil || o.Outcome != routerwire.OfferOutcomeOffered {
		t.Fatalf("Offer %+v %v", o, err)
	}
	if _, err := c.Pick(routerwire.PickRequest{Model: "missing"}); err == nil {
		t.Fatal("Pick allowed")
	}
	if len(actions) != 4 || actions[0] != routerservice.ActionInventory+" "+routerservice.ResourceInventory || actions[1] != actions[0] || actions[2] != actions[0] || actions[3] != routerservice.ActionRoute+" "+routerservice.ResourceRoutes {
		t.Fatalf("actions %v", actions)
	}
	if *mapped != 4 || len(r.Asked()) != 0 {
		t.Fatal("mapping repeated or refusal reached route audit")
	}
}

func TestRemoteServicesKeepsSelectedAndLegacySetsSeparate(t *testing.T) {
	spki := []byte("selected remote fixture")
	peer := remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}
	config := RemoteConfig{Account: "receiving-owner", Admit: func(remote.Peer) error { return nil }, RouterPolicy: func(context.Context, routerservice.RemoteCaller, string, string) error { return nil }}
	routes := router.New()
	selected, err := RemoteServices(context.Background(), peer, nil, routes, config)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"abstraction.inference/chat@1", "abstraction.inference/embed@1", RouterContract}
	if len(selected) != len(want) {
		t.Fatalf("selected count %d", len(selected))
	}
	for i, service := range selected {
		if service.ServiceContract() != want[i] {
			t.Fatalf("selected[%d]=%s", i, service.ServiceContract())
		}
	}
	legacy := fwire.NewEndpointClient(remoteRouterFrames{handler: RemoteHandler(nil, routes, config), peer: peer})
	description, err := legacy.Describe()
	if err != nil {
		t.Fatal(err)
	}
	legacyWant := []string{"abstraction.inference/chat@1", "abstraction.inference/embed@1", "abstraction.inference/transcription@1", "abstraction.inference/speech@1", "abstraction.inference/image@1", "abstraction.inference/live@1", RouterContract}
	if len(description.Services) != len(legacyWant) {
		t.Fatalf("legacy count %d", len(description.Services))
	}
	for i, service := range description.Services {
		if service.Contract != legacyWant[i] {
			t.Fatalf("legacy[%d]=%s", i, service.Contract)
		}
	}
	config.Account = ""
	if _, err := RemoteServices(context.Background(), peer, nil, routes, config); err == nil {
		t.Fatal("missing account accepted")
	}
}

func TestChat3PrivateRemoteReceiverUsesMappedCaller(t *testing.T) {
	p, routes, decided := peerProvider(t)
	if err := p.BindChatV3LogicalOwner("receiving-job-owner"); err != nil {
		t.Fatal(err)
	}
	spki := []byte("chat3 receiving fixture")
	peer := remote.Peer{Certificate: &x509.Certificate{RawSubjectPublicKeyInfo: spki}, Key: sha256.Sum256(spki)}
	config := RemoteConfig{Account: "receiving-owner", Admit: func(remote.Peer) error { return nil }}
	base, _, err := remoteServices(inference.WithPeer(context.Background(), &identity.Peer{}), peer, p, routes, config)
	if err != nil {
		t.Fatal(err)
	}
	// Compose the existing private receiver explicitly; production served sets
	// remain unchanged until HTTP and generated qualification are complete.
	chat := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: &remoteChatV3{base}})
	history, err := chat.GetHistoryWindow()
	if err != nil || history.Outcome != wire.ChatHistoryOutcomeAvailable || history.Window == nil {
		t.Fatalf("history %+v %v", history, err)
	}
	request := wire.RequestV3{Identity: acceptance.RequestIdentity{Key: "request", HistoryEpoch: history.Window.HistoryEpoch},
		Target: wire.ChatTarget{Model: "anthropic/claude-sonnet-5"}, Content: wire.ChatContent{
			Messages:   []wire.ChatMessage{{Role: wire.RoleUser, Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "hello"}}}},
			Guarantees: []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}, Credential: "openrouter"}}
	admission, err := chat.Start(request)
	if err != nil || admission.Outcome != wire.ChatStartOutcomeNotPermitted || admission.Request != "" {
		t.Fatalf("admission %+v %v", admission, err)
	}
	calls := decided()
	if len(calls) != 1 || calls[0].subject != base.subject || calls[0].subject.Account != config.Account || calls[0].peer != nil {
		t.Fatalf("chat3 receiver attribution %+v", calls)
	}
}
