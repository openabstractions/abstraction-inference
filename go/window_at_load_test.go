package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// fakeOllama is an Ollama that loads its one model with a 4096-token window
// on a prompt-less generate request or on the first chat request, and lists
// that window in /api/ps once loaded. Like Ollama, it would serve a longer
// prompt by cutting it; the test fails if such a prompt reaches it.
type fakeOllama struct {
	server *httptest.Server
	loaded atomic.Bool
	loads  atomic.Int64
	mu     sync.Mutex
	chats  []string
}

const ollamaWindowModel = "qwen3:8b"

func newFakeOllama(t *testing.T) *fakeOllama {
	f := &fakeOllama{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"models":[{"name":%q}]}`, ollamaWindowModel)
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) {
		if !f.loaded.Load() {
			w.Write([]byte(`{"models":[]}`))
			return
		}
		fmt.Fprintf(w, `{"models":[{"name":%q,"model":%q,"context_length":4096}]}`, ollamaWindowModel, ollamaWindowModel)
	})
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"capabilities":["completion","tools"],"model_info":{"general.architecture":"qwen3","qwen3.context_length":40960},"parameters":"temperature 0.6"}`))
	})
	mux.HandleFunc("/api/generate", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		f.loads.Add(1)
		f.loaded.Store(true)
		fmt.Fprintf(w, `{"model":%q,"done":true,"done_reason":"load"}`, ollamaWindowModel)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.chats = append(f.chats, string(raw))
		f.mu.Unlock()
		f.loaded.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOllama) chatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chats)
}

// An agent's system prompt longer than the window Ollama loads its model with
// is refused plainly as a context overflow naming that window, before Ollama
// can cut it silently: the model is loaded as the request would load it, the
// loaded window is read from /api/ps, and the prompt never reaches Ollama. A
// prompt within the window is served (INF-Q7, router ROUTE-D8).
func TestAPromptOverOllamasLoadedWindowIsRefusedNotCut(t *testing.T) {
	ollama := newFakeOllama(t)
	r := router.New(router.Ollama(ollama.server.URL))
	r.Survey()
	p, err := New(Config{Router: r, SurveyAge: time.Hour,
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply:  func(context.Context, Subject, string, string, string) (map[string]string, string) { return nil, "unknown" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ask := func(system string) wire.Reply {
		t.Helper()
		req := wire.Request{Model: ollamaWindowModel, Messages: []wire.Message{
			{Role: wire.RoleSystem, Parts: []wire.Part{{Kind: wire.PartKindText, Text: system}}},
			{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "Edit main.go."}}}},
			Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
		a := p.Start(context.Background(), caller, req)
		if a.Outcome != wire.StartOutcomeAccepted {
			t.Fatalf("admission %+v", a)
		}
		_, end, _ := drain(t, p, caller, a.Operation)
		return end
	}
	system := strings.Repeat("You are a coding agent. Follow the repository rules exactly. ", 700)
	end := ask(system)
	want := fmt.Sprintf("upstream:context_length_exceeded:4096:%d", promptTokensAtLeast(wire.Request{Messages: []wire.Message{
		{Parts: []wire.Part{{Text: system}}}, {Parts: []wire.Part{{Text: "Edit main.go."}}}}}))
	if end.Outcome != wire.ReplyOutcomeRefused || end.Reason != want {
		t.Fatalf("long prompt: %+v, want reason %s", end, want)
	}
	if ollama.chatCount() != 0 || ollama.loads.Load() != 1 {
		t.Fatalf("chats %d loads %d", ollama.chatCount(), ollama.loads.Load())
	}
	if end = ask("You are a coding agent."); end.Outcome != wire.ReplyOutcomeCompleted || ollama.chatCount() != 1 {
		t.Fatalf("short prompt: %+v chats %d", end, ollama.chatCount())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(ollama.chats[0]), &sent); err != nil || sent["model"] != ollamaWindowModel {
		t.Fatalf("sent %s", ollama.chats[0])
	}
}
