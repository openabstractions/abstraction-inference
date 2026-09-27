package inference

import (
	"bytes"
	"context"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type liveFixture struct {
	p                          *Provider
	connects, appends, commits atomic.Int64
	saved                      []byte
	revoke                     bool
}

type fixtureLiveConnection struct {
	f      *liveFixture
	events chan LiveEvent
	closed chan struct{}
	once   sync.Once
	closes atomic.Int64
}

func (c *fixtureLiveConnection) Append(_ context.Context, _ []byte) error {
	c.f.appends.Add(1)
	return nil
}

func (c *fixtureLiveConnection) Commit(_ context.Context) error {
	c.f.commits.Add(1)
	c.events <- LiveEvent{Kind: LiveEventAudio, Audio: []byte{1, 2, 3, 4}}
	c.events <- LiveEvent{Kind: LiveEventTranscript, Transcript: "hello"}
	c.events <- LiveEvent{Kind: LiveEventTranscript, Final: true}
	c.events <- LiveEvent{Kind: LiveEventTerminal, Final: true, Outcome: LiveCompleted, Usage: LiveUsage{InputTokens: 2, OutputTokens: 3}}
	return nil
}

func (c *fixtureLiveConnection) Read(ctx context.Context) (LiveEvent, error) {
	select {
	case event := <-c.events:
		return event, nil
	default:
	}
	select {
	case event := <-c.events:
		return event, nil
	case <-ctx.Done():
		return LiveEvent{}, NewLiveError(LiveErrorCancelled)
	case <-c.closed:
		return LiveEvent{}, NewLiveError(LiveErrorCancelled)
	}
}

func (c *fixtureLiveConnection) Close() {
	c.once.Do(func() {
		c.closes.Add(1)
		close(c.closed)
	})
}

func TestLiveAdapterRejectsUnknownVocabulary(t *testing.T) {
	for _, event := range []LiveEvent{
		{Kind: LiveEventKind(0)},
		{Kind: LiveEventKind(255)},
		{Kind: LiveEventTerminal},
		{Kind: LiveEventTerminal, Outcome: LiveOutcome(255)},
	} {
		connection := &fixtureLiveConnection{events: make(chan LiveEvent, 1)}
		connection.events <- event
		_, err := (localLiveBackend{connection: connection}).read(context.Background())
		if !IsLiveError(err, LiveErrorMalformed) {
			t.Fatalf("unknown vocabulary %+v: %v", event, err)
		}
	}
	for _, code := range []LiveErrorCode{0, 255} {
		err := NewLiveError(code)
		if !IsLiveError(err, LiveErrorUnavailable) || IsLiveError(err, code) {
			t.Fatalf("unknown adapter error %d was not safely classified: %v", code, err)
		}
	}
}

func TestLiveAdapterPreservesTerminalStatus(t *testing.T) {
	for outcome, want := range map[LiveOutcome]string{
		LiveCompleted: "completed", LiveCancelled: "cancelled", LiveFailed: "failed", LiveIncomplete: "incomplete",
	} {
		connection := &fixtureLiveConnection{events: make(chan LiveEvent, 1)}
		connection.events <- LiveEvent{Kind: LiveEventTerminal, Outcome: outcome}
		event, err := (localLiveBackend{connection: connection}).read(context.Background())
		if err != nil || event.outcome != want {
			t.Fatalf("terminal status %d: %+v, %v", outcome, event, err)
		}
	}
}

func newLiveFixture(t *testing.T, denied bool) *liveFixture {
	t.Helper()
	f := &liveFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-realtime"}]}`))
		}
	}))
	t.Cleanup(server.Close)
	routes := router.New(router.NewHosted("live", server.URL, router.WireOpenAIRealtime, "live"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	routes.Survey()
	cfg := Config{Router: routes, HTTP: server.Client(), SurveyAge: time.Hour, Idle: time.Second,
		LiveDialer: func(_ context.Context, client *http.Client, _, _, _ string, _ wire.LiveFormat, headers map[string]string) (LiveConnection, error) {
			if client.CheckRedirect == nil || headers["Authorization"] != "Bearer fixture-live-key" {
				t.Error("live connector did not receive guarded credential request")
			}
			f.connects.Add(1)
			return &fixtureLiveConnection{f: f, events: make(chan LiveEvent, 4), closed: make(chan struct{})}, nil
		},
		Decide: func(context.Context, Subject, string, string) (string, error) {
			if denied {
				return "denied", nil
			}
			return "permitted", nil
		},
		Apply: func(_ context.Context, s Subject, contract, credential, target string) (map[string]string, string) {
			if s != caller || contract != LiveContract {
				t.Errorf("wrong credential subject/contract: %+v %s", s, contract)
			}
			return map[string]string{"Authorization": "Bearer fixture-live-key"}, "applied"
		},
		PrepareContentWrite: func(_ context.Context, s Subject, profile, media string, limit int64) (ContentCommitter, ContentOutcome) {
			if s != caller || profile != "live" || media != liveMediaType {
				t.Errorf("wrong writer scope")
			}
			return func(_ context.Context, digest string, data []byte) ContentOutcome {
				if f.revoke {
					return ContentForbidden
				}
				if audioDigest(data) != digest {
					t.Errorf("wrong digest")
				}
				f.saved = append([]byte(nil), data...)
				return ContentResolved
			}, ContentResolved
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.surveyed = time.Now()
	f.p = p
	t.Cleanup(func() { p.Close() })
	return f
}
func liveRequest() wire.LiveRequest {
	return wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "live"}
}
func liveEnd(t *testing.T, p *Provider, id string) (wire.LiveReply, []wire.Delta) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var deltas []wire.Delta
	for cursor := int64(0); ctx.Err() == nil; {
		page := p.ObserveLive(ctx, caller, id, cursor, 1, 65536, 500)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("page %+v", page)
		}
		deltas = append(deltas, page.Deltas...)
		for _, d := range page.Deltas {
			if d.LiveEnd != nil {
				return *d.LiveEnd, deltas
			}
		}
		cursor = page.Next
	}
	t.Fatal("live result timed out")
	return wire.LiveReply{}, nil
}
func TestLiveLifecycleReplayOwnershipAndDelivery(t *testing.T) {
	f := newLiveFixture(t, false)
	ctx := context.Background()
	a := f.p.StartLive(ctx, caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	pcm := make([]byte, 9600)
	if got := f.p.AppendLive(ctx, Subject{Account: caller.Account, Program: "other"}, a.Operation, 0, pcm); got.Outcome != wire.LiveInputOutcomeUnknown {
		t.Fatal(got)
	}
	if got := f.p.AppendLive(ctx, caller, a.Operation, 1, pcm); got.Outcome != wire.LiveInputOutcomeOutOfOrder {
		t.Fatal(got)
	}
	for _, want := range []wire.LiveInputOutcome{wire.LiveInputOutcomeAccepted, wire.LiveInputOutcomeDuplicate} {
		if got := f.p.AppendLive(ctx, caller, a.Operation, 0, pcm); got.Outcome != want || got.NextSequence != 1 {
			t.Fatal(got)
		}
	}
	if got := f.p.Observe(ctx, caller, a.Operation, 0, 1, 100, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatal(got)
	}
	for _, want := range []wire.LiveInputOutcome{wire.LiveInputOutcomeAccepted, wire.LiveInputOutcomeDuplicate} {
		if got := f.p.CommitLive(ctx, caller, a.Operation); got.Outcome != want {
			t.Fatal(got)
		}
	}
	end, deltas := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.InputAudioBytes != 9600 || end.OutputAudioBytes != 4 || end.Delivery.Digest != audioDigest([]byte{1, 2, 3, 4}) {
		t.Fatalf("end %+v", end)
	}
	var audio []byte
	final := false
	for _, d := range deltas {
		if d.Audio != nil {
			audio = append(audio, d.Audio.Data...)
		}
		if d.Transcript != nil {
			final = final || d.Transcript.IsFinal
		}
	}
	if !bytes.Equal(audio, f.saved) || !final {
		t.Fatalf("audio mismatch or no final transcript marker")
	}
	if f.appends.Load() != 1 || f.commits.Load() != 1 {
		t.Fatal("duplicate sent upstream")
	}
	if got := f.p.AppendLive(ctx, caller, a.Operation, 1, pcm); got.Outcome != wire.LiveInputOutcomeClosed {
		t.Fatal(got)
	}
}
func TestLiveDenialBeforeConnectAndRevocationBeforeCommit(t *testing.T) {
	f := newLiveFixture(t, true)
	a := f.p.StartLive(context.Background(), caller, liveRequest())
	if a.Outcome != wire.StartOutcomeNotPermitted || f.connects.Load() != 0 {
		t.Fatal(a)
	}
	allowed := newLiveFixture(t, false)
	allowed.revoke = true
	a = allowed.p.StartLive(context.Background(), caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	allowed.p.AppendLive(context.Background(), caller, a.Operation, 0, make([]byte, 9600))
	allowed.p.CommitLive(context.Background(), caller, a.Operation)
	end, _ := liveEnd(t, allowed.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeForbidden || end.Reason != "output:forbidden" || end.Delivery.Digest != "" || len(allowed.saved) != 0 {
		t.Fatal(end)
	}
}

func TestLiveWithoutLocalAdapterRefusesBeforeAdmission(t *testing.T) {
	f := newLiveFixture(t, false)
	f.p.cfg.LiveDialer = nil
	for _, admission := range []wire.Admission{
		f.p.PreflightLive(context.Background(), caller, liveRequest()),
		f.p.StartLive(context.Background(), caller, liveRequest()),
	} {
		if admission.Outcome != wire.StartOutcomeUnavailable || admission.Reason != "upstream:unavailable" || admission.Operation != "" {
			t.Fatalf("missing local adapter admitted: %+v", admission)
		}
	}
	if f.connects.Load() != 0 {
		t.Fatal("missing local adapter connected upstream")
	}
}

func TestLiveDialErrorClosesRejectedConnection(t *testing.T) {
	f := newLiveFixture(t, false)
	connection := &fixtureLiveConnection{f: f, closed: make(chan struct{}), events: make(chan LiveEvent, 1)}
	f.p.cfg.LiveDialer = func(context.Context, *http.Client, string, string, string, wire.LiveFormat, map[string]string) (LiveConnection, error) {
		return connection, NewLiveError(LiveErrorRefused)
	}
	a := f.p.StartLive(context.Background(), caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	end, _ := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeRefused || connection.closes.Load() != 1 {
		t.Fatalf("dial error end %+v; closes %d", end, connection.closes.Load())
	}
	op := f.p.visibleProfile(caller, a.Operation, router.ProfileLive)
	op.live.mu.Lock()
	backend := op.live.backend
	op.live.mu.Unlock()
	if backend != nil {
		t.Fatal("rejected backend was published")
	}
}
func TestLiveIdleCancelsUpstream(t *testing.T) {
	f := newLiveFixture(t, false)
	a := f.p.StartLive(context.Background(), caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	op := f.p.visibleProfile(caller, a.Operation, router.ProfileLive)
	select {
	case <-op.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("idle did not cancel session")
	}
	end, _ := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCancelled || end.Reason != "idle" {
		t.Fatal(end)
	}
}
func TestLiveCompletionWinsLateCancellation(t *testing.T) {
	f := newLiveFixture(t, false)
	entered, release := make(chan struct{}), make(chan struct{})
	original := f.p.cfg.PrepareContentWrite
	f.p.cfg.PrepareContentWrite = func(ctx context.Context, s Subject, profile, media string, limit int64) (ContentCommitter, ContentOutcome) {
		commit, outcome := original(ctx, s, profile, media, limit)
		return func(ctx context.Context, digest string, data []byte) ContentOutcome {
			close(entered)
			<-release
			return commit(ctx, digest, data)
		}, outcome
	}
	ctx := context.Background()
	a := f.p.StartLive(ctx, caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	f.p.AppendLive(ctx, caller, a.Operation, 0, make([]byte, 9600))
	f.p.CommitLive(ctx, caller, a.Operation)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("commit did not start")
	}
	result := make(chan wire.Cancellation, 1)
	go func() { result <- f.p.CancelLive(caller, a.Operation) }()
	select {
	case r := <-result:
		close(release)
		t.Fatalf("cancel completed before result publication: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	r := <-result
	if r.Outcome != wire.CancelOutcomeEnded || r.ReplyOutcome == nil || *r.ReplyOutcome != wire.ReplyOutcomeCompleted {
		t.Fatal(r)
	}
	end, _ := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Delivery.Digest == "" {
		t.Fatal(end)
	}
}
