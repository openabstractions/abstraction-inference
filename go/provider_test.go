package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const secret = "sk-or-UPSTREAM-FIXTURE-5e2b91"

var caller = Subject{Account: "S-1-5-21-fixture", Program: `C:\apps\chat.exe`}

// upstream is a fake provider with a request log. It lists models, answers
// OpenAI chat completions and Anthropic messages as server-sent events, and
// records every request's method, path and headers.
type upstream struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	log    []logged
	// chunks is the text each streamed reply carries, one event per chunk.
	chunks []string
	// usage reported at the end of each reply.
	input, output int64
	cost          string
	// hold keeps a stream open, writing a chunk every interval, until the
	// client goes away; closed receives the time it went.
	hold     bool
	interval time.Duration
	closed   chan time.Time
	// release, when set, holds each stream after its first chunk until it is
	// closed, so a test can read before the retained window moves.
	release chan struct{}
	// embedBase64 answers embeddings as base64 float32 instead of number arrays.
	embedBase64 bool
	// transcript is the buffered transcription response. transcriptionHold
	// keeps the request open until its context is cancelled.
	transcript        string
	transcriptionHold bool
	speech            []byte
	speechStatus      int
}

// paused waits for release after a stream's first chunk; it reports false
// when the client went away first.
func (u *upstream) paused(r *http.Request, sent int) bool {
	if u.release == nil || sent != 1 {
		return true
	}
	select {
	case <-u.release:
		return true
	case <-r.Context().Done():
		return false
	}
}

type logged struct {
	method, path string
	header       http.Header
	body         string
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{t: t, chunks: []string{"Hel", "lo", "!"}, input: 7, output: 3, closed: make(chan time.Time, 4), interval: 20 * time.Millisecond}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) requests() []logged {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]logged(nil), u.log...)
}

func (u *upstream) posts() []logged {
	var out []logged
	for _, l := range u.requests() {
		if l.method == http.MethodPost {
			out = append(out, l)
		}
	}
	return out
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.log = append(u.log, logged{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body)})
	u.mu.Unlock()
	authorized := r.Header.Get("Authorization") == "Bearer "+secret || r.Header.Get("x-api-key") == secret || r.Header.Get("xi-api-key") == secret
	if !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher := w.(http.Flusher)
	send := func(event, data string) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models" && r.Header.Get("xi-api-key") == secret:
		w.Write([]byte(`[{"model_id":"eleven_multilingual_v2"}]`))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		w.Write([]byte(`{"data":[{"id":"anthropic/claude-sonnet-5"},{"id":"claude-sonnet-5"}]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/embeddings":
		u.embeddings(w, body)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/audio/transcriptions":
		if u.transcriptionHold {
			<-r.Context().Done()
			u.closed <- time.Now()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if u.transcript == "" {
			u.transcript = `{"language":"en","duration":30,"segments":[{"start":0,"end":10,"text":"one"},{"start":10,"end":20,"text":" two"},{"start":20,"end":30,"text":" three"}]}`
		}
		_, _ = io.WriteString(w, u.transcript)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/audio/speech":
		if u.speechStatus != 0 {
			http.Error(w, "fixture speech refusal", u.speechStatus)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		if len(u.speech) == 0 {
			u.speech = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 96)...)
		}
		for start := 0; start < len(u.speech); start += 32768 {
			end := min(start+32768, len(u.speech))
			_, _ = w.Write(u.speech[start:end])
			flusher.Flush()
		}
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/text-to-speech/") && strings.HasSuffix(r.URL.Path, "/stream"):
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3elevenlabs-audio"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/listen":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"metadata":{"duration":30},"results":{"utterances":[{"start":0,"end":30,"transcript":"deep gram"}],"channels":[{"detected_language":"en","alternatives":[{"words":[{"start":0,"end":1,"word":"deep"},{"start":1,"end":2,"word":"gram"}]}]}]}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chat/completions":
		w.Header().Set("Content-Type", "text/event-stream")
		for i, c := range u.chunks {
			raw, _ := json.Marshal(c)
			send("", `{"choices":[{"delta":{"content":`+string(raw)+`}}]}`)
			if !u.paused(r, i+1) {
				return
			}
		}
		if u.hold {
			u.holdOpen(r, func() { send("", `{"choices":[{"delta":{"content":"."}}]}`) })
			return
		}
		send("", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		cost := ""
		if u.cost != "" {
			cost = `,"cost":` + u.cost
		}
		send("", fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d%s}}`, u.input, u.output, cost))
		send("", "[DONE]")
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		w.Header().Set("Content-Type", "text/event-stream")
		send("message_start", fmt.Sprintf(`{"type":"message_start","message":{"usage":{"input_tokens":%d,"output_tokens":1}}}`, u.input))
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for i, c := range u.chunks {
			raw, _ := json.Marshal(c)
			send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(raw)+`}}`)
			if !u.paused(r, i+1) {
				return
			}
		}
		if u.hold {
			u.holdOpen(r, func() {
				send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"."}}`)
			})
			return
		}
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, u.output))
		send("message_stop", `{"type":"message_stop"}`)
	default:
		http.NotFound(w, r)
	}
}

func (u *upstream) holdOpen(r *http.Request, tick func()) {
	t := time.NewTicker(u.interval)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			u.closed <- time.Now()
			return
		case <-t.C:
			tick()
		}
	}
}

// policy is an exact-rule decision point; down makes it unreachable.
type policy struct {
	mu    sync.Mutex
	rules map[string]string
	down  bool
	asked []string
}

func (p *policy) decide(_ context.Context, s Subject, action, resource string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, s.Program+" "+action+" "+resource)
	if p.down {
		return "", errors.New("decision point down")
	}
	if word, ok := p.rules[s.Program+" "+action+" "+resource]; ok {
		return word, nil
	}
	return "not_granted", nil
}

// applier stands in for abstraction.credentials applier@1 and records uses.
type applier struct {
	mu     sync.Mutex
	header string
	uses   []string
}

func (a *applier) apply(_ context.Context, s Subject, consumer, name, target string) (map[string]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.uses = append(a.uses, s.Program+" "+consumer+" "+name+" "+target)
	if name != "openrouter" && name != "anthropic" && name != "deepgram" && name != "elevenlabs" {
		return nil, "unknown"
	}
	if a.header == "x-api-key" {
		return map[string]string{"x-api-key": secret}, "applied"
	}
	if a.header == "xi-api-key" {
		return map[string]string{"xi-api-key": secret}, "applied"
	}
	return map[string]string{"Authorization": "Bearer " + secret}, "applied"
}

func (a *applier) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.uses)
}

type fixture struct {
	up       *upstream
	provider *Provider
	policy   *policy
	applier  *applier
	mu       sync.Mutex
	records  []Record
	recorded chan struct{}
	errs     bytes.Buffer
}

func (f *fixture) logged() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.records...)
}

func (f *fixture) waitLogged(t *testing.T, count int) []Record {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if records := f.logged(); len(records) >= count {
			return records
		}
		select {
		case <-f.recorded:
		case <-timer.C:
			records := f.logged()
			if len(records) >= count {
				return records
			}
			t.Fatalf("audit callback did not record %d entries: %+v", count, records)
		}
	}
}

// setup builds a provider over one hosted host of the given wire. The router
// is surveyed once before the test, so every later upstream request is the
// provider's own.
func setup(t *testing.T, kind string, tune func(*Config)) *fixture {
	t.Helper()
	f := &fixture{up: newUpstream(t), policy: &policy{rules: map[string]string{}}, applier: &applier{}, recorded: make(chan struct{}, 64)}
	name, base := "openrouter", f.up.server.URL+"/api/v1"
	if kind == router.WireAnthropicMessages {
		name, base = "anthropic", f.up.server.URL
		f.applier.header = "x-api-key"
	}
	if kind == router.WireDeepgramPrerecorded {
		name, base = "deepgram", f.up.server.URL
	}
	if kind == router.WireElevenLabsStream {
		name, base = "elevenlabs", f.up.server.URL
		f.applier.header = "xi-api-key"
	}
	r := router.New(router.NewHosted(name, base, kind, name))
	r.UseCredentials(func(ctx context.Context, consumer, credential, target string) (map[string]string, error) {
		headers, outcome := f.applier.apply(ctx, Subject{Account: "runtime", Program: "runtime"}, consumer, credential, target)
		if outcome != "applied" {
			return nil, &router.CredentialRefusal{Outcome: outcome, Name: credential}
		}
		return headers, nil
	})
	r.Survey()
	f.policy.rules[caller.Program+" "+ActionComplete+" host:"+name] = "permitted"
	cfg := Config{Router: r, Decide: f.policy.decide, Apply: f.applier.apply, SurveyAge: time.Hour, Idle: 2 * time.Second,
		Record: func(rec Record) {
			f.mu.Lock()
			f.records = append(f.records, rec)
			f.mu.Unlock()
			select {
			case f.recorded <- struct{}{}:
			default:
			}
		},
		OnError: func(err error) { f.mu.Lock(); fmt.Fprintln(&f.errs, err); f.mu.Unlock() }}
	if tune != nil {
		tune(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Mark the router fresh: the survey above is the only listing request.
	p.surveyed = time.Now()
	t.Cleanup(func() { p.Close() })
	f.provider = p
	return f
}

func hostedRequest(credential string) wire.Request {
	return wire.Request{Model: "anthropic/claude-sonnet-5",
		Messages:   []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "Say hello"}}}},
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: credential}
}

// drain observes an operation to its end and folds text parts.
func drain(t *testing.T, p *Provider, s Subject, id string) (string, wire.Reply, []wire.Delta) {
	t.Helper()
	var text strings.Builder
	var all []wire.Delta
	cursor := int64(0)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		page := p.Observe(context.Background(), s, id, cursor, 256, 65536, 1000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Sequence != cursor {
				t.Fatalf("delta %d at cursor %d", d.Sequence, cursor)
			}
			cursor++
			all = append(all, d)
			if d.Kind == wire.DeltaKindPart && d.Part.Kind == wire.PartKindText {
				text.WriteString(d.Part.Text)
			}
			if d.Kind == wire.DeltaKindEnd {
				return text.String(), *d.End, all
			}
		}
		if page.Next != cursor {
			t.Fatalf("next %d, cursor %d", page.Next, cursor)
		}
	}
	t.Fatal("operation did not end")
	return "", wire.Reply{}, nil
}

func TestTheCredentialHeaderAppearsOncePerUpstreamRequestAndNowhereElse(t *testing.T) {
	for _, kind := range []string{router.WireOpenAICompatible, router.WireAnthropicMessages} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t, kind, nil)
			name := "openrouter"
			model := "anthropic/claude-sonnet-5"
			if kind == router.WireAnthropicMessages {
				name, model = "anthropic", "claude-sonnet-5"
			}
			req := hostedRequest(name)
			req.Model = model
			before := f.applier.count()
			admission := f.provider.Start(context.Background(), caller, req)
			if admission.Outcome != wire.StartOutcomeAccepted || admission.Host != name {
				t.Fatalf("admission %+v", admission)
			}
			text, reply, deltas := drain(t, f.provider, caller, admission.Operation)
			if text != "Hello!" || reply.Outcome != wire.ReplyOutcomeCompleted || reply.StopReason != wire.StopReasonEnd || reply.Usage.Input != 7 || reply.Usage.Output != 3 {
				t.Fatalf("reply %q %+v", text, reply)
			}
			if f.applier.count() != before+1 {
				t.Fatalf("applier called %d times for one request", f.applier.count()-before)
			}
			posts := f.up.posts()
			if len(posts) != 1 {
				t.Fatalf("%d upstream requests", len(posts))
			}
			header := "Authorization"
			if kind == router.WireAnthropicMessages {
				header = "x-api-key"
			}
			if got := posts[0].header.Values(header); len(got) != 1 || !strings.Contains(got[0], secret) {
				t.Fatalf("upstream %s values %q", header, got)
			}
			if strings.Contains(posts[0].body, secret) {
				t.Fatal("the secret entered the upstream body")
			}
			records := f.waitLogged(t, 1)
			evidence, _ := json.Marshal([]any{admission, deltas, reply, records})
			f.mu.Lock()
			evidence = append(evidence, f.errs.Bytes()...)
			f.mu.Unlock()
			if bytes.Contains(evidence, []byte(secret)) {
				t.Fatalf("a reply, delta, record or error carries the secret: %s", evidence)
			}
			if len(records) != 1 || records[0].Outcome != "completed" || records[0].Credential != name || records[0].TokensIn != 7 || records[0].TokensOut != 3 || records[0].Program != caller.Program {
				t.Fatalf("records %+v", records)
			}
		})
	}
}

func TestAProgramWithoutARuleIsNotPermittedAndTheUpstreamSeesNothing(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	seen := len(f.up.requests())
	stranger := Subject{Account: caller.Account, Program: `C:\apps\other.exe`}
	a := f.provider.Start(context.Background(), stranger, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:not_granted" || a.Operation != "" {
		t.Fatalf("admission %+v", a)
	}
	if n := len(f.up.requests()); n != seen {
		t.Fatalf("the upstream saw %d requests", n-seen)
	}
	if f.applier.count() != 1 {
		t.Fatalf("the applier was called for a refused program: %d uses", f.applier.count())
	}
	if r := f.logged(); len(r) != 1 || r[0].Outcome != "not_permitted" || r[0].Reason != "rights:not_granted" {
		t.Fatalf("refusal record %+v", r)
	}
}

func TestASpentCeilingRefusesBeforeTheRequest(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ceilings.json")
	limits := map[string]Ceiling{"openrouter": {TokensPerDay: 10, MicrosPerDay: 5_000_000}}
	ceilings, err := OpenCeilings(state, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.Ceilings = ceilings })
	f.up.cost = "0.0012345"
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("first admission %+v", a)
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Cost == nil || reply.Cost.Micros != 1234 {
		t.Fatalf("cost %+v", reply.Cost)
	}
	posts := len(f.up.posts())
	uses := f.applier.count()
	a = f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeBudgetExceeded || a.Reason != "ceiling:tokens:openrouter" {
		t.Fatalf("second admission %+v", a)
	}
	if len(f.up.posts()) != posts || f.applier.count() != uses {
		t.Fatal("a spent ceiling still applied the credential or sent the request")
	}
	reopened, err := OpenCeilings(state, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unit, over := reopened.Exceeded("openrouter"); !over || unit != "tokens" {
		t.Fatalf("counts did not survive a restart: %s %v", unit, over)
	}
	records := f.waitLogged(t, 2)
	if last := records[len(records)-1]; last.Ceiling != "tokens 10/10 micros 1234/5000000" {
		t.Fatalf("ceiling state %q", last.Ceiling)
	}
}

func TestDurableCeilingAccountingIsIdempotentAcrossReopenAndDayRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ceilings.json")
	now := time.Date(2026, 9, 19, 23, 59, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	ceilings, err := OpenCeilings(path, map[string]Ceiling{"replicate": {ImagesPerDay: 10}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := ceilings.AddDurableAttempt("operation-1", "replicate"); err != nil {
		t.Fatal(err)
	}
	if err := ceilings.AddDurableImages("operation-1", "replicate", 2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	reopened, err := OpenCeilings(path, map[string]Ceiling{"replicate": {ImagesPerDay: 10}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.AddDurableAttempt("operation-1", "replicate"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AddDurableImages("operation-1", "replicate", 2); err != nil {
		t.Fatal(err)
	}
	_, _, _, requests, images, _, _ := reopened.SpendUnits("replicate")
	if requests != 0 || images != 0 {
		t.Fatalf("requests=%d images=%d", requests, images)
	}
}

func TestDurableCeilingPersistFailureRollsBackAndRetries(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	path := filepath.Join(dir, "ceilings.json")
	ceilings, err := OpenCeilings(path, map[string]Ceiling{"replicate": {RequestsPerDay: 10}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ceilings.AddDurableAttempt("operation-1", "replicate"); err == nil {
		t.Fatal("persist failure was accepted")
	}
	_, _, _, requests, _, _, _ := ceilings.SpendUnits("replicate")
	if requests != 0 {
		t.Fatalf("failed persist mutated requests=%d", requests)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := ceilings.AddDurableAttempt("operation-1", "replicate"); err != nil {
		t.Fatal(err)
	}
	_, _, _, requests, _, _, _ = ceilings.SpendUnits("replicate")
	if requests != 1 {
		t.Fatalf("retry requests=%d", requests)
	}
}

func TestCallerExitClosesTheUpstreamWithinBudget(t *testing.T) {
	idle := 400 * time.Millisecond
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.Idle = idle })
	f.up.hold = true
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted || a.IdleMs != idle.Milliseconds() {
		t.Fatalf("admission %+v", a)
	}
	page := f.provider.Observe(context.Background(), caller, a.Operation, 0, 1, 65536, 5000)
	if len(page.Deltas) != 1 {
		t.Fatalf("first page %+v", page)
	}
	// The caller is gone: nothing observes from here.
	lastObserve := time.Now()
	select {
	case closed := <-f.up.closed:
		if took := closed.Sub(lastObserve); took > idle+time.Second {
			t.Fatalf("upstream closed %v after the last observe, budget %v", took, idle+time.Second)
		}
	case <-time.After(idle + 3*time.Second):
		t.Fatal("the upstream request stayed open after the caller left")
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeCancelled || reply.Reason != "idle" {
		t.Fatalf("reply %+v", reply)
	}
}

func TestAnObserverInFlightKeepsTheOperationAlive(t *testing.T) {
	idle := 300 * time.Millisecond
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.Idle = idle })
	f.up.hold = true
	f.up.interval = 2 * time.Second
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	cursor := int64(0)
	until := time.Now().Add(3 * idle)
	for time.Now().Before(until) {
		page := f.provider.Observe(context.Background(), caller, a.Operation, cursor, 256, 65536, 1000)
		cursor = page.Next
	}
	select {
	case <-f.up.closed:
		t.Fatal("an observed operation was closed as idle")
	default:
	}
	c := f.provider.Cancel(caller, a.Operation)
	if c.Outcome != wire.CancelOutcomeCancelled || c.ReplyOutcome == nil || *c.ReplyOutcome != wire.ReplyOutcomeCancelled {
		t.Fatalf("cancel %+v", c)
	}
	select {
	case <-f.up.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel left the upstream open")
	}
	c = f.provider.Cancel(caller, a.Operation)
	if c.Outcome != wire.CancelOutcomeEnded || *c.ReplyOutcome != wire.ReplyOutcomeCancelled {
		t.Fatalf("second cancel %+v", c)
	}
}

func TestIdleCandidateIsRecheckedBeforeCancellation(t *testing.T) {
	now := time.Now()
	idle := 300 * time.Millisecond
	op := newOperation("operation", caller, nil, "model", "", 16, now.Add(-idle))
	cancelled := make(chan struct{})
	op.cancel = func() { close(cancelled) }
	if op.idleSince(now) < idle {
		t.Fatal("fixture is not an idle candidate")
	}

	// The watchdog may have selected this candidate immediately before an
	// observer entered. Its cancellation decision must recheck that observer.
	op.observeBegin()
	if op.stopIdle(now, idle) {
		t.Fatal("an in-flight observer was cancelled as idle")
	}
	select {
	case <-cancelled:
		t.Fatal("an in-flight observer closed the upstream")
	default:
	}

	op.observeEnd(now)
	if !op.stopIdle(now.Add(idle), idle) {
		t.Fatal("an unobserved operation was not cancelled after its idle budget")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("idle cancellation did not close the upstream")
	}
}

func TestAReconnectResumesFromItsCursorAndAnOlderCursorReadsGap(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.RetainedDeltas = 4 })
	f.up.chunks = []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	// The upstream holds after its first chunk until the first page is read;
	// without the hold a fast stream moves the window past cursor 0 first.
	f.up.release = make(chan struct{})
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.RetainedDeltas != 4 {
		close(f.up.release)
		t.Fatalf("admission %+v", a)
	}
	// Read up to two deltas, then "disconnect" and let the stream finish.
	first := f.provider.Observe(context.Background(), caller, a.Operation, 0, 2, 65536, 2000)
	close(f.up.release)
	if first.Outcome != wire.PageOutcomePage || len(first.Deltas) == 0 {
		t.Fatalf("first page %+v", first)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ended := f.provider.visible(caller, a.Operation).endedOutcome(); ended || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 8 text deltas, one usage delta and the end: sequences 0..9; 6..9 retained.
	gap := f.provider.Observe(context.Background(), caller, a.Operation, first.Next, 256, 65536, 0)
	if gap.Outcome != wire.PageOutcomeGap || gap.Next != 6 || len(gap.Deltas) != 0 {
		t.Fatalf("older cursor: %+v", gap)
	}
	resumed := f.provider.Observe(context.Background(), caller, a.Operation, 7, 256, 65536, 0)
	if resumed.Outcome != wire.PageOutcomePage || len(resumed.Deltas) != 3 || resumed.Deltas[0].Sequence != 7 || !resumed.AtEnd || resumed.Next != 10 {
		t.Fatalf("resumed page %+v", resumed)
	}
	again := f.provider.Observe(context.Background(), caller, a.Operation, 10, 256, 65536, 0)
	if again.Outcome != wire.PageOutcomePage || len(again.Deltas) != 0 || !again.AtEnd {
		t.Fatalf("after end %+v", again)
	}
}

func TestResumeWhileStreamingLosesNothing(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.up.chunks = strings.Split("the quick brown fox jumps over the lazy dog", "")
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	var text strings.Builder
	cursor := int64(0)
	for {
		page := f.provider.Observe(context.Background(), caller, a.Operation, cursor, 3, 65536, 1000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("page %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindPart {
				text.WriteString(d.Part.Text)
			}
		}
		cursor = page.Next
		if page.AtEnd {
			break
		}
	}
	if text.String() != "the quick brown fox jumps over the lazy dog" {
		t.Fatalf("folded %q", text.String())
	}
}

func TestUnsupportedFeatureIsRefusedBeforeSpend(t *testing.T) {
	f := setup(t, router.WireAnthropicMessages, nil)
	seen, uses := len(f.up.requests()), f.applier.count()
	schema := hostedRequest("anthropic")
	schema.Model = "claude-sonnet-5"
	schema.Options = &wire.Options{JSONSchema: `{"type":"object"}`}
	a := f.provider.Start(context.Background(), caller, schema)
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "feature:"+FeatureJSONSchema {
		t.Fatalf("json schema on anthropic-messages: %+v", a)
	}
	extension := hostedRequest("anthropic")
	extension.Model = "claude-sonnet-5"
	extension.Extensions = map[string]string{"example/speculative": "on"}
	extension.RequiredExtensions = []string{"example/speculative"}
	a = f.provider.Start(context.Background(), caller, extension)
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "extension:example/speculative" {
		t.Fatalf("required unknown extension: %+v", a)
	}
	if len(f.up.requests()) != seen || f.applier.count() != uses || len(f.policy.asked) != 0 {
		t.Fatal("an unsupported request reached the upstream, the applier or the decision point")
	}
}

func TestARightsOutageReadsUnavailable(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.policy.down = true
	seen, uses := len(f.up.requests()), f.applier.count()
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "rights:unavailable" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.requests()) != seen || f.applier.count() != uses {
		t.Fatal("a decision outage still spent")
	}
}

func TestGuaranteesAndCredentialsSelectHosts(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	local := hostedRequest("openrouter")
	local.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	if a := f.provider.Start(context.Background(), caller, local); a.Outcome != wire.StartOutcomeNoHost || a.Reason != "router:unauthorised" {
		t.Fatalf("local-only: %+v", a)
	}
	other := hostedRequest("someone-else")
	if a := f.provider.Start(context.Background(), caller, other); a.Outcome != wire.StartOutcomeNoHost {
		t.Fatalf("another credential name: %+v", a)
	}
	both := hostedRequest("openrouter")
	both.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly, wire.RequestGuaranteeHostedAllowed}
	if a := f.provider.Start(context.Background(), caller, both); a.Outcome != wire.StartOutcomeInvalid || a.Reason != "guarantees" {
		t.Fatalf("both guarantees: %+v", a)
	}
	future := hostedRequest("openrouter")
	future.Guarantees = []wire.RequestGuarantee{wire.RequestGuarantee("example.runtime/private-routing@1")}
	roundTrip, err := wire.Decode(wire.Encode(&future))
	if err != nil || roundTrip == nil || len(roundTrip.Guarantees) != 1 || roundTrip.Guarantees[0] != future.Guarantees[0] {
		t.Fatalf("future guarantee codec round trip: %+v %v", roundTrip, err)
	}
	if a := f.provider.Start(context.Background(), caller, *roundTrip); a.Outcome != wire.StartOutcomeInvalid || a.Reason != "guarantees" {
		t.Fatalf("future guarantee: %+v", a)
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("a refused request was sent")
	}
}

// chat@1 picks by the chat profile: a hosted host declaring only embed is
// never sent a chat request.
func TestAHostWithoutTheChatProfileIsNotPicked(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.provider.cfg.Router.Hosts()[0].Profiles = []string{router.ProfileEmbed}
	if a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter")); a.Outcome != wire.StartOutcomeNoHost || a.Reason != "router:no-host" {
		t.Fatalf("chat on an embed host: %+v", a)
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("a refused request was sent")
	}
}

func TestAnApplierRefusalIsNotPermittedWithItsWord(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	r := router.New(router.NewHosted("openrouter", f.up.server.URL+"/api/v1", router.WireOpenAICompatible, "revoked-key"))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer " + secret}, nil
	})
	r.Survey()
	f.provider.cfg.Router = r
	a := f.provider.Start(context.Background(), caller, hostedRequest("revoked-key"))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "credential:unknown:revoked-key" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("a refused credential still sent the request")
	}
}

func TestOperationsAreVisibleOnlyToTheirCaller(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	stranger := Subject{Account: caller.Account, Program: `C:\apps\other.exe`}
	if page := f.provider.Observe(context.Background(), stranger, a.Operation, 0, 1, 1024, 0); page.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("stranger observe %+v", page)
	}
	if c := f.provider.Cancel(stranger, a.Operation); c.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("stranger cancel %+v", c)
	}
	if page := f.provider.Observe(context.Background(), Subject{}, a.Operation, 0, 1, 1024, 0); page.Outcome != wire.PageOutcomeForbidden {
		t.Fatalf("unbound observe %+v", page)
	}
	drain(t, f.provider, caller, a.Operation)
}

func TestAnUpstreamRefusalReadsRefused(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.provider.cfg.Apply = func(context.Context, Subject, string, string, string) (map[string]string, string) {
		return map[string]string{"Authorization": "Bearer wrong"}, "applied"
	}
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != "upstream:401" {
		t.Fatalf("reply %+v", reply)
	}
}
