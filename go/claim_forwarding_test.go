package inference_test

import (
	"context"
	"errors"
	"maps"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	identityremote "github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

// claimUpstream is a remote runtime that records the extensions of every Start
// it receives and refuses the work, so no operation runs beyond admission.
type claimUpstream struct {
	seen chan map[string]string
	lost bool
	gate <-chan struct{}
}

func (u claimUpstream) record(values map[string]string) (wire.Admission, error) {
	u.seen <- maps.Clone(values)
	if u.gate != nil {
		<-u.gate
	}
	if u.lost {
		return wire.Admission{}, errLostReply
	}
	return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "fixture"}, nil
}

type claimChat struct{ claimUpstream }

func (c claimChat) Start(r wire.Request) (wire.Admission, error) { return c.record(r.Extensions) }
func (claimChat) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (claimChat) Cancel(string) (wire.Cancellation, error) { return wire.Cancellation{}, nil }

type claimEmbed struct{ claimUpstream }

func (e claimEmbed) Embed(r wire.EmbedRequest) (wire.Embeddings, error) {
	if _, err := e.record(r.Extensions); err != nil {
		return wire.Embeddings{}, err
	}
	return wire.Embeddings{Outcome: wire.EmbedOutcomeUnavailable, Reason: "fixture"}, nil
}

type claimSpeech struct{ claimUpstream }

type claimImage struct{ claimUpstream }

func (c claimImage) Start(r wire.ImageRequest) (wire.Admission, error) { return c.record(r.Extensions) }
func (claimImage) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (claimImage) Cancel(string) (wire.Cancellation, error) { return wire.Cancellation{}, nil }

func (c claimSpeech) Start(r wire.SpeechRequest) (wire.Admission, error) {
	return c.record(r.Extensions)
}
func (claimSpeech) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (claimSpeech) Cancel(string) (wire.Cancellation, error) { return wire.Cancellation{}, nil }

type claimTranscription struct{ claimUpstream }

func (c claimTranscription) Start(r wire.TranscriptionRequest) (wire.Admission, error) {
	return c.record(r.Extensions)
}
func (claimTranscription) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (claimTranscription) Cancel(string) (wire.Cancellation, error) { return wire.Cancellation{}, nil }

type claimLive struct{ claimUpstream }

func (c claimLive) Start(r wire.LiveRequest) (wire.Admission, error) { return c.record(r.Extensions) }
func (claimLive) Append(string, int64, []byte) (wire.LiveInputResult, error) {
	return wire.LiveInputResult{}, nil
}
func (claimLive) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return wire.DeltaPage{}, nil
}
func (claimLive) Commit(string) (wire.LiveInputResult, error) { return wire.LiveInputResult{}, nil }
func (claimLive) Cancel(string) (wire.Cancellation, error)    { return wire.Cancellation{}, nil }

type claimInventory struct{}

func (claimInventory) Modalities(string, int64) (routerwire.ModalityPage, error) {
	return routerwire.ModalityPage{}, errors.New("unexpected profile catalogue call")
}

func (claimInventory) Models(bool) (routerwire.ModelsSnapshot, error) {
	return routerwire.ModelsSnapshot{Models: []routerwire.Family{{Family: "claim-model", Names: []routerwire.Alias{{Name: "claim-model", Servable: true, Hosted: true}}}}}, nil
}
func (claimInventory) Hosts(bool) (routerwire.HostsSnapshot, error) {
	return routerwire.HostsSnapshot{}, nil
}
func (claimInventory) Pick(routerwire.PickRequest) (routerwire.PickResult, error) {
	return routerwire.PickResult{}, nil
}
func (claimInventory) Offer(routerwire.OfferRequest) (routerwire.OfferSnapshot, error) {
	return routerwire.OfferSnapshot{}, nil
}
func (claimInventory) ObserveOffer(routerwire.OfferRequest, string, int64) (routerwire.OfferObservation, error) {
	return routerwire.OfferObservation{}, nil
}

type claimFixture struct {
	provider *inference.Provider
	seen     chan map[string]string
	records  chan inference.Record
}

// newClaimFixture is a runtime whose only host is a remote runtime serving
// every Start-based profile the admission seam separates.
func newClaimFixture(t *testing.T) claimFixture {
	return newClaimFixtureOutcome(t, false, nil, false)
}

func newClaimFixtureOutcome(t *testing.T, lost bool, gate <-chan struct{}, malformed bool) claimFixture {
	t.Helper()
	serverTLS, clientTLS := remoteLiveTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := claimUpstream{seen: make(chan map[string]string, 4), lost: lost, gate: gate}
	handler := func(_ context.Context, _ identityremote.Peer, frame []byte) ([]byte, error) {
		reply, err := wire.ServeEndpoint(frame, "remote-test", "", &routerwire.RouterDispatcher{Handler: claimInventory{}},
			&wire.ChatDispatcher{Handler: claimChat{upstream}}, &wire.EmbedderDispatcher{Handler: claimEmbed{upstream}}, &wire.SpeechDispatcher{Handler: claimSpeech{upstream}},
			&wire.TranscriptionDispatcher{Handler: claimTranscription{upstream}}, &wire.LiveDispatcher{Handler: claimLive{upstream}}, &wire.ImageDispatcher{Handler: claimImage{upstream}})
		if malformed && err != nil {
			return []byte{0}, nil
		}
		return reply, err
	}
	serveCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (identityremote.Server{TLS: serverTLS, Admit: remoteTestPin(t, clientTLS), Handler: handler, Timeout: 5 * time.Second, MaxFrame: 8 << 20}).Serve(serveCtx, listener)
	}()
	t.Cleanup(func() {
		stop()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("remote server: %v", err)
		}
	})
	host, err := router.NewRemote("lab", listener.Addr().String(), clientTLS, "remote-key", remoteTestPin(t, serverTLS))
	if err != nil {
		t.Fatal(err)
	}
	host.Profiles = []string{router.ProfileChat, router.ProfileEmbed, router.ProfileSpeech, router.ProfileTranscription, router.ProfileLive, router.ProfileImage}
	routes := router.New(host)
	routes.Survey()
	records := make(chan inference.Record, 8)
	provider, err := inference.New(inference.Config{Router: routes, SurveyAge: time.Hour, Record: func(r inference.Record) { records <- r },
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveSelectionV2: func(_ context.Context, _ inference.Subject, name string) (wire.SelectionPolicyV2, bool, error) {
			return wire.SelectionPolicyV2{Name: name, Candidates: []wire.SelectionCandidateV2{{Model: "claim-model", Placement: wire.CandidatePlacementHosted, Credential: "remote-key"}}}, true, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return claimFixture{provider: provider, seen: upstream.seen, records: records}
}

var claimCaller = inference.Subject{Account: "account", Program: "/apps/caller"}

// forgedExtensions is an application's request: one vendor control, a forged
// caller claim and another key in the reserved namespace.
func forgedExtensions() map[string]string {
	return map[string]string{"vendor/k": "v", inference.ClaimExtension: "forged-app", inference.ReservedExtensionPrefix + "other": "x"}
}

var claimHosted = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}

// An application's reserved keys never reach the next runtime. It receives the
// vendor control and the claim this runtime derives from the bound caller, and
// this runtime records no claim for a caller it does not trust.
func TestForgedApplicationClaimNeverLeavesTheRuntime(t *testing.T) {
	chat := func(p *inference.Provider, ctx context.Context) wire.Admission {
		return p.Start(ctx, claimCaller, wire.Request{Model: "claim-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
			Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
	}
	content := func() wire.RequestContent {
		return wire.RequestContent{Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}}, Extensions: forgedExtensions()}
	}
	for _, tc := range []struct {
		name  string
		start func(*inference.Provider, context.Context) wire.Admission
	}{
		{"chat@1", chat},
		{"chat@2 explicit model", func(p *inference.Provider, ctx context.Context) wire.Admission {
			c := content()
			c.Guarantees, c.Credential = claimHosted, "remote-key"
			a := p.StartV2(ctx, claimCaller, wire.RequestV2{Target: wire.Target{Model: "claim-model"}, Content: c})
			return wire.Admission{Outcome: a.Outcome, Reason: a.Reason, Operation: a.Operation}
		}},
		{"chat@2 selection policy", func(p *inference.Provider, ctx context.Context) wire.Admission {
			a := p.StartV2(ctx, claimCaller, wire.RequestV2{Target: wire.Target{SelectionPolicy: "default"}, Content: content()})
			return wire.Admission{Outcome: a.Outcome, Reason: a.Reason, Operation: a.Operation}
		}},
		{"speech", func(p *inference.Provider, ctx context.Context) wire.Admission {
			return p.StartSpeech(ctx, claimCaller, wire.SpeechRequest{Model: "claim-model", Voice: "alloy", Text: "hello", Format: wire.SpeechFormatMp3,
				Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
		}},
		{"transcription", func(p *inference.Provider, ctx context.Context) wire.Admission {
			return p.StartTranscription(ctx, claimCaller, wire.TranscriptionRequest{Model: "claim-model", AudioDigest: "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				MediaType: "audio/wav", Timestamps: wire.TimestampModeSegment, Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
		}},
		{"live", func(p *inference.Provider, ctx context.Context) wire.Admission {
			return p.StartLive(ctx, claimCaller, wire.LiveRequest{Model: "claim-model", Voice: "alloy", Format: wire.LiveFormatPcm1624000,
				Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newClaimFixture(t)
			app := inference.WithRoute(context.Background(), inference.RouteNative, "program")
			if a := tc.start(f.provider, app); a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			select {
			case seen := <-f.seen:
				claim := claimCaller.Program
				if strings.HasPrefix(tc.name, "chat@") {
					claim = filepath.Base(claim)
				}
				want := map[string]string{"vendor/k": "v", inference.ClaimExtension: claim}
				if !maps.Equal(seen, want) {
					t.Fatalf("next runtime received %v, want %v", seen, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("remote runtime received no Start")
			}
			select {
			case r := <-f.records:
				if r.Claim != "" || r.Program != claimCaller.Program {
					t.Fatalf("record %+v", r)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no operation record")
			}
		})
	}
}

// Remote offers carry the local caller's display claim while the receiving
// runtime derives authority from its pinned peer. A forged reserved key cannot
// replace that display claim or leak another reserved value.
func TestRemoteChatAndEmbedOriginClaimCarriesNoPath(t *testing.T) {
	for _, profile := range []string{router.ProfileChat, router.ProfileEmbed} {
		t.Run(profile, func(t *testing.T) {
			f := newClaimFixture(t)
			ctx := inference.WithRoute(context.Background(), inference.RouteNative, "program")
			if profile == router.ProfileChat {
				admission := f.provider.Start(ctx, claimCaller, wire.Request{Model: "claim-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}}, Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
				if admission.Outcome != wire.StartOutcomeAccepted {
					t.Fatalf("chat admission %+v", admission)
				}
			} else {
				result := f.provider.Embed(ctx, claimCaller, wire.EmbedRequest{Model: "claim-model", Inputs: []string{"hello"}, Guarantees: claimHosted, Credential: "remote-key", Extensions: forgedExtensions()})
				if result.Outcome != wire.EmbedOutcomeUnavailable || result.Reason != "remote:fixture" {
					t.Fatalf("embed result %+v", result)
				}
			}
			select {
			case seen := <-f.seen:
				want := map[string]string{"vendor/k": "v", inference.ClaimExtension: filepath.Base(claimCaller.Program)}
				if !maps.Equal(seen, want) || strings.ContainsAny(seen[inference.ClaimExtension], `/\`) {
					t.Fatalf("remote display extensions %v, want %v", seen, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("remote received no request")
			}
		})
	}
}
