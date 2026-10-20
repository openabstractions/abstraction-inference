package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// The founding use (P-M9; research/origin.txt:133-139): a plain desktop chat
// client with a placeholder key talks to deepseek-reasoner with tools through
// the gateway. The fixture provider answers like deepseek-reasoner: every
// reply carries reasoning_content, and a tool-call turn sent back without its
// exact reasoning_content is refused with HTTP 400, as DeepSeek documents.
// Its usage object has DeepSeek's documented fields and no cost, so the
// founding preset's budget is counted in tokens (INF-C2).

const (
	deepseekSecret = "sk-fixture-deepseek-0123456789abcdef"
	deepseekModel  = "deepseek-reasoner"
	placeholderKey = "placeholder"
	deepseekTrace  = "The user wants the weather; call get_weather for Riga."
	secondTrace    = "The tool answered; reply briefly."
)

type deepseekFixture struct {
	server  *httptest.Server
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
	echoed  bool
}

func (d *deepseekFixture) posts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bodies)
}

func newDeepseekFixture(t *testing.T) *deepseekFixture {
	d := &deepseekFixture{}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+deepseekSecret {
			http.Error(w, `{"error":{"message":"Authentication Fails"}}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"object":"list","data":[{"id":"` + deepseekModel + `","object":"model"}]}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.bodies = append(d.bodies, string(raw))
		d.headers = append(d.headers, r.Header.Clone())
		d.mu.Unlock()
		var body struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(raw, &body) != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		toolResult := false
		for _, m := range body.Messages {
			var role string
			json.Unmarshal(m["role"], &role)
			if role == "tool" {
				toolResult = true
			}
			if role != "assistant" {
				continue
			}
			if _, calls := m["tool_calls"]; !calls {
				continue
			}
			var echoed string
			if json.Unmarshal(m["reasoning_content"], &echoed) != nil || echoed != deepseekTrace {
				http.Error(w, `{"error":{"message":"Missing reasoning_content field in the assistant message","type":"invalid_request_error"}}`, http.StatusBadRequest)
				return
			}
			d.mu.Lock()
			d.echoed = true
			d.mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		send := func(data string) { fmt.Fprintf(w, "data: %s\n\n", data); flusher.Flush() }
		trace := deepseekTrace
		if toolResult {
			trace = secondTrace
		}
		for _, piece := range strings.SplitAfter(trace, " ") {
			raw, _ := json.Marshal(piece)
			send(`{"choices":[{"index":0,"delta":{"content":null,"reasoning_content":` + string(raw) + `}}]}`)
		}
		if toolResult {
			send(`{"choices":[{"index":0,"delta":{"content":"Riga is 12 C."}}]}`)
			send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":12,"total_tokens":52,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":40,"completion_tokens_details":{"reasoning_tokens":7}}}`)
		} else {
			send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_00_riga","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Riga\"}"}}]}}]}`)
			send(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":20,"total_tokens":50,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":30,"completion_tokens_details":{"reasoning_tokens":12}}}`)
		}
		send("[DONE]")
	}))
	t.Cleanup(d.server.Close)
	return d
}

// foundingUse is the window, provider and fixture provider of the founding
// use. Its decision point is the program's rule: unset or revoked, it reads
// not_granted and admits the first-spend question; answered allow,
// permitted; blocked, denied with no question.
type foundingUse struct {
	up       *deepseekFixture
	base     string
	mu       sync.Mutex
	rule     string // "", "allow" or "block"
	asked    chan string
	answers  chan string
	awaited  []string
	records  []inference.Record
	ceilings *inference.Ceilings
	logs     bytes.Buffer
	wait     time.Duration
	keyCheck KeyCheck
}

const firstSpendQuestion = "first-use:jan-deepseek"

// placeholderKeys is the runtime's key rule (serve verifyKey, INF-W4): a key
// that is not a minted local key is a placeholder whose grant spends under the
// hosted server's declared credential.
func placeholderKeys(_ context.Context, _ inference.Subject, key string) (Grant, string) {
	if key == "" || strings.HasPrefix(key, "oalk_") {
		return Grant{}, KeyUnknown
	}
	return Grant{Declared: true}, KeyVerified
}

func setupFoundingUse(t *testing.T) *foundingUse {
	t.Helper()
	f := &foundingUse{up: newDeepseekFixture(t), rule: "allow", asked: make(chan string, 8), answers: make(chan string, 8), wait: 5 * time.Second, keyCheck: placeholderKeys}
	return f.open(t)
}

func (f *foundingUse) open(t *testing.T) *foundingUse {
	t.Helper()
	const program = `C:\Program Files\Jan\Jan.exe`
	apply := func(_ context.Context, _ inference.Subject, _, name, _ string) (map[string]string, string) {
		if name != "deepseek" {
			return nil, "unknown"
		}
		return map[string]string{"Authorization": "Bearer " + deepseekSecret}, "applied"
	}
	r := router.New(router.NewHosted("deepseek", f.up.server.URL, router.WireOpenAICompatible, "deepseek"))
	r.UseCredentials(func(ctx context.Context, consumer, name, target string) (map[string]string, error) {
		headers, outcome := apply(ctx, inference.Subject{}, consumer, name, target)
		if outcome != "applied" {
			return nil, &router.CredentialRefusal{Outcome: outcome, Name: name}
		}
		return headers, nil
	})
	r.Survey()
	// The DeepSeek preset's budget is in tokens: its replies carry no cost.
	ceilings, err := inference.OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"),
		map[string]inference.Ceiling{"deepseek": {TokensPerDay: 100}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.ceilings = ceilings
	logf := func(err error) { f.mu.Lock(); fmt.Fprintln(&f.logs, err); f.mu.Unlock() }
	record := func(rec inference.Record) { f.mu.Lock(); f.records = append(f.records, rec); f.mu.Unlock() }
	provider, err := inference.New(inference.Config{Router: r, SurveyAge: time.Hour, Ceilings: ceilings, Record: record, OnError: logf, Apply: apply,
		Decide: func(ctx context.Context, s inference.Subject, action, resource string) (string, error) {
			if s.Program != program || action != inference.ActionComplete || resource != "host:deepseek" {
				return "not_granted", nil
			}
			f.mu.Lock()
			rule := f.rule
			f.mu.Unlock()
			switch rule {
			case "allow":
				return "permitted", nil
			case "block":
				return "denied", nil
			}
			// The refusal admits the first-spend question (the runtime's
			// decideAsking) and names it to the window.
			inference.AskedFrom(ctx).Add(firstSpendQuestion)
			select {
			case f.asked <- firstSpendQuestion:
			default:
			}
			return "not_granted", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Close() })
	subject := inference.Subject{Account: account(t), Program: program}
	window, err := Listen("127.0.0.1:0", Config{Chat: provider, Account: subject.Account, Record: record, OnError: logf,
		Bind: func(net.Conn, time.Time) (Bound, error) { return fixedBound{subject}, nil },
		Keys: f.keyCheck,
		// The runtime's listing rule for a client with no rules is tested
		// through its own decision point in serve's
		// TestFoundingUseThroughTheRuntimesDecisionPoint.
		Models: func(context.Context, inference.Subject) []string { return []string{deepseekModel} },
		// The presenter fixture answers the question: allow writes the rule
		// and the held call is admitted again.
		Await: func(ctx context.Context, _ inference.Subject, ids []string) bool {
			f.mu.Lock()
			f.awaited = append(f.awaited, ids...)
			f.mu.Unlock()
			select {
			case answer := <-f.answers:
				if answer != "allow" {
					return false
				}
				f.mu.Lock()
				f.rule = "allow"
				f.mu.Unlock()
				return true
			case <-ctx.Done():
				return false
			}
		},
		QuestionWait: f.wait})
	if err != nil {
		t.Fatal(err)
	}
	go window.Serve(context.Background())
	t.Cleanup(func() { window.Close() })
	f.base = "http://" + window.Addr().String() + "/v1"
	return f
}

// post sends what a desktop client sends: a bearer placeholder key and a JSON
// body. It returns the status, the raw response and the request bytes.
func (f *foundingUse) post(t *testing.T, body map[string]any) (int, string, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.base+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+placeholderKey)
	req.Header.Set("Content-Type", "application/json")
	var sent bytes.Buffer
	req.Write(&sent)
	req, _ = http.NewRequest(http.MethodPost, f.base+"/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+placeholderKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(answer), sent.String()
}

var weatherTool = []any{map[string]any{"type": "function", "function": map[string]any{"name": "get_weather",
	"description": "Weather for a city", "parameters": map[string]any{"type": "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}}, "required": []string{"city"}}}}}

func TestFoundingUseDeepseekReasonerWithToolsThroughTheGateway(t *testing.T) {
	f := setupFoundingUse(t)
	f.mu.Lock()
	f.rule = ""
	f.mu.Unlock()
	var exchanged []string

	// Turn 1, non-streaming: the client offers a tool. It holds no rule yet,
	// so its first spend admits the question; the window holds the call
	// until the presenter fixture answers allow, then admits it again
	// (INF-W10), and the client receives the tool call.
	go func() {
		<-f.asked
		f.answers <- "allow"
	}()
	first := []any{map[string]any{"role": "user", "content": "Weather in Riga?"}}
	status, answer, sent := f.post(t, map[string]any{"model": deepseekModel, "messages": first, "tools": weatherTool})
	exchanged = append(exchanged, answer, sent)
	if status != http.StatusOK {
		t.Fatalf("turn 1: %d %s", status, answer)
	}
	f.mu.Lock()
	awaited := strings.Join(f.awaited, ",")
	f.mu.Unlock()
	if awaited != firstSpendQuestion {
		t.Fatalf("the window waited on %q, want the first-spend question once", awaited)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content   *string          `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(answer), &completion); err != nil || len(completion.Choices) != 1 {
		t.Fatalf("turn 1 body %s", answer)
	}
	choice := completion.Choices[0]
	if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) != 1 || choice.Message.ToolCalls[0]["id"] != "call_00_riga" {
		t.Fatalf("turn 1 reply %s", answer)
	}
	if strings.Contains(answer, "get_weather for Riga") {
		t.Fatalf("reasoning reached the client as answer text: %s", answer)
	}

	// Turn 2, streaming: the client sends the call back without
	// reasoning_content, as plain clients do, plus the tool's result.
	second := append(first,
		map[string]any{"role": "assistant", "content": nil, "tool_calls": choice.Message.ToolCalls},
		map[string]any{"role": "tool", "tool_call_id": "call_00_riga", "content": `{"celsius":12}`})
	status, answer, sent = f.post(t, map[string]any{"model": deepseekModel, "messages": second, "tools": weatherTool,
		"stream": true, "stream_options": map[string]any{"include_usage": true}})
	exchanged = append(exchanged, answer, sent)
	if status != http.StatusOK {
		t.Fatalf("turn 2: %d %s", status, answer)
	}
	var text strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(answer))
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Error   any `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		json.Unmarshal([]byte(data), &chunk)
		if chunk.Error != nil {
			t.Fatalf("turn 2 stream error: %s", data)
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
	}
	if text.String() != "Riga is 12 C." {
		t.Fatalf("turn 2 text %q from %s", text.String(), answer)
	}
	f.up.mu.Lock()
	echoed := f.up.echoed
	f.up.mu.Unlock()
	if !echoed {
		t.Fatal("the fixture provider never saw the reasoning echoed")
	}

	// Spend: the replies carried 50 and 52 tokens and no cost, which passed
	// the 100-token day budget; the next call reads budget_exceeded before any
	// upstream request.
	if _, tokens, micros := f.ceilings.Spend("deepseek"); tokens != 102 || micros != 0 {
		t.Fatalf("spend today: %d tokens, %d micros", tokens, micros)
	}
	posts := f.up.posts()
	status, answer, sent = f.post(t, map[string]any{"model": deepseekModel, "messages": first, "tools": weatherTool})
	exchanged = append(exchanged, answer, sent)
	if status != http.StatusTooManyRequests || !strings.Contains(answer, "budget_exceeded") || !strings.Contains(answer, "tokens") {
		t.Fatalf("over the budget: %d %s", status, answer)
	}
	if f.up.posts() != posts {
		t.Fatal("a call over the budget reached the provider")
	}

	// Revoking the client's rule removes it: the next call reads not_granted,
	// the first-spend question is asked again and the call is held on it
	// (INF-W10); the person's refuse answers the call with its reason.
	f.mu.Lock()
	f.rule = ""
	f.awaited = nil
	f.mu.Unlock()
	go func() {
		<-f.asked
		f.answers <- "refuse"
	}()
	status, answer, sent = f.post(t, map[string]any{"model": deepseekModel, "messages": first})
	exchanged = append(exchanged, answer, sent)
	if status != http.StatusForbidden || !strings.Contains(answer, "not_permitted") || !strings.Contains(answer, "rights:not_granted") {
		t.Fatalf("revoked: %d %s", status, answer)
	}
	f.mu.Lock()
	awaited = strings.Join(f.awaited, ",")
	f.mu.Unlock()
	if awaited != firstSpendQuestion {
		t.Fatalf("after the revoke the window waited on %q, want the question asked again", awaited)
	}

	// Blocking the client refuses its next call at once with a reason,
	// asking nothing.
	f.mu.Lock()
	f.rule = "block"
	f.awaited = nil
	f.mu.Unlock()
	status, answer, sent = f.post(t, map[string]any{"model": deepseekModel, "messages": first})
	exchanged = append(exchanged, answer, sent)
	if status != http.StatusForbidden || !strings.Contains(answer, "not_permitted") || !strings.Contains(answer, "rights:denied") {
		t.Fatalf("blocked: %d %s", status, answer)
	}
	f.mu.Lock()
	awaited = strings.Join(f.awaited, ",")
	f.mu.Unlock()
	if awaited != "" {
		t.Fatalf("a blocked call was held on %q", awaited)
	}
	if f.up.posts() != posts {
		t.Fatal("a revoked or blocked call reached the provider")
	}

	// The key appears in no argv, environment, log, audit record or client
	// request or response; only the provider's Authorization header holds it.
	f.mu.Lock()
	records, _ := json.Marshal(f.records)
	logs := f.logs.String()
	f.mu.Unlock()
	places := map[string]string{
		"argv":        strings.Join(os.Args, "\x00"),
		"environment": strings.Join(os.Environ(), "\x00"),
		"logs":        logs,
		"audit":       string(records),
		"client":      strings.Join(exchanged, "\x00"),
	}
	f.up.mu.Lock()
	places["provider bodies"] = strings.Join(f.up.bodies, "\x00")
	for _, h := range f.up.headers {
		if h.Get("Authorization") != "Bearer "+deepseekSecret {
			t.Fatalf("provider header %q", h.Get("Authorization"))
		}
	}
	f.up.mu.Unlock()
	if len(records) < 4 {
		t.Fatalf("audit records %s", records)
	}
	// The placeholder key spent under the server's declared credential: the
	// completed calls and the budget refusal name it.
	f.mu.Lock()
	named := 0
	for _, r := range f.records {
		if r.Credential == "deepseek" && (r.Outcome == "completed" || r.Outcome == "budget_exceeded") {
			named++
		}
	}
	f.mu.Unlock()
	if named != 3 {
		t.Fatalf("%d records name the declared credential, want 3: %s", named, records)
	}
	for place, content := range places {
		if strings.Contains(content, deepseekSecret) {
			t.Fatalf("the key appears in %s", place)
		}
	}
}

// The window holds a call whose first spend admitted a question (INF-W10): a
// refuse answers it at once, an unanswered question until QuestionWait, and
// neither reaches the provider.
func TestFirstSpendQuestionHoldsTheCallUntilAnswered(t *testing.T) {
	first := []any{map[string]any{"role": "user", "content": "Weather in Riga?"}}

	refused := setupFoundingUse(t)
	refused.mu.Lock()
	refused.rule = ""
	refused.mu.Unlock()
	go func() {
		<-refused.asked
		refused.answers <- "refuse"
	}()
	status, answer, _ := refused.post(t, map[string]any{"model": deepseekModel, "messages": first})
	if status != http.StatusForbidden || !strings.Contains(answer, "rights:not_granted") {
		t.Fatalf("refused: %d %s", status, answer)
	}

	wait := 300 * time.Millisecond
	unanswered := (&foundingUse{up: newDeepseekFixture(t), asked: make(chan string, 8), answers: make(chan string, 8), wait: wait, keyCheck: placeholderKeys}).open(t)
	started := time.Now()
	status, answer, _ = unanswered.post(t, map[string]any{"model": deepseekModel, "messages": first, "stream": true})
	if status != http.StatusForbidden || !strings.Contains(answer, "rights:not_granted") {
		t.Fatalf("unanswered: %d %s", status, answer)
	}
	if held := time.Since(started); held < wait {
		t.Fatalf("an unanswered question held the call %v, want at least %v", held, wait)
	}
	if refused.up.posts() != 0 || unanswered.up.posts() != 0 {
		t.Fatal("a refused call reached the provider")
	}
}

// heldProvider refuses its first admission forbidden, naming one first-use
// question when ask is set, and answers every later admission unavailable
// with reason "second-admission".
type heldProvider struct {
	ask      bool
	attempts int
}

func (p *heldProvider) admit(ctx context.Context) wire.Admission {
	p.attempts++
	if p.attempts == 1 {
		if p.ask {
			inference.AskedFrom(ctx).Add("q-held")
		}
		return wire.Admission{Outcome: wire.StartOutcomeForbidden, Reason: "rights:not_granted"}
	}
	return wire.Admission{Outcome: wire.StartOutcomeUnavailable, Reason: "second-admission"}
}
func (p *heldProvider) Start(ctx context.Context, _ inference.Subject, _ wire.Request) wire.Admission {
	return p.admit(ctx)
}
func (*heldProvider) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*heldProvider) Cancel(inference.Subject, string) wire.Cancellation { return wire.Cancellation{} }
func (p *heldProvider) StartImage(ctx context.Context, _ inference.Subject, _ wire.ImageRequest) wire.Admission {
	return p.admit(ctx)
}
func (*heldProvider) ObserveImage(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*heldProvider) CancelImage(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{}
}
func (p *heldProvider) StartSpeech(ctx context.Context, _ inference.Subject, _ wire.SpeechRequest) wire.Admission {
	return p.admit(ctx)
}
func (*heldProvider) ObserveSpeech(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*heldProvider) CancelSpeech(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{}
}
func (p *heldProvider) StartTranscription(ctx context.Context, _ inference.Subject, _ wire.TranscriptionRequest) wire.Admission {
	return p.admit(ctx)
}
func (*heldProvider) ObserveTranscription(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*heldProvider) CancelTranscription(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{}
}
func (p *heldProvider) Embed(ctx context.Context, _ inference.Subject, _ wire.EmbedRequest) wire.Embeddings {
	a := p.admit(ctx)
	if a.Outcome == wire.StartOutcomeForbidden {
		return wire.Embeddings{Outcome: wire.EmbedOutcomeForbidden, Reason: a.Reason}
	}
	return wire.Embeddings{Outcome: wire.EmbedOutcomeUnavailable, Reason: a.Reason}
}
func (p *heldProvider) PreflightLive(ctx context.Context, _ inference.Subject, _ wire.LiveRequest) wire.Admission {
	return p.admit(ctx)
}
func (*heldProvider) StartLive(context.Context, inference.Subject, wire.LiveRequest) wire.Admission {
	return wire.Admission{}
}
func (*heldProvider) AppendLive(context.Context, inference.Subject, string, int64, []byte) wire.LiveInputResult {
	return wire.LiveInputResult{}
}
func (*heldProvider) ObserveLive(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*heldProvider) CommitLive(context.Context, inference.Subject, string) wire.LiveInputResult {
	return wire.LiveInputResult{}
}
func (*heldProvider) CancelLive(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{}
}

// rechecked is a binding whose peer moves while the call is held.
type rechecked struct {
	fixedBound
	moved *bool
}

func (b rechecked) Recheck() error {
	if *b.moved {
		return fmt.Errorf("%w: fixture", identity.ErrPeerMoved)
	}
	return nil
}

// Every admitting route holds a forbidden refusal through its first-use
// question (INF-W10): an Allow admits the call once more, a refusal without a
// question answers at once, and a connection that moved during the hold is
// refused for its binding before anything else is written (INF-W3).
func TestEveryRouteHoldsAQuestionedRefusalThroughTheWindow(t *testing.T) {
	subject := inference.Subject{Account: "account", Program: "/apps/held"}
	routes := []struct {
		name string
		call func(w *Window, rw http.ResponseWriter, b Bound)
	}{
		{"chat", func(w *Window, rw http.ResponseWriter, b Bound) {
			w.openAIChat(rw, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)), b, Grant{})
		}},
		{"image", func(w *Window, rw http.ResponseWriter, b Bound) {
			w.openAIImageGeneration(rw, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-1","prompt":"lighthouse","n":1,"size":"1024x1024","response_format":"b64_json"}`)), b, Grant{Credential: "openai"})
		}},
		{"speech", func(w *Window, rw http.ResponseWriter, b Bound) {
			w.openAISpeech(rw, httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","voice":"alloy","input":"hello","response_format":"wav"}`)), b, Grant{Credential: "openai"})
		}},
		{"transcription", func(w *Window, rw http.ResponseWriter, b Bound) {
			r, _ := multipartAudio(t)
			w.openAITranscription(rw, r, b, Grant{})
		}},
		{"embeddings", func(w *Window, rw http.ResponseWriter, b Bound) {
			w.openAIEmbeddings(rw, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"e","input":"hi"}`)), b, Grant{})
		}},
		{"realtime", func(w *Window, rw http.ResponseWriter, b Bound) {
			w.openAIRealtime(rw, httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil), b, Grant{Credential: "openai"})
		}},
	}
	cases := []struct {
		name              string
		ask, allow, moved bool
		awaits, tries     int
		reason            string
		status            int
	}{
		{name: "allowed", ask: true, allow: true, awaits: 1, tries: 2, reason: "second-admission", status: http.StatusServiceUnavailable},
		{name: "unquestioned", tries: 1, reason: "rights:not_granted", status: http.StatusForbidden},
		{name: "moved after allow", ask: true, allow: true, moved: true, awaits: 1, tries: 1, reason: "binding:peer_moved", status: http.StatusForbidden},
		{name: "moved after no allow", ask: true, moved: true, awaits: 1, tries: 1, reason: "binding:peer_moved", status: http.StatusForbidden},
	}
	for _, route := range routes {
		for _, c := range cases {
			t.Run(route.name+"/"+c.name, func(t *testing.T) {
				provider := &heldProvider{ask: c.ask}
				awaits, moved := 0, false
				w := &Window{cfg: Config{Chat: provider,
					StoreContent: func(context.Context, inference.Subject, string, []byte) inference.ContentOutcome {
						return inference.ContentResolved
					},
					Await: func(ctx context.Context, _ inference.Subject, ids []string) bool {
						awaits++
						moved = c.moved
						// A window with zero QuestionWait holds for 40 s.
						deadline, ok := ctx.Deadline()
						if left := time.Until(deadline); !ok || left > 40*time.Second || left < 39*time.Second {
							t.Errorf("held for %v (deadline %v), want 40s", left, ok)
						}
						if len(ids) != 1 || ids[0] != "q-held" {
							t.Errorf("awaited %v", ids)
						}
						return c.allow
					}}}
				rw := httptest.NewRecorder()
				route.call(w, rw, rechecked{fixedBound{subject}, &moved})
				if provider.attempts != c.tries || awaits != c.awaits {
					t.Fatalf("attempts %d awaits %d, want %d and %d", provider.attempts, awaits, c.tries, c.awaits)
				}
				var body map[string]any
				if rw.Code != c.status || json.Unmarshal(rw.Body.Bytes(), &body) != nil || !strings.Contains(rw.Body.String(), c.reason) {
					t.Fatalf("status %d body %q, want one refusal %d naming %q", rw.Code, rw.Body.String(), c.status, c.reason)
				}
				if route.name == "realtime" && rw.Header().Get("Upgrade") != "" {
					t.Fatal("a refused realtime call was upgraded")
				}
			})
		}
	}
}

// An upload the store refuses for want of its input profile holds the call
// for the input question the store raised, inside the same hold as the start
// (INF-W10): an Allow stores the upload and starts the request once; no
// answer refuses content:write:forbidden naming the question still pending
// (INF-W11) and starts nothing.
func TestUploadStoreHoldsForItsInputQuestion(t *testing.T) {
	subject := inference.Subject{Account: "account", Program: "/apps/uploader"}
	type uploadRoute struct {
		name   string
		chat   Chat
		call   func(w *Window, rw http.ResponseWriter)
		starts func() int
	}
	image := &fakeImage{}
	transcriber := &fakeTranscriber{}
	routes := []uploadRoute{{"image edit", image, func(w *Window, rw http.ResponseWriter) {
		*image = fakeImage{}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for name, value := range map[string]string{"model": "gpt-image-1", "prompt": "a hat"} {
			_ = mw.WriteField(name, value)
		}
		part, _ := mw.CreateFormFile("image", "input.png")
		_, _ = part.Write([]byte("\x89PNG\r\n\x1a\ninput"))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w.openAIImageEdit(rw, r, fixedBound{subject}, Grant{})
	}, func() int {
		if image.request.Mode == wire.ImageModeEdit {
			return 1
		}
		return 0
	}}, {"transcription", transcriber, func(w *Window, rw http.ResponseWriter) {
		transcriber.starts = 0
		r, _ := multipartAudio(t)
		w.openAITranscription(rw, r, fixedBound{subject}, Grant{})
	}, func() int { return transcriber.starts }}}
	for _, route := range routes {
		for _, allow := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allow=%v", route.name, allow), func(t *testing.T) {
				stores, awaits := 0, 0
				granted := false
				w := &Window{cfg: Config{Chat: route.chat,
					StoreContent: func(ctx context.Context, got inference.Subject, _ string, _ []byte) inference.ContentOutcome {
						stores++
						if got != subject {
							t.Errorf("stored for %+v", got)
						}
						if !granted {
							inference.AskedFrom(ctx).Add("q-input")
							return inference.ContentForbidden
						}
						return inference.ContentResolved
					},
					ReadContent: func(context.Context, inference.Subject, string, int64) ([]byte, inference.ContentOutcome) {
						return []byte("\x89PNG\r\n\x1a\nout"), inference.ContentResolved
					},
					Await: func(_ context.Context, _ inference.Subject, ids []string) bool {
						awaits++
						if len(ids) != 1 || ids[0] != "q-input" {
							t.Errorf("held on %v", ids)
						}
						granted = allow
						return allow
					},
					Pending: func(_ context.Context, _ inference.Subject, ids []string) []string { return ids },
				}}
				rw := httptest.NewRecorder()
				route.call(w, rw)
				if awaits != 1 {
					t.Fatalf("held %d times, want once", awaits)
				}
				if allow {
					if rw.Code == http.StatusForbidden || stores != 2 || route.starts() != 1 {
						t.Fatalf("after Allow: status %d stores %d starts %d body %s", rw.Code, stores, route.starts(), rw.Body.String())
					}
					return
				}
				if rw.Code != http.StatusForbidden || stores != 1 || route.starts() != 0 ||
					!strings.Contains(rw.Body.String(), "forbidden: content:write:forbidden rights:question_pending:q-input") {
					t.Fatalf("unanswered: status %d stores %d starts %d body %s", rw.Code, stores, route.starts(), rw.Body.String())
				}
			})
		}
	}
}

// A key in the minted spelling is verified as a local key and never read as a
// placeholder; an empty key is refused before the body is read (INF-W4).
func TestPlaceholderKeyIsAnyNonEmptyKeyButAMintedOne(t *testing.T) {
	f := setupFoundingUse(t)
	for _, key := range []string{"", "oalk_damaged"} {
		req, _ := http.NewRequest(http.MethodPost, f.base+"/chat/completions", strings.NewReader(`{"model":"deepseek-reasoner","messages":[{"role":"user","content":"hi"}]}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: %d", key, resp.StatusCode)
		}
	}
	if f.up.posts() != 0 {
		t.Fatal("a refused key reached the provider")
	}
}
