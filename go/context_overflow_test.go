package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// overflowSecret stands for prompt text, paths or keys an upstream may echo
// in an error body. No reply, delta, audit record or reported error may
// carry it.
const overflowSecret = "C:/Users/private/project/AGENTS.md sk-live-ECHOED"

func assertNoOverflowDetail(t *testing.T, f *fixture, values ...any) {
	t.Helper()
	evidence, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	evidence = append(evidence, f.errs.Bytes()...)
	f.mu.Unlock()
	for _, leaked := range []string{overflowSecret, "exceeds the available context", "maximum context length", "prompt is too long", "longer than the context length"} {
		if strings.Contains(string(evidence), leaked) {
			t.Fatalf("upstream detail %q leaked: %s", leaked, evidence)
		}
	}
}

func TestHTTPContextOverflowIsRefusedWithOnlyItsNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body, reason string
		status                   int
		// observed serves the request from a local Lemonade fixture whose
		// loaded context the router observed at 4096 tokens.
		observed bool
	}{
		{name: "ollama wording without a code uses the observed window", observed: true, status: 400, reason: "upstream:context_length_exceeded:4096:0",
			body: `{"error":{"message":"the prompt is longer than the context length currently available to the model; shorten the prompt, adjust the context length in settings, or use a model with a longer context length ` + overflowSecret + `","type":"invalid_request_error","param":null,"code":null}}`},
		{name: "context mentioned outside an overflow phrase keeps its status", kind: router.WireOpenAICompatible, status: 400, reason: "upstream:400",
			body: `{"error":{"message":"invalid context parameter: context must be an object ` + overflowSecret + `","type":"invalid_request_error","code":null}}`},
		{name: "lemonade wrapped llama.cpp", kind: router.WireOpenAICompatible, status: 400, reason: "upstream:context_length_exceeded:4096:8219",
			body: `{"error":{"message":"the request exceeds the available context size, try increasing it ` + overflowSecret + `","type":"invalid_request_error","code":"context_length_exceeded","status_code":400,` +
				`"details":{"backend":"llamacpp","response":{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":8219,"n_ctx":4096}}}}}`},
		{name: "openai compatible message", kind: router.WireOpenAICompatible, status: 400, reason: "upstream:context_length_exceeded:8192:9000",
			body: `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens. ` + overflowSecret + `","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`},
		{name: "anthropic messages", kind: router.WireAnthropicMessages, status: 400, reason: "upstream:context_length_exceeded:200000:210000",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum ` + overflowSecret + `"}}`},
		{name: "code without numbers", kind: router.WireOpenAICompatible, status: 400, reason: "upstream:context_length_exceeded:0:0",
			body: `{"error":{"message":"` + overflowSecret + `","code":"context_length_exceeded"}}`},
		{name: "other refusal keeps its status", kind: router.WireOpenAICompatible, status: 400, reason: "upstream:400",
			body: `{"error":{"message":"invalid tool schema ` + overflowSecret + `","type":"invalid_request_error"}}`},
		{name: "rate limit is never overflow", kind: router.WireOpenAICompatible, status: 429, reason: "upstream:429",
			body: `{"error":{"message":"rate limit: too many tokens per minute ` + overflowSecret + `"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *fixture
			var req wire.Request
			if tc.observed {
				f = lemonadeProvider(t, newLemonadeEngine(t, 4096, func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				}))
				req = lemonadeRequest()
			} else {
				f = setup(t, tc.kind, nil)
				f.up.chatStatus, f.up.chatBody = tc.status, tc.body
				name, model := "openrouter", "anthropic/claude-sonnet-5"
				if tc.kind == router.WireAnthropicMessages {
					name, model = "anthropic", "claude-sonnet-5"
				}
				req = hostedRequest(name)
				req.Model = model
			}
			a := f.provider.Start(context.Background(), caller, req)
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			_, reply, deltas := drain(t, f.provider, caller, a.Operation)
			want := wire.ReplyOutcomeRefused
			if tc.status == 429 {
				want = wire.ReplyOutcomeUnavailable
			}
			if reply.Outcome != want || reply.Reason != tc.reason || reply.StopReason != wire.StopReasonNoStop {
				t.Fatalf("reply %s/%s, want %s/%s", reply.Outcome, reply.Reason, want, tc.reason)
			}
			records := f.waitLogged(t, 1)
			if records[0].Reason != tc.reason {
				t.Fatalf("audit reason %q, want %q", records[0].Reason, tc.reason)
			}
			assertNoOverflowDetail(t, f, reply, deltas, records)
		})
	}
}

func TestStreamContextOverflowIsRefusedAndEngineDeathStaysUnavailable(t *testing.T) {
	llamaError := `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it ` + overflowSecret + `","type":"exceed_context_size_error","n_prompt_tokens":8219,"n_ctx":4096}}`
	for _, tc := range []struct {
		name            string
		kind            string
		chunks          []string
		streamError     string
		withDone, drop  bool
		outcome         wire.ReplyOutcome
		reason, partial string
	}{
		{name: "overflow then EOF", kind: router.WireOpenAICompatible, streamError: llamaError, outcome: wire.ReplyOutcomeRefused, reason: "upstream:context_length_exceeded:4096:8219"},
		{name: "overflow then DONE", kind: router.WireOpenAICompatible, streamError: llamaError, withDone: true, outcome: wire.ReplyOutcomeRefused, reason: "upstream:context_length_exceeded:4096:8219"},
		{name: "overflow after partial output", kind: router.WireOpenAICompatible, chunks: []string{"partial"}, streamError: llamaError,
			outcome: wire.ReplyOutcomeRefused, reason: "upstream:context_length_exceeded:4096:8219", partial: "partial"},
		{name: "anthropic overflow event", kind: router.WireAnthropicMessages,
			streamError: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum ` + overflowSecret + `"}}`,
			outcome:     wire.ReplyOutcomeRefused, reason: "upstream:context_length_exceeded:200000:210000"},
		{name: "engine exits mid-reply", kind: router.WireOpenAICompatible, chunks: []string{"partial"}, drop: true,
			outcome: wire.ReplyOutcomeUnavailable, reason: "upstream:stream", partial: "partial"},
		{name: "other stream error", kind: router.WireOpenAICompatible,
			streamError: `{"error":{"message":"backend crashed ` + overflowSecret + `","type":"server_error"}}`,
			outcome:     wire.ReplyOutcomeUnavailable, reason: "upstream:error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, tc.kind, nil)
			f.up.chunks = tc.chunks
			f.up.streamError, f.up.streamErrorDone, f.up.streamDrop = tc.streamError, tc.withDone, tc.drop
			name, model := "openrouter", "anthropic/claude-sonnet-5"
			if tc.kind == router.WireAnthropicMessages {
				name, model = "anthropic", "claude-sonnet-5"
			}
			req := hostedRequest(name)
			req.Model = model
			a := f.provider.Start(context.Background(), caller, req)
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			text, reply, deltas := drain(t, f.provider, caller, a.Operation)
			if reply.Outcome != tc.outcome || reply.Reason != tc.reason || reply.StopReason != wire.StopReasonNoStop {
				t.Fatalf("reply %s/%s, want %s/%s", reply.Outcome, reply.Reason, tc.outcome, tc.reason)
			}
			if text != tc.partial {
				t.Fatalf("partial text %q, want %q", text, tc.partial)
			}
			records := f.waitLogged(t, 1)
			if records[0].Outcome != tc.outcome.String() || records[0].Reason != tc.reason {
				t.Fatalf("audit %+v", records[0])
			}
			assertNoOverflowDetail(t, f, reply, deltas, records)
		})
	}
}

// lemonadeEngine is a local Lemonade fixture with one downloaded model whose
// loaded context is ctx; a ctx of 0 holds the model downloaded and unloaded.
// chat answers every chat request.
type lemonadeEngine struct {
	server *httptest.Server
	ctx    atomic.Int64
	chat   func(w http.ResponseWriter)
}

func newLemonadeEngine(t *testing.T, ctx int64, chat func(w http.ResponseWriter)) *lemonadeEngine {
	e := &lemonadeEngine{chat: chat}
	e.ctx.Store(ctx)
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//unchecked: fixture drains the request body; the handler answers from fixed data
		io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/models":
			io.WriteString(w, `{"data":[{"id":"Qwen3-0.6B-GGUF","downloaded":true,"max_context_window":40960,"context_length":40960,"labels":[]}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health" && e.ctx.Load() == 0:
			io.WriteString(w, `{"all_models_loaded":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/health":
			fmt.Fprintf(w, `{"model_loaded":"Qwen3-0.6B-GGUF","all_models_loaded":[{"model_name":"Qwen3-0.6B-GGUF","loaded":true,"recipe_options":{"ctx_size":%d}}]}`, e.ctx.Load())
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chat/completions":
			e.chat(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.server.Close)
	return e
}

func lemonadeProvider(t *testing.T, e *lemonadeEngine) *fixture {
	t.Helper()
	return lemonadeProviderOn(t, []*router.Host{router.Lemonade(e.server.URL)}, "lemonade")
}

// lemonadeProviderOn is a provider over hosts that permits the caller to
// complete on the permitted hosts alone.
func lemonadeProviderOn(t *testing.T, hosts []*router.Host, permitted ...string) *fixture {
	t.Helper()
	f := &fixture{policy: &policy{rules: map[string]string{}}, applier: &applier{}, recorded: make(chan struct{}, 64)}
	r := router.New(hosts...)
	r.Survey()
	for _, host := range permitted {
		f.policy.rules[caller.Program+" "+ActionComplete+" host:"+host] = "permitted"
	}
	p, err := New(Config{Router: r, Decide: f.policy.decide, Apply: f.applier.apply, SurveyAge: time.Hour, Idle: 2 * time.Second,
		Record: func(rec Record) {
			f.mu.Lock()
			f.records = append(f.records, rec)
			f.mu.Unlock()
			select {
			case f.recorded <- struct{}{}:
			default:
			}
		},
		OnError: func(err error) { f.mu.Lock(); fmt.Fprintln(&f.errs, err); f.mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	p.surveyed = time.Now()
	t.Cleanup(func() { p.Close() })
	f.provider = p
	return f
}

func lemonadeRequest() wire.Request {
	return wire.Request{Model: "Qwen3-0.6B-GGUF",
		Messages:   []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: overflowSecret}}}},
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
}

// The first request of a session has no usage evidence. When the engine's
// refusal names no window, the reply carries the loaded context the router
// observed for the model server and model that refused it.
func TestFirstRequestOverflowReportsTheObservedLoadedContext(t *testing.T) {
	e := newLemonadeEngine(t, 4096, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"the request exceeds the available context size, try increasing it `+overflowSecret+`","type":"invalid_request_error","code":"context_length_exceeded","status_code":400}}`)
	})
	f := lemonadeProvider(t, e)
	a := f.provider.Start(context.Background(), caller, lemonadeRequest())
	if a.Outcome != wire.StartOutcomeAccepted || a.Host != "lemonade" {
		t.Fatalf("admission %+v", a)
	}
	_, reply, deltas := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != "upstream:context_length_exceeded:4096:0" {
		t.Fatalf("reply %s/%s", reply.Outcome, reply.Reason)
	}
	assertNoOverflowDetail(t, f, reply, deltas, f.waitLogged(t, 1))
}

// Two model servers serve the same model name with different contexts. A
// refusal that names no window carries the refusing server's own observed
// window, and the request reaches the server that holds the model.
func TestOverflowNamesTheRefusingServersWindow(t *testing.T) {
	refuse := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"the request exceeds the available context size","code":"context_length_exceeded"}}`)
	}
	for _, tc := range []struct {
		name          string
		first, second int64
		refusing      string
		window        string
	}{
		{"both loaded, first serves", 8192, 4096, "lemonade", "8192"},
		{"second alone loaded", 0, 4096, "lemonade-b", "4096"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := newLemonadeEngine(t, tc.first, refuse), newLemonadeEngine(t, tc.second, refuse)
			other := router.Lemonade(second.server.URL)
			other.Name = "lemonade-b"
			f := lemonadeProviderOn(t, []*router.Host{router.Lemonade(first.server.URL), other}, "lemonade", "lemonade-b")
			a := f.provider.Start(context.Background(), caller, lemonadeRequest())
			if a.Outcome != wire.StartOutcomeAccepted || a.Host != tc.refusing {
				t.Fatalf("admission %+v, want accepted on %s", a, tc.refusing)
			}
			_, reply, _ := drain(t, f.provider, caller, a.Operation)
			if want := "upstream:context_length_exceeded:" + tc.window + ":0"; reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != want {
				t.Fatalf("reply %s/%s, want refused/%s", reply.Outcome, reply.Reason, want)
			}
		})
	}
}

// A loaded context can change after the router's last survey. The window the
// refusing engine states outranks the older observation.
func TestTheEnginesStatedWindowOutranksAStaleObservation(t *testing.T) {
	e := newLemonadeEngine(t, 8192, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error","n_prompt_tokens":8219,"n_ctx":4096}}`+"\n\n")
	})
	f := lemonadeProvider(t, e)
	a := f.provider.Start(context.Background(), caller, lemonadeRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != "upstream:context_length_exceeded:4096:8219" {
		t.Fatalf("reply %s/%s", reply.Outcome, reply.Reason)
	}
}

func TestContextOverflowReadsOnlyBoundedNumbers(t *testing.T) {
	for _, tc := range []struct {
		body          string
		window, input int64
		ok            bool
	}{
		{body: `{"error":{"type":"exceed_context_size_error","n_ctx":4096,"n_prompt_tokens":8219}}`, window: 4096, input: 8219, ok: true},
		{body: `{"error":{"code":"context_length_exceeded","n_ctx":-1,"n_prompt_tokens":"8219"}}`, ok: true},
		{body: `{"error":{"code":"context_length_exceeded","n_ctx":1e30}}`, ok: true},
		{body: `{"error":{"message":"request (8,219 tokens) exceeds the available context size (4,096 tokens), try increasing it"}}`, window: 4096, input: 8219, ok: true},
		{body: `{"error":{"message":"The prompt has 5000 tokens, but the configured context size is 4096 tokens"}}`, window: 4096, input: 5000, ok: true},
		{body: `{"error":"Input (5000 tokens) is longer than the model's context length (4096 tokens)."}`, window: 4096, input: 5000, ok: true},
		{body: `{"error":{"message":"the prompt is longer than the context length currently available to the model; shorten the prompt, adjust the context length in settings, or use a model with a longer context length","type":"invalid_request_error","param":null,"code":null}}`, ok: true},
		{body: `{"error":{"message":"the input length exceeds the context length","type":"invalid_request_error","param":null,"code":null}}`, ok: true},
		{body: `{"error":{"message":"invalid context parameter: context must be an object","type":"invalid_request_error"}}`},
		{body: `{"error":{"message":"rate limit exceeded: maximum context length is 4096 tokens"}}`},
		{body: `{"error":{"message":"server error","code":500}}`},
		{body: `not json maximum context length is 4096 tokens`},
		{body: ``},
	} {
		window, input, ok := contextOverflow([]byte(tc.body))
		if window != tc.window || input != tc.input || ok != tc.ok {
			t.Errorf("%s: got %d/%d/%v, want %d/%d/%v", tc.body, window, input, ok, tc.window, tc.input, tc.ok)
		}
	}
}
