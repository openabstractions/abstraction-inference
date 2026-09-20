package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/coder/websocket"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"net/http"
	"net/http/httptest"
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

func newLiveFixture(t *testing.T, denied bool) *liveFixture {
	t.Helper()
	f := &liveFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Write([]byte(`{"data":[{"id":"gpt-realtime"}]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-live-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		f.connects.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		if _, _, err = c.Read(ctx); err != nil {
			return
		}
		if err = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated"}`)); err != nil {
			return
		}
		for {
			_, b, e := c.Read(ctx)
			if e != nil {
				return
			}
			var msg struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(b, &msg) != nil {
				return
			}
			switch msg.Type {
			case "input_audio_buffer.append":
				f.appends.Add(1)
			case "input_audio_buffer.commit":
				f.commits.Add(1)
			case "response.create":
				for _, event := range []string{
					`{"type":"response.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}) + `"}`,
					`{"type":"response.output_audio_transcript.delta","delta":"hello"}`,
					`{"type":"response.output_audio_transcript.done","transcript":"hello"}`,
					`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":3}}}`,
				} {
					if c.Write(ctx, websocket.MessageText, []byte(event)) != nil {
						return
					}
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	routes := router.New(router.NewHosted("live", server.URL, router.WireOpenAIRealtime, "live"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	routes.Survey()
	cfg := Config{Router: routes, HTTP: server.Client(), SurveyAge: time.Hour, Idle: time.Second,
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
	if end.Outcome != wire.ReplyOutcomeForbidden || end.Delivery.Digest != "" || len(allowed.saved) != 0 {
		t.Fatal(end)
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
