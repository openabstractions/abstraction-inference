package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type liveGatewayProvider struct {
	mu         sync.Mutex
	request    wire.LiveRequest
	preflight  wire.LiveRequest
	subject    inference.Subject
	appends    [][]byte
	sequence   []int64
	starts     atomic.Int64
	preflights atomic.Int64
	commits    atomic.Int64
	cancels    atomic.Int64
	// refuse, when set to a real outcome, is what PreflightLive answers with
	// instead of admitting the connection. Zero admits.
	refuse wire.StartOutcome
	reason string
	// idleMS is the wait budget the preflight reports to the window.
	idleMS int64
}

func (p *liveGatewayProvider) PreflightLive(_ context.Context, subject inference.Subject, request wire.LiveRequest) wire.Admission {
	p.mu.Lock()
	p.preflight = request
	refuse, reason := p.refuse, p.reason
	p.mu.Unlock()
	p.preflights.Add(1)
	if refuse != 0 {
		return wire.Admission{Outcome: refuse, Reason: reason}
	}
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Host: "fixture-live", Model: request.Model, IdleMs: p.idleMS}
}

func (*liveGatewayProvider) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{Outcome: wire.StartOutcomeUnavailable}
}
func (*liveGatewayProvider) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{Outcome: wire.PageOutcomeUnknown}
}
func (*liveGatewayProvider) Cancel(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeUnknown}
}
func (p *liveGatewayProvider) StartLive(_ context.Context, subject inference.Subject, request wire.LiveRequest) wire.Admission {
	p.mu.Lock()
	p.subject, p.request = subject, request
	p.mu.Unlock()
	p.starts.Add(1)
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "live-operation"}
}
func (p *liveGatewayProvider) AppendLive(_ context.Context, _ inference.Subject, _ string, sequence int64, audio []byte) wire.LiveInputResult {
	p.mu.Lock()
	p.appends = append(p.appends, append([]byte(nil), audio...))
	p.sequence = append(p.sequence, sequence)
	p.mu.Unlock()
	return wire.LiveInputResult{Outcome: wire.LiveInputOutcomeAccepted, NextSequence: sequence + 1}
}
func (p *liveGatewayProvider) CommitLive(_ context.Context, _ inference.Subject, _ string) wire.LiveInputResult {
	p.commits.Add(1)
	p.mu.Lock()
	next := int64(len(p.appends))
	p.mu.Unlock()
	return wire.LiveInputResult{Outcome: wire.LiveInputOutcomeAccepted, NextSequence: next}
}
func (p *liveGatewayProvider) ObserveLive(ctx context.Context, _ inference.Subject, _ string, cursor, _, _, waitMS int64) wire.DeltaPage {
	if p.commits.Load() == 0 {
		select {
		case <-ctx.Done():
			return wire.DeltaPage{Outcome: wire.PageOutcomeUnavailable}
		case <-time.After(time.Duration(waitMS) * time.Millisecond):
			return wire.DeltaPage{Outcome: wire.PageOutcomePage, Next: cursor}
		}
	}
	if cursor != 0 {
		return wire.DeltaPage{Outcome: wire.PageOutcomePage, Next: cursor, AtEnd: true}
	}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{
		{Sequence: 0, Kind: wire.DeltaKindAudio, Audio: &wire.AudioChunk{Index: 0, Data: []byte{4, 5, 6, 7}}},
		{Sequence: 1, Kind: wire.DeltaKindTranscript, Transcript: &wire.LiveTranscript{Text: "hello "}},
		{Sequence: 2, Kind: wire.DeltaKindTranscript, Transcript: &wire.LiveTranscript{Text: "there", IsFinal: true}},
		{Sequence: 3, Kind: wire.DeltaKindEnd, LiveEnd: &wire.LiveReply{Outcome: wire.ReplyOutcomeCompleted, Usage: wire.Usage{Input: 3, Output: 5}}},
	}, Next: 4, AtEnd: true}
}
func (p *liveGatewayProvider) CancelLive(inference.Subject, string) wire.Cancellation {
	p.cancels.Add(1)
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

type liveGatewayBound struct {
	subject inference.Subject
	revoked atomic.Bool
	checks  atomic.Int64
	closes  atomic.Int64
}

func (b *liveGatewayBound) Subject() inference.Subject { return b.subject }
func (*liveGatewayBound) Rung() string                 { return "fixture/live" }
func (b *liveGatewayBound) Recheck() error {
	b.checks.Add(1)
	if b.revoked.Load() {
		return errors.New("peer moved")
	}
	return nil
}
func (b *liveGatewayBound) Close() error { b.closes.Add(1); return nil }

type liveGatewayFixture struct {
	window   *Window
	provider *liveGatewayProvider
	bound    *liveGatewayBound
	keyGone  atomic.Bool
	serve    chan error
}

func newLiveGatewayFixture(t *testing.T) *liveGatewayFixture {
	t.Helper()
	f := &liveGatewayFixture{provider: &liveGatewayProvider{}, bound: &liveGatewayBound{subject: inference.Subject{Account: "account", Program: "/apps/voice"}}, serve: make(chan error, 1)}
	window, err := Listen("127.0.0.1:0", Config{Chat: f.provider, Account: "account",
		Bind: func(net.Conn, time.Time) (Bound, error) { return f.bound, nil },
		Keys: func(_ context.Context, subject inference.Subject, key string) (Grant, string) {
			if f.keyGone.Load() {
				return Grant{}, KeyRevoked
			}
			if subject != f.bound.subject || key != "live-key" {
				return Grant{}, KeyWrongProgram
			}
			return Grant{Credential: "hosted-live"}, KeyVerified
		}, OnError: func(err error) { t.Log(err) }})
	if err != nil {
		t.Fatal(err)
	}
	f.window = window
	go func() { f.serve <- window.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = window.Close()
		select {
		case err := <-f.serve:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return f
}

// liveModel is the model the fixture's connections name in the Realtime URL.
const liveModel = "gpt-realtime"

func (f *liveGatewayFixture) url(query string) string {
	return "ws://" + f.window.Addr().String() + "/v1/realtime" + query
}

func (f *liveGatewayFixture) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	conn, response, err := f.tryDial(t, "?model="+liveModel)
	if err != nil {
		if response != nil {
			t.Fatalf("dial status %s: %v", response.Status, err)
		}
		t.Fatal(err)
	}
	return conn
}

func (f *liveGatewayFixture) tryDial(t *testing.T, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	header := http.Header{"Authorization": []string{"Bearer live-key"}}
	return websocket.Dial(ctx, f.url(query), &websocket.DialOptions{HTTPHeader: header})
}

func writeLiveEvent(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readLiveEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

// TestRealtimeGatewayDecidesRightsBeforeUpgrade holds INF-W8's order: the
// complete decision on the routed host answers with an HTTP status while the
// connection is still an ordinary request, and no socket is ever upgraded for
// a program the rule refuses.
func TestRealtimeGatewayDecidesRightsBeforeUpgrade(t *testing.T) {
	for _, test := range []struct {
		name    string
		query   string
		outcome wire.StartOutcome
		reason  string
		status  int
		word    string
	}{
		{name: "not permitted", query: "?model=" + liveModel, outcome: wire.StartOutcomeNotPermitted, reason: "rights:not_granted", status: http.StatusForbidden, word: "not_permitted"},
		{name: "no host", query: "?model=" + liveModel, outcome: wire.StartOutcomeNoHost, reason: "router:no_profile", status: http.StatusNotFound, word: "no_host"},
		{name: "wire without live", query: "?model=" + liveModel, outcome: wire.StartOutcomeUnsupportedFeature, reason: "wire:openai-compatible", status: http.StatusBadRequest, word: "unsupported_feature"},
		{name: "no model in the url", query: "", status: http.StatusBadRequest, word: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLiveGatewayFixture(t)
			f.provider.mu.Lock()
			f.provider.refuse, f.provider.reason = test.outcome, test.reason
			f.provider.mu.Unlock()
			conn, response, err := f.tryDial(t, test.query)
			if err == nil {
				conn.CloseNow()
				t.Fatal("the connection was upgraded despite the refusal")
			}
			if response == nil {
				t.Fatalf("no HTTP response: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("error body: %v", err)
			}
			if body.Error.Code != test.word {
				t.Fatalf("error code = %q, want %q", body.Error.Code, test.word)
			}
			if f.provider.starts.Load() != 0 {
				t.Fatal("a refused connection reached native StartLive")
			}
			if test.query == "" && f.provider.preflights.Load() != 0 {
				t.Fatal("a request without a model reached the provider")
			}
			if test.query != "" {
				f.provider.mu.Lock()
				defer f.provider.mu.Unlock()
				if f.provider.preflight.Model != liveModel || f.provider.preflight.Credential != "hosted-live" {
					t.Fatalf("preflight request = %+v", f.provider.preflight)
				}
			}
		})
	}
}

// TestRealtimeGatewayClosesAnIdleConnection spends the wait budget the
// preflight reported, both before a session is admitted and after, and holds
// that an admitted session is cancelled when the budget runs out.
func TestRealtimeGatewayClosesAnIdleConnection(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure bool
		cancels   int64
	}{
		{name: "before session.update", configure: false, cancels: 0},
		{name: "after session.update", configure: true, cancels: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLiveGatewayFixture(t)
			f.provider.idleMS = 150
			conn := f.dial(t)
			defer conn.CloseNow()
			assertLiveType(t, readLiveEvent(t, conn), "session.created")
			if test.configure {
				writeLiveEvent(t, conn, validLiveSession())
				assertLiveType(t, readLiveEvent(t, conn), "session.updated")
			}
			event := readLiveEvent(t, conn)
			assertLiveType(t, event, "error")
			body, _ := event["error"].(map[string]any)
			if body == nil || body["code"] != "cancelled" {
				t.Fatalf("idle error = %#v", event)
			}
			deadline := time.Now().Add(3 * time.Second)
			for f.provider.cancels.Load() != test.cancels && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if f.provider.cancels.Load() != test.cancels {
				t.Fatalf("native cancels = %d, want %d", f.provider.cancels.Load(), test.cancels)
			}
		})
	}
}

// TestRealtimeGatewayBindsTheSessionToTheConnectionModel keeps session.update
// from naming a model other than the one complete was decided against.
func TestRealtimeGatewayBindsTheSessionToTheConnectionModel(t *testing.T) {
	f := newLiveGatewayFixture(t)
	conn := f.dial(t)
	defer conn.CloseNow()
	assertLiveType(t, readLiveEvent(t, conn), "session.created")
	other := validLiveSession()
	other["session"].(map[string]any)["model"] = "another-realtime"
	writeLiveEvent(t, conn, other)
	assertLiveType(t, readLiveEvent(t, conn), "error")
	if f.provider.starts.Load() != 0 {
		t.Fatal("a second model reached native StartLive")
	}
}

func assertLiveType(t *testing.T, event map[string]any, want string) {
	t.Helper()
	if event["type"] != want {
		t.Fatalf("event type = %v, want %q: %#v", event["type"], want, event)
	}
}

func validLiveSession() map[string]any {
	return map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime", "model": liveModel, "output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "turn_detection": nil},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "voice": "marin"},
		},
	}}
}

func TestRealtimeGatewayDefersNativeCommitUntilResponseCreate(t *testing.T) {
	f := newLiveGatewayFixture(t)
	conn := f.dial(t)
	defer conn.CloseNow()
	assertLiveType(t, readLiveEvent(t, conn), "session.created")
	writeLiveEvent(t, conn, validLiveSession())
	assertLiveType(t, readLiveEvent(t, conn), "session.updated")

	audio := make([]byte, 4800)
	for i := range audio {
		audio[i] = byte(i)
	}
	writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(audio)})
	writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.commit"})
	assertLiveType(t, readLiveEvent(t, conn), "input_audio_buffer.committed")
	if got := f.provider.commits.Load(); got != 0 {
		t.Fatalf("native commits after buffer commit = %d, want 0", got)
	}
	writeLiveEvent(t, conn, map[string]any{"type": "response.create"})
	assertLiveType(t, readLiveEvent(t, conn), "response.created")

	wantTypes := []string{"response.output_audio.delta", "response.output_audio_transcript.delta", "response.output_audio_transcript.delta", "response.output_audio_transcript.done", "response.output_audio.done", "response.done"}
	var last map[string]any
	for _, want := range wantTypes {
		last = readLiveEvent(t, conn)
		assertLiveType(t, last, want)
	}
	response, ok := last["response"].(map[string]any)
	if !ok || response["status"] != "completed" {
		t.Fatalf("terminal response = %#v", last)
	}
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if f.provider.starts.Load() != 1 || f.provider.commits.Load() != 1 || len(f.provider.appends) != 1 || f.provider.sequence[0] != 0 || string(f.provider.appends[0]) != string(audio) {
		t.Fatalf("native calls: starts=%d commits=%d sequences=%v appends=%d", f.provider.starts.Load(), f.provider.commits.Load(), f.provider.sequence, len(f.provider.appends))
	}
	if f.provider.subject != f.bound.subject || f.provider.request.Model != "gpt-realtime" || f.provider.request.Voice != "marin" || f.provider.request.Format != wire.LiveFormatPcm1624000 || f.provider.request.Credential != "hosted-live" {
		t.Fatalf("native request = %+v for %+v", f.provider.request, f.provider.subject)
	}
}

func TestRealtimeGatewayRejectsUnsupportedAndOversizedInput(t *testing.T) {
	t.Run("exact maximum append", func(t *testing.T) {
		f := newLiveGatewayFixture(t)
		conn := f.dial(t)
		defer conn.CloseNow()
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, validLiveSession())
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(make([]byte, maxRealtimeAudioBytes))})
		writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.commit"})
		assertLiveType(t, readLiveEvent(t, conn), "input_audio_buffer.committed")
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		if len(f.provider.appends) != 1 || len(f.provider.appends[0]) != maxRealtimeAudioBytes {
			t.Fatalf("appended chunks = %d", len(f.provider.appends))
		}
	})
	t.Run("unsupported session", func(t *testing.T) {
		f := newLiveGatewayFixture(t)
		conn := f.dial(t)
		defer conn.CloseNow()
		readLiveEvent(t, conn)
		bad := validLiveSession()
		session := bad["session"].(map[string]any)
		audio := session["audio"].(map[string]any)
		input := audio["input"].(map[string]any)
		input["turn_detection"] = map[string]any{"type": "semantic_vad"}
		writeLiveEvent(t, conn, bad)
		assertLiveType(t, readLiveEvent(t, conn), "error")
		if f.provider.starts.Load() != 0 {
			t.Fatal("unsupported session reached native StartLive")
		}
	})
	t.Run("oversized append", func(t *testing.T) {
		f := newLiveGatewayFixture(t)
		conn := f.dial(t)
		defer conn.CloseNow()
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, validLiveSession())
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(make([]byte, maxRealtimeAudioBytes+2))})
		assertLiveType(t, readLiveEvent(t, conn), "error")
		deadline := time.Now().Add(time.Second)
		for f.provider.cancels.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if f.provider.cancels.Load() != 1 {
			t.Fatalf("cancels = %d, want 1", f.provider.cancels.Load())
		}
	})
	t.Run("malformed base64", func(t *testing.T) {
		f := newLiveGatewayFixture(t)
		conn := f.dial(t)
		defer conn.CloseNow()
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, validLiveSession())
		readLiveEvent(t, conn)
		writeLiveEvent(t, conn, map[string]any{"type": "input_audio_buffer.append", "audio": "AAAA!"})
		assertLiveType(t, readLiveEvent(t, conn), "error")
		if f.provider.starts.Load() != 1 {
			t.Fatal("valid session was not admitted")
		}
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		if len(f.provider.appends) != 0 {
			t.Fatal("malformed audio reached native AppendLive")
		}
	})
}

func TestRealtimeGatewayTerminatesOnLostAuthorityDisconnectAndWindowClose(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(*liveGatewayFixture, *websocket.Conn)
	}{
		{name: "key revoked", stop: func(f *liveGatewayFixture, _ *websocket.Conn) { f.keyGone.Store(true) }},
		{name: "peer moved", stop: func(f *liveGatewayFixture, _ *websocket.Conn) { f.bound.revoked.Store(true) }},
		{name: "disconnect", stop: func(_ *liveGatewayFixture, c *websocket.Conn) { _ = c.CloseNow() }},
		{name: "window close", stop: func(f *liveGatewayFixture, _ *websocket.Conn) { _ = f.window.Close() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLiveGatewayFixture(t)
			conn := f.dial(t)
			readLiveEvent(t, conn)
			writeLiveEvent(t, conn, validLiveSession())
			readLiveEvent(t, conn)
			test.stop(f, conn)
			deadline := time.Now().Add(3 * time.Second)
			for f.provider.cancels.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if f.provider.cancels.Load() != 1 {
				t.Fatalf("native cancels = %d, want 1", f.provider.cancels.Load())
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for {
				_, _, err := conn.Read(ctx)
				if err != nil {
					break
				}
			}
		})
	}
}
