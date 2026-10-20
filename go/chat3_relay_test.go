package inference

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

type chat3RelayFixture struct {
	starts             []wire.RequestV3
	windows, cancels   int
	admission          wire.ChatAdmission
	pages              []wire.ChatPage
	err                error
	onStart, onObserve func()
}

func (f *chat3RelayFixture) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	f.windows++
	return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeAvailable, Window: &acceptance.HistoryWindow{HistoryEpoch: "remote-epoch"}}, nil
}
func (f *chat3RelayFixture) Start(request wire.RequestV3) (wire.ChatAdmission, error) {
	if f.onStart != nil {
		f.onStart()
	}
	f.starts = append(f.starts, request)
	return f.admission, nil
}
func (f *chat3RelayFixture) Observe(string, int64, int64, int64, int64) (wire.ChatPage, error) {
	if f.onObserve != nil {
		f.onObserve()
	}
	if len(f.pages) == 0 {
		return wire.ChatPage{}, f.err
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}
func (f *chat3RelayFixture) Cancel(string) (wire.ChatCancellation, error) {
	f.cancels++
	return wire.ChatCancellation{Outcome: wire.CancelOutcomeCancelled}, nil
}

func chat3RelayOperation(t *testing.T, p *Provider) *operation {
	t.Helper()
	return chat3RelayOperationFor(t, p, caller)
}

func chat3RelayOperationFor(t *testing.T, p *Provider, subject Subject) *operation {
	t.Helper()
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = p.chat3History.window().HistoryEpoch
	e, owner, _, _, err := p.chat3History.reserve(subject, r.Identity, []byte("body"), p.cfg.Now())
	if err != nil || !owner {
		t.Fatal("reserve relay identity")
	}
	p.chat3History.begin(e)
	op := newOperation("relay-local", subject, &router.Host{Name: "lab/inner", Wire: router.WireRemote, Domain: "lab"}, "model", "", 32, p.cfg.Now())
	op.chat3 = &chat3Run{request: r, history: e}
	return op
}

func TestChat3RelayDerivedIdentityAndTerminalPassThrough(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	subject := caller
	subject.Program = filepath.Join(t.TempDir(), "chat.exe")
	op := chat3RelayOperationFor(t, f.provider, subject)
	zero := int64(0)
	finish := wire.FinishReason("future")
	terminal := wire.ChatResult{Outcome: wire.ChatResultOutcomeCompleted, Usage: wire.ChatUsage{Output: &zero}, FinishReason: &finish, ServedBy: &wire.ServedBy{Place: wire.ServedPlaceLocal, Server: "inner"}}
	relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeAccepted, Request: "remote-id"}, pages: []wire.ChatPage{{Outcome: wire.PageOutcomePage, Next: 3, AtEnd: true, Events: []wire.ChatEvent{
		{Sequence: 0, Kind: wire.ChatEventKindProgress, Progress: &wire.ChatProgress{Phase: wire.ChatProgressPhaseLoading}},
		{Sequence: 1, Kind: wire.ChatEventKindPart, Part: &wire.ChatPart{Block: 0, Kind: wire.ChatPartKindReasoning, Text: "thinking"}},
		{Sequence: 2, Kind: wire.ChatEventKindEnd, End: &terminal},
	}}}}
	result := f.provider.runChat3Relay(context.Background(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
	if result.Outcome != wire.ReplyOutcomeCompleted || len(relay.starts) != 1 || relay.windows != 1 || relay.cancels != 0 {
		t.Fatalf("relay result %+v starts %d windows %d cancels %d", result, len(relay.starts), relay.windows, relay.cancels)
	}
	forwarded := relay.starts[0]
	if forwarded.Identity.Key != chat3RelayKey(subject, op.chat3.request.Identity) || forwarded.Identity.HistoryEpoch != "remote-epoch" || forwarded.Target.Model != "model" {
		t.Fatalf("forwarded identity/target %+v", forwarded)
	}
	if forwarded.Content.Extensions[ClaimExtension] != "chat.exe" {
		t.Fatalf("missing attributed claim %+v", forwarded.Content.Extensions)
	}
	if f.provider.chat3History.remoteEpoch(op.chat3.history) != "remote-epoch" {
		t.Fatal("relay epoch not retained")
	}
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, End: &result}, time.Now())
	got := op.chat3.events[len(op.chat3.events)-1].End
	if !reflect.DeepEqual(got, &terminal) {
		t.Fatalf("inner terminal rewritten: got %+v want %+v", got, terminal)
	}
}

func TestChat3RelayTimingKeepsLocalClockAndServingRuntimeClaimsDistinct(t *testing.T) {
	base := time.Now()
	var millis atomic.Int64
	millis.Store(12)
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.Now = func() time.Time { return base.Add(time.Duration(millis.Load()) * time.Millisecond) }
	})
	op := chat3RelayOperation(t, f.provider)
	op.timing.begin(base, f.provider.cfg.Now())
	remoteQueue, remoteLoad, remotePrefill, remoteDecode := int64(4), int64(3100), int64(410), int64(2200)
	terminal := wire.ChatResult{Outcome: wire.ChatResultOutcomeCompleted, ServedBy: &wire.ServedBy{Place: wire.ServedPlaceRemote, Server: "inner", Machine: "nas"},
		Timing: &wire.ChatTiming{QueueMs: &remoteQueue, LoadMs: &remoteLoad, PrefillMs: &remotePrefill, DecodeMs: &remoteDecode}}
	relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeAccepted, Request: "remote-id"},
		onStart: func() { millis.Store(20) }, onObserve: func() { millis.Store(852) },
		pages: []wire.ChatPage{{Outcome: wire.PageOutcomePage, Next: 2, AtEnd: true, Events: []wire.ChatEvent{
			{Sequence: 0, Kind: wire.ChatEventKindProgress, Progress: &wire.ChatProgress{Phase: wire.ChatProgressPhaseLoading}},
			{Sequence: 1, Kind: wire.ChatEventKindEnd, End: &terminal}}}}}
	end := f.provider.runChat3Relay(context.Background(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
	millis.Store(900)
	op.timing.finish(f.provider.cfg.Now())
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, End: &end}, f.provider.cfg.Now())
	if !reflect.DeepEqual(op.chat3.terminal, &terminal) {
		t.Fatal("serving runtime terminal rewritten")
	}
	if *op.timing.facts.QueueMS != 12 || *op.timing.facts.RelayMS != 840 || op.timing.facts.LoadMS != nil || op.timing.facts.PrefillMS != nil || op.timing.facts.DecodeMS != nil {
		t.Fatalf("remote claim became local fact: %+v", op.timing.facts)
	}
	d := op.delegatedTimingLocked()
	if d == nil || *d.Timing.QueueMs != 4 || *d.Timing.LoadMs != 3100 || d.ServedBy.Machine != "nas" {
		t.Fatalf("delegated observations: %+v", d)
	}
	// The retained terminal and the audit callback own independent snapshots.
	*d.Timing.LoadMs = 1
	if *terminal.Timing.LoadMs != 3100 {
		t.Fatal("record mutation changed terminal")
	}
}

func TestChat3RelayUnknownOrFailedWaitNeverResubmitsOrImplicitlyCancels(t *testing.T) {
	for _, outcome := range []wire.ChatStartOutcome{wire.ChatStartOutcomeUnknown, wire.ChatStartOutcomeAccepted} {
		t.Run(outcome.String(), func(t *testing.T) {
			f := setup(t, router.WireOpenAICompatible, nil)
			op := chat3RelayOperation(t, f.provider)
			relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: outcome, Request: "remote-id"}, err: errors.New("wait lost")}
			result := f.provider.runChat3Relay(context.Background(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
			if result.Outcome != wire.ReplyOutcomeUnavailable || result.Reason != "remote:uncertain" || len(relay.starts) != 1 || relay.cancels != 0 {
				t.Fatalf("uncertainty triggered action: %+v starts %d cancels %d", result, len(relay.starts), relay.cancels)
			}
		})
	}
}

func TestChat3RelayUnknownProgressIsDisplayOnlyAndOriginStallPassesThrough(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	op := chat3RelayOperation(t, f.provider)
	terminal := wire.ChatResult{Outcome: wire.ChatResultOutcomeFailed, Cause: "stalled"}
	relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeAccepted, Request: "remote"}, pages: []wire.ChatPage{{Outcome: wire.PageOutcomePage, Next: 2, AtEnd: true, Events: []wire.ChatEvent{
		{Sequence: 0, Kind: wire.ChatEventKindProgress, Progress: &wire.ChatProgress{Phase: wire.ChatProgressPhase("future.phase")}},
		{Sequence: 1, Kind: wire.ChatEventKindEnd, End: &terminal},
	}}}}
	armed := false
	ctx := context.WithValue(context.Background(), generationSendKey{}, func() { armed = true })
	prefill := false
	ctx = context.WithValue(ctx, executionPrefillKey{}, func(int64, int64) bool { prefill = true; return true })
	end := f.provider.runChat3Relay(ctx, op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
	if armed || prefill || !op.generation.deadline.IsZero() {
		t.Fatal("relay created a second generation timer")
	}
	if len(op.chat3.events) != 1 || op.chat3.events[0].Progress.Phase != "future.phase" {
		t.Fatal("unknown display phase changed")
	}
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, End: &end}, time.Now())
	if !reflect.DeepEqual(op.chat3.events[1].End, &terminal) {
		t.Fatal("origin stalled terminal rewritten")
	}
}

func TestChat3RelayKeyIsScopedAndUnambiguous(t *testing.T) {
	id := acceptance.RequestIdentity{Key: "key", HistoryEpoch: "epoch"}
	first := chat3RelayKey(Subject{Account: "a", Program: "bc"}, id)
	if len(first) != 71 || first == chat3RelayKey(Subject{Account: "ab", Program: "c"}, id) {
		t.Fatal("relay key lacks scoped tuple boundary")
	}
	id.HistoryEpoch = "other"
	if first == chat3RelayKey(Subject{Account: "a", Program: "bc"}, id) {
		t.Fatal("caller epoch not included")
	}
}
