package inference_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	identityremote "github.com/openabstractions/abstraction-identity/remote"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

// F20 (C-77): a request a named selection policy routed records the route that
// ran and the preferred route, and a fallback only after the preferred route
// was known unavailable. A refusal or an uncertain remote execution stays a
// visible failure and never reaches another candidate.

var selectionCaller = inference.Subject{Account: "account", Program: "/apps/chat"}

type selectionRemoteInventory struct{}

func (selectionRemoteInventory) Modalities(string, int64) (routerwire.ModalityPage, error) {
	return routerwire.ModalityPage{}, errors.New("unexpected profile catalogue call")
}
func (selectionRemoteInventory) Models(bool) (routerwire.ModelsSnapshot, error) {
	return routerwire.ModelsSnapshot{Models: []routerwire.Family{{Family: "remote-model", Names: []routerwire.Alias{{Name: "remote-model", Servable: true, Hosted: true}}}}}, nil
}
func (selectionRemoteInventory) Hosts(bool) (routerwire.HostsSnapshot, error) {
	return routerwire.HostsSnapshot{}, nil
}
func (selectionRemoteInventory) Pick(routerwire.PickRequest) (routerwire.PickResult, error) {
	return routerwire.PickResult{}, nil
}
func (selectionRemoteInventory) Offer(routerwire.OfferRequest) (routerwire.OfferSnapshot, error) {
	return routerwire.OfferSnapshot{}, nil
}
func (selectionRemoteInventory) ObserveOffer(routerwire.OfferRequest, string, int64) (routerwire.OfferObservation, error) {
	return routerwire.OfferObservation{}, nil
}

// selectionRemoteChat is the remote runtime's chat@1. observe decides how the
// accepted operation's observation is answered.
type selectionRemoteChat struct {
	starts  *atomic.Int32
	observe func() (wire.DeltaPage, error)
}

// errLostReply makes the remote runtime receive Start and answer nothing.
var errLostReply = errors.New("reply lost")

func (c selectionRemoteChat) Start(wire.Request) (wire.Admission, error) {
	c.starts.Add(1)
	if c.observe == nil {
		return wire.Admission{}, errLostReply
	}
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "remote-op"}, nil
}
func (c selectionRemoteChat) Observe(string, int64, int64, int64, int64) (wire.DeltaPage, error) {
	return c.observe()
}
func (selectionRemoteChat) Cancel(string) (wire.Cancellation, error) {
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}, nil
}

func completedRemotePage() (wire.DeltaPage, error) {
	part := wire.Part{Kind: wire.PartKindText, Text: "remote"}
	end := wire.Reply{Outcome: wire.ReplyOutcomeCompleted, StopReason: wire.StopReasonEnd, Host: "remote-engine", Model: "remote-model",
		Message: wire.Message{Role: wire.RoleAssistant, Parts: []wire.Part{}}}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Next: 2, AtEnd: true, Deltas: []wire.Delta{
		{Sequence: 0, Kind: wire.DeltaKindPart, Index: 0, Part: &part},
		{Sequence: 1, Kind: wire.DeltaKindEnd, End: &end}}}, nil
}

// selectionFixture is a runtime with a remote runtime "lab" and a local
// OpenAI-compatible server "local", and the policy: remote-model on lab
// preferred, then local-model.
type selectionFixture struct {
	provider     *inference.Provider
	records      chan inference.Record
	remoteStarts *atomic.Int32
	localPosts   *atomic.Int32
	lab          *router.Host
}

// The existing fixture has a real mutually authenticated loopback remoteLink.
// Local application chat3 may read its inventory but never start work there.
func TestApplicationChat3LocalPlacementNeverStartsThePairedWire(t *testing.T) {
	f := newSelectionFixture(t, true, completedRemotePage, nil)
	if f.lab.Wire != router.WireRemote || !f.lab.Hosted {
		t.Fatal("fixture is not the paired remote wire")
	}
	probe := f.start()
	if probe.Outcome != wire.StartOutcomeAccepted || probe.Host != "lab" || f.end(t, probe.Operation).Outcome != wire.ReplyOutcomeCompleted || f.remoteStarts.Load() != 1 {
		t.Fatal("existing paired fixture is not callable", probe, f.remoteStarts.Load())
	}
	if err := f.provider.BindChatV3LogicalOwner("local-application-job-owner"); err != nil {
		t.Fatal(err)
	}
	window := f.provider.GetChatHistoryWindow(selectionCaller)
	if window.Window == nil {
		t.Fatal(window)
	}
	ctx := inference.WithExecutionPlacement(t.Context(), inference.ExecutionLocal)
	request := wire.RequestV3{Identity: acceptance.RequestIdentity{Key: "local-never-paired", HistoryEpoch: window.Window.HistoryEpoch}, Target: wire.ChatTarget{Model: "remote-model"}, Content: wire.ChatContent{Messages: []wire.ChatMessage{{Role: wire.RoleUser, Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "inert"}}}}, Guarantees: []wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly}}}
	admission := f.provider.StartV3(ctx, selectionCaller, request)
	if admission.Outcome != wire.ChatStartOutcomeNoHost {
		t.Fatal(admission)
	}
	if f.remoteStarts.Load() != 1 || f.localPosts.Load() != 0 {
		t.Fatal("local application crossed paired wire", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

// newSelectionFixture's remote runtime is unreachable when reachable is false.
// A reachable remote with a nil observe receives Start and loses its reply.
func newSelectionFixture(t *testing.T, reachable bool, observe func() (wire.DeltaPage, error), decide func(string) string, fallback ...wire.SelectionCandidateV2) selectionFixture {
	t.Helper()
	serverTLS, clientTLS := remoteLiveTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	starts := &atomic.Int32{}
	address := listener.Addr().String()
	if reachable {
		chat := selectionRemoteChat{starts: starts, observe: observe}
		handler := func(_ context.Context, _ identityremote.Peer, frame []byte) ([]byte, error) {
			return wire.ServeEndpoint(frame, "remote-test", "", &routerwire.RouterDispatcher{Handler: selectionRemoteInventory{}}, &wire.ChatDispatcher{Handler: chat})
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
	} else if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	posts := &atomic.Int32{}
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		posts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(local.Close)
	lab, err := router.NewRemote("lab", address, clientTLS, "remote-key", remoteTestPin(t, serverTLS))
	if err != nil {
		t.Fatal(err)
	}
	lab.Profiles = []string{router.ProfileChat}
	routes := router.New(lab, router.OpenAICompatible("local", local.URL, "/v1", []string{"local-model"}))
	records := make(chan inference.Record, 16)
	provider, err := inference.New(inference.Config{Router: routes, SurveyAge: time.Hour, Record: func(r inference.Record) { records <- r },
		Decide: func(_ context.Context, _ inference.Subject, _ string, resource string) (string, error) {
			if decide != nil {
				return decide(resource), nil
			}
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		ResolveSelectionV2: func(_ context.Context, _ inference.Subject, name string) (wire.SelectionPolicyV2, bool, error) {
			second := wire.SelectionCandidateV2{Model: "local-model", Placement: wire.CandidatePlacementLocal}
			if len(fallback) > 0 {
				second = fallback[0]
			}
			return wire.SelectionPolicyV2{Name: name, Candidates: []wire.SelectionCandidateV2{
				{Model: "remote-model", Placement: wire.CandidatePlacementHosted, Credential: "remote-key"},
				second}}, true, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return selectionFixture{provider: provider, records: records, remoteStarts: starts, localPosts: posts, lab: lab}
}

func (f selectionFixture) start() wire.AdmissionV2 {
	return f.provider.StartV2(context.Background(), selectionCaller, wire.RequestV2{Target: wire.Target{SelectionPolicy: "everyday"},
		Content: wire.RequestContent{Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
			Guarantees: []wire.RequestGuarantee{}}})
}

// end observes operation id to its terminal reply.
func (f selectionFixture) end(t *testing.T, id string) wire.Reply {
	t.Helper()
	cursor := int64(0)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		page := f.provider.Observe(context.Background(), selectionCaller, id, cursor, 256, 65536, 1000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindEnd {
				return *d.End
			}
		}
		cursor = page.Next
	}
	t.Fatal("operation did not end")
	return wire.Reply{}
}

func (f selectionFixture) record(t *testing.T) inference.Record {
	t.Helper()
	select {
	case r := <-f.records:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no record")
		return inference.Record{}
	}
}

func TestSelectionRecordsTheKnownUnavailableRemotePreferredRoute(t *testing.T) {
	f := newSelectionFixture(t, false, nil, nil)
	got := f.start()
	if got.Outcome != wire.StartOutcomeAccepted || got.Host != "local" || got.Model != "local-model" || got.PreferredModel != "remote-model" ||
		got.SelectionReason == nil || *got.SelectionReason != wire.SelectionReasonPreferredRouteUnavailable {
		t.Fatalf("fallback admission: %+v", got)
	}
	if reply := f.end(t, got.Operation); reply.Outcome != wire.ReplyOutcomeCompleted || reply.Host != "local" || reply.Model != "local-model" {
		t.Fatalf("fallback reply: %+v", reply)
	}
	r := f.record(t)
	if r.Operation != got.Operation || r.Outcome != "completed" || r.Host != "local" || r.Model != "local-model" || r.Domain != "" ||
		r.Preferred != "remote-model" || r.PreferredHost != "lab" || r.Selection != "preferred_route_unavailable" || r.Placement != wire.ServedPlaceLocal {
		t.Fatalf("fallback record: %+v", r)
	}
	if f.remoteStarts.Load() != 0 || f.localPosts.Load() != 1 {
		t.Fatalf("remote starts %d, local posts %d", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

func TestSelectionRecordsTheRemotePreferredRouteThatAnswered(t *testing.T) {
	f := newSelectionFixture(t, true, completedRemotePage, nil)
	got := f.start()
	if got.Outcome != wire.StartOutcomeAccepted || got.Host != "lab" || got.PreferredModel != "remote-model" ||
		got.SelectionReason == nil || *got.SelectionReason != wire.SelectionReasonPreferred {
		t.Fatalf("preferred admission: %+v", got)
	}
	if reply := f.end(t, got.Operation); reply.Outcome != wire.ReplyOutcomeCompleted || reply.Host != "lab" {
		t.Fatalf("preferred reply: %+v", reply)
	}
	r := f.record(t)
	if r.Outcome != "completed" || r.Host != "lab" || r.Model != "remote-model" || r.Domain != f.lab.Domain ||
		r.Preferred != "remote-model" || r.PreferredHost != "lab" || r.Selection != "preferred" || r.Placement != wire.ServedPlaceRemote {
		t.Fatalf("preferred record: %+v", r)
	}
	if f.remoteStarts.Load() != 1 || f.localPosts.Load() != 0 {
		t.Fatalf("remote starts %d, local posts %d", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

// The remote runtime accepted the work and its observation was then lost:
// whether the remote finished is unknown. The caller and the record read a
// failure on the preferred route, and the local candidate is never tried.
func TestSelectionUncertainRemoteExecutionStaysAVisibleFailure(t *testing.T) {
	f := newSelectionFixture(t, true, func() (wire.DeltaPage, error) {
		return wire.DeltaPage{}, errors.New("observation lost")
	}, nil)
	got := f.start()
	if got.Outcome != wire.StartOutcomeAccepted || got.Host != "lab" || *got.SelectionReason != wire.SelectionReasonPreferred {
		t.Fatalf("admission: %+v", got)
	}
	reply := f.end(t, got.Operation)
	if reply.Outcome != wire.ReplyOutcomeUnavailable || reply.Reason == "" || reply.Host != "lab" {
		t.Fatalf("uncertain reply: %+v", reply)
	}
	r := f.record(t)
	if r.Outcome != "unavailable" || r.Reason != reply.Reason || r.Host != "lab" || r.Domain != f.lab.Domain ||
		r.Preferred != "remote-model" || r.Selection != "preferred" {
		t.Fatalf("uncertain record: %+v", r)
	}
	if f.remoteStarts.Load() != 1 || f.localPosts.Load() != 0 {
		t.Fatalf("remote starts %d, local posts %d", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

// A Start the remote runtime received but never answered may run there. The
// request fails visibly on the preferred route and is never sent elsewhere.
func TestSelectionLostRemoteStartReplyStaysAVisibleFailure(t *testing.T) {
	f := newSelectionFixture(t, true, nil, nil)
	got := f.start()
	if got.Outcome != wire.StartOutcomeAccepted || got.Host != "lab" || *got.SelectionReason != wire.SelectionReasonPreferred {
		t.Fatalf("admission: %+v", got)
	}
	reply := f.end(t, got.Operation)
	if reply.Outcome != wire.ReplyOutcomeUnavailable || reply.Reason != "remote:uncertain" || reply.Host != "lab" {
		t.Fatalf("lost reply: %+v", reply)
	}
	r := f.record(t)
	if r.Outcome != "unavailable" || r.Reason != reply.Reason || r.Host != "lab" || r.Preferred != "remote-model" || r.Selection != "preferred" {
		t.Fatalf("lost reply record: %+v", r)
	}
	if f.remoteStarts.Load() != 1 || f.localPosts.Load() != 0 {
		t.Fatalf("remote starts %d, local posts %d", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

// A rights refusal on the preferred remote route is the request's answer; the
// record names the refused preferred route and the local candidate is never tried.
func TestSelectionRightsRefusalOfThePreferredRouteIsRecordedWithoutFallback(t *testing.T) {
	f := newSelectionFixture(t, true, completedRemotePage, func(resource string) string {
		if resource == inference.ResourceHost("lab") {
			return "denied"
		}
		return "permitted"
	})
	got := f.start()
	if got.Outcome != wire.StartOutcomeNotPermitted || got.Reason != "rights:denied" || got.SelectionReason != nil {
		t.Fatalf("refusal: %+v", got)
	}
	r := f.record(t)
	if r.Outcome != "not_permitted" || r.Host != "lab" || r.Model != "remote-model" || r.Preferred != "remote-model" || r.Selection != "preferred" {
		t.Fatalf("refusal record: %+v", r)
	}
	if f.remoteStarts.Load() != 0 || f.localPosts.Load() != 0 {
		t.Fatalf("remote starts %d, local posts %d", f.remoteStarts.Load(), f.localPosts.Load())
	}
}

// After a known-unavailable preferred route, a refusal of the fallback keeps
// the fallback in its record: the refusal is never read as the preferred one.
func TestSelectionFallbackRefusalNamesThePreferredRoute(t *testing.T) {
	f := newSelectionFixture(t, false, nil, func(resource string) string {
		if resource == inference.ResourceHost("local") {
			return "denied"
		}
		return "permitted"
	})
	got := f.start()
	if got.Outcome != wire.StartOutcomeNotPermitted || got.SelectionReason != nil {
		t.Fatalf("fallback refusal: %+v", got)
	}
	r := f.record(t)
	if r.Outcome != "not_permitted" || r.Host != "local" || r.Model != "local-model" || r.Preferred != "remote-model" ||
		r.PreferredHost != "lab" || r.Selection != "preferred_route_unavailable" {
		t.Fatalf("fallback refusal record: %+v", r)
	}
	if f.localPosts.Load() != 0 {
		t.Fatalf("local posts %d", f.localPosts.Load())
	}
}

// The second policy row can be refused before pickCandidate; the continue
// from a known-unavailable first route must already mark the fallback.
func TestSelectionMalformedSecondCandidateRetainsFallbackNote(t *testing.T) {
	for _, image := range []bool{false, true} {
		t.Run(fmt.Sprint("image=", image), func(t *testing.T) {
			f := newSelectionFixture(t, false, nil, nil, wire.SelectionCandidateV2{Model: "refused-second"})
			f.lab.Profiles = append(f.lab.Profiles, router.ProfileImage)
			var got wire.AdmissionV2
			if image {
				got = f.provider.StartImageV2(t.Context(), selectionCaller, wire.ImageRequestV2{Target: wire.Target{SelectionPolicy: "everyday"}, Content: wire.ImageRequestContent{Mode: wire.ImageModeGenerate, Prompt: "fixture", Size: "1024x1024", Count: 1}})
			} else {
				got = f.start()
			}
			if got.Outcome != wire.StartOutcomeUnavailable || got.Reason != "selection:invalid" {
				t.Fatalf("second refusal: %+v", got)
			}
			r := f.record(t)
			if r.Model != "refused-second" || r.Preferred != "remote-model" || r.PreferredHost != "lab" || r.Selection != "preferred_route_unavailable" {
				t.Fatalf("fallback note: %+v", r)
			}
			if f.remoteStarts.Load() != 0 || f.localPosts.Load() != 0 {
				t.Fatal("refused selection dispatched")
			}
		})
	}
}
