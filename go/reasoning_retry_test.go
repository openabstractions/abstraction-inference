package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// localThinker is a model server on this machine, listed on Ollama's routes,
// whose model spends any output limit on reasoning and answers "Paris" when
// asked for reasoning_effort none. refuseNone answers that second ask 400.
type localThinker struct {
	mu         sync.Mutex
	asks       []map[string]any
	refuseNone bool
}

func (l *localThinker) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/tags", "/api/ps":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"models":[{"name":"thinker"}]}`)
		return
	case "/v1/chat/completions":
	default:
		http.NotFound(w, r)
		return
	}
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	l.mu.Lock()
	l.asks = append(l.asks, body)
	refuse := l.refuseNone
	l.mu.Unlock()
	none := body["reasoning_effort"] == "none"
	if none && refuse {
		http.Error(w, `{"error":"unknown reasoning_effort"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if none {
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Paris\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
		return
	}
	limit, _ := body["max_tokens"].(float64)
	for range int(limit) {
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"hmm \"}}]}\n\n")
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n")
	fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":%d}}\n\ndata: [DONE]\n\n", int(limit))
}

func (l *localThinker) taken() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.asks...)
}

func localThinkerProvider(t *testing.T, thinker *localThinker) *Provider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(thinker.serve))
	t.Cleanup(server.Close)
	host := router.Ollama(server.URL)
	return setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		r := router.New(host)
		r.Survey()
		cfg.Router = r
		cfg.Decide = func(context.Context, Subject, string, string) (string, error) { return "permitted", nil }
	}).provider
}

// R34 owner machine, 2026-11-12: qwen3.6-35b-a3b at max_output 10 ended
// chat@3 upstream:reasoning_only with the model's cut thought as the only
// block. A local server is asked once more with reasoning_effort none; the
// first attempt's reasoning block stays, the answer follows under a new
// block, and the usage counts both attempts. A server that refuses the
// second ask ends the reply failed under that refusal's cause.
func TestChat3ReasoningOnlyLocalReplyIsAskedAgainWithoutReasoning(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		thinker := &localThinker{refuseNone: refuse}
		p := localThinkerProvider(t, thinker)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{p, caller, ctx}})
		window, err := client.GetHistoryWindow()
		if err != nil || window.Window == nil {
			cancel()
			t.Fatalf("history: %+v %v", window, err)
		}
		req := chat3RequestFixture()
		req.Identity.HistoryEpoch = window.Window.HistoryEpoch
		req.Target.Model = "thinker"
		limit := int64(10)
		req.Content.Options = &wire.ChatOptions{MaxOutput: limit}
		a, err := client.Start(req)
		if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
			cancel()
			t.Fatalf("admission %v %v", a.Outcome, err)
		}
		var end *wire.ChatResult
		var cursor int64
		var text strings.Builder
		blocks := map[int64]wire.ChatPartKind{}
		ended := map[int64]bool{}
		for end == nil && ctx.Err() == nil {
			page, err := client.Observe(a.Request, cursor, 256, 65536, 100)
			if err != nil || page.Outcome != wire.PageOutcomePage {
				cancel()
				t.Fatalf("observe %v %v", page.Outcome, err)
			}
			for _, event := range page.Events {
				switch {
				case event.End != nil:
					end = event.End
				case event.Part != nil:
					if kind, seen := blocks[event.Part.Block]; seen && kind != event.Part.Kind || ended[event.Part.Block] {
						t.Fatalf("block %d reused: %s after %s", event.Part.Block, event.Part.Kind, kind)
					}
					blocks[event.Part.Block] = event.Part.Kind
					if event.Part.Kind == wire.ChatPartKindText {
						text.WriteString(event.Part.Text)
					}
				case event.BlockEnd != nil:
					ended[event.BlockEnd.Block] = true
				}
			}
			cursor = page.Next
		}
		cancel()
		if end == nil {
			t.Fatal("terminal missing")
		}
		asks := thinker.taken()
		if len(asks) != 2 || asks[0]["reasoning_effort"] != nil || asks[1]["reasoning_effort"] != "none" || asks[1]["max_tokens"] != float64(limit) {
			t.Fatalf("refuse=%v upstream asks %v", refuse, asks)
		}
		if refuse {
			if end.Outcome != wire.ChatResultOutcomeFailed || !strings.HasPrefix(end.Cause, "upstream:") || end.Cause == ReasonReasoningOnly || text.Len() != 0 {
				t.Fatalf("refused second ask: %s/%s %q", end.Outcome, end.Cause, text.String())
			}
			continue
		}
		if end.Outcome != wire.ChatResultOutcomeCompleted || text.String() != "Paris" || blocks[0] != wire.ChatPartKindReasoning || blocks[1] != wire.ChatPartKindText {
			t.Fatalf("answer: %s/%s %q blocks %v", end.Outcome, end.Cause, text.String(), blocks)
		}
		if end.Usage.Output == nil || *end.Usage.Output != limit+1 || end.Usage.Input == nil || *end.Usage.Input != 24 {
			t.Fatalf("usage of both attempts: %+v", end.Usage)
		}
	}
}

// A hosted server bills each attempt; its reasoning-only reply ends INF-Q8
// after one ask (TestChat3ReasoningOnlyTerminalUsesQ8AndPreservesUsage
// counts the posts), and a caller's own reasoning effort is kept.
func TestReasoningRetryAsksOnlyLocalServersWithoutCallerEffort(t *testing.T) {
	local, hosted := &router.Host{Name: "ollama"}, router.NewHosted("openrouter", "http://127.0.0.1:1", router.WireOpenAICompatible, "openrouter")
	if _, ok := withoutReasoning(hosted, wire.Request{}); ok {
		t.Fatal("hosted server asked again")
	}
	if _, ok := withoutReasoning(local, wire.Request{Extensions: map[string]string{reasoningEffortExtension: "high"}}); ok {
		t.Fatal("caller's reasoning effort replaced")
	}
	caller := map[string]string{"openai/user": "u"}
	again, ok := withoutReasoning(local, wire.Request{Extensions: caller})
	if !ok || again.Extensions[reasoningEffortExtension] != "none" || again.Extensions["openai/user"] != "u" || len(caller) != 1 {
		t.Fatalf("again %+v, caller's map %v", again.Extensions, caller)
	}
}
