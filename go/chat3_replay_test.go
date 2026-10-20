package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3OpaqueReplaySilentOriginEndsStalled(t *testing.T) {
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	var posts atomic.Int32
	opened, cancelled := make(chan struct{}), make(chan struct{})
	seal := []byte(`{ "type": "thinking", "thinking": "prior", "signature": "opaque" }`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5"}]}`)
			return
		}
		posts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Contains(body, seal) {
			t.Errorf("opaque replay bytes %s: %v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(opened)
		<-r.Context().Done()
		close(cancelled)
	}))
	t.Cleanup(server.Close)
	f := setup(t, router.WireAnthropicMessages, func(cfg *Config) {
		cfg.Now, cfg.Idle = now, time.Hour
		cfg.Router = router.New(router.NewHosted("anthropic", server.URL, router.WireAnthropicMessages, "anthropic"))
		cfg.Router.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
		cfg.Router.Survey()
	})
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	request := chat3HostedRequest(t, client)
	request.Content.Credential = "anthropic"
	request.Content.Messages = append([]wire.ChatMessage{{Role: wire.RoleAssistant,
		Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "previous answer"}}, Reasoning: []wire.ChatReplay{{
			Seal: wire.ReasoningSeal{Origin: router.WireAnthropicMessages, Bytes: seal},
		}}}}, request.Content.Messages...)
	a, err := client.Start(request)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start %+v %v", a, err)
	}
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("opaque replay did not open its SSE stream")
	}
	mu.Lock()
	clock = clock.Add(180 * time.Second)
	mu.Unlock()
	select {
	case <-f.provider.visible(caller, a.Request).finished:
	case <-time.After(3 * time.Second):
		t.Fatal("silent opaque replay did not stop at the origin stall bound")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled replay did not cancel its upstream")
	}
	page, err := client.Observe(a.Request, 0, 256, 65536, 0)
	if err != nil || !page.AtEnd || len(page.Events) == 0 {
		t.Fatalf("terminal %+v %v", page, err)
	}
	end := page.Events[len(page.Events)-1].End
	if end == nil || end.Outcome != wire.ChatResultOutcomeFailed || end.Cause != "stalled" || posts.Load() != 1 {
		t.Fatalf("terminal %+v posts %d", end, posts.Load())
	}
}

func TestChat3ReplayActualOriginatingRequestBytes(t *testing.T) {
	for _, origin := range []string{router.WireOpenAICompatible, router.WireAnthropicMessages} {
		t.Run(origin, func(t *testing.T) {
			seal := []byte("trace\nwith <opaque> bytes +/= and 雪")
			if origin == router.WireAnthropicMessages {
				// Deliberately retain lexical whitespace and escapes. The replay
				// encoder must preserve the complete original opaque block.
				seal = []byte("{ \"type\" : \"thinking\", \"thinking\":\"trace\\n\", \"signature\" : \"opaque+/=\\u003c雪\" }")
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request struct {
					Messages []struct {
						Role      string
						Content   json.RawMessage
						Reasoning string `json:"reasoning_content"`
					}
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					return
				}
				if len(request.Messages) != 2 || request.Messages[0].Role != "assistant" {
					t.Errorf("messages %s", body)
					return
				}
				if origin == router.WireOpenAICompatible {
					if request.Messages[0].Reasoning != string(seal) {
						t.Errorf("reasoning bytes %q", request.Messages[0].Reasoning)
					}
				} else if !bytes.Contains(body, seal) {
					t.Errorf("opaque block changed: %s", body)
				}
				if bytes.Contains(request.Messages[0].Content, []byte("display-only reasoning")) {
					t.Errorf("public reasoning in content: %s", body)
				}
				if origin == router.WireOpenAICompatible {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"answer\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer server.Close()
			messages := []wire.ChatMessage{{Role: wire.RoleAssistant, Reasoning: []wire.ChatReplay{{Block: 4, Seal: wire.ReasoningSeal{Origin: origin, Bytes: seal}}}}, {Role: wire.RoleUser}}
			ctx := withChat3Replay(context.Background(), messages, func(a chat3ReplayAudit) { t.Errorf("unexpected audit %+v", a) })
			req := wire.Request{Messages: []wire.Message{{Role: wire.RoleAssistant, Parts: []wire.Part{{Kind: wire.PartKindToolCall, CallID: "call", Name: "tool", Arguments: "{}"}}}, {Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "continue"}}}}}
			var answer string
			sink := chat3Sink{Part: func(p wire.ChatPart) error {
				if p.Kind == wire.ChatPartKindText {
					answer += p.Text
				}
				return nil
			}}
			host := &router.Host{Base: server.URL, Chat: "/chat"}
			var result chat3UpstreamResult
			if origin == router.WireOpenAICompatible {
				result = runChat3OpenAI(ctx, server.Client(), host, "m", req, nil, nil, nil, nil, sink)
			} else {
				result = runChat3Anthropic(ctx, server.Client(), host, "m", req, nil, nil, nil, nil, sink)
			}
			if result.Err != nil || result.Cause != "" || answer != "answer" || calls.Load() != 1 {
				t.Fatalf("result %+v answer %q calls %d", result, answer, calls.Load())
			}
		})
	}
}

func TestChat3ReplayWholeOmissionAuditedAnswerCompletes(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		value        []byte
		reason       wire.SealOmission
	}{
		{"incompatible", router.WireAnthropicMessages, []byte(`{"type":"thinking","signature":"s"}`), wire.SealOmissionIncompatibleOrigin},
		{"overbound", router.WireOpenAICompatible, bytes.Repeat([]byte("r"), chat3SealBytes+1), wire.SealOmissionEncodedSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body struct{ Messages []map[string]json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if len(body.Messages) != 1 || body.Messages[0]["reasoning_content"] != nil {
					t.Errorf("omitted seal escaped: %+v", body)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			var audits []chat3ReplayAudit
			ctx := withChat3Replay(context.Background(), []wire.ChatMessage{{Role: wire.RoleAssistant, Reasoning: []wire.ChatReplay{{Block: 9, Seal: wire.ReasoningSeal{Origin: tc.origin, Bytes: tc.value}}}}}, func(a chat3ReplayAudit) { audits = append(audits, a) })
			req := wire.Request{Messages: []wire.Message{{Role: wire.RoleAssistant, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "previous answer"}}}}}
			var answer string
			result := runChat3OpenAI(ctx, server.Client(), &router.Host{Base: server.URL, Chat: "/chat"}, "m", req, nil, nil, nil, nil, chat3Sink{Part: func(p wire.ChatPart) error { answer += p.Text; return nil }})
			if result.Err != nil || result.Cause != "" || answer != "answer" || calls.Load() != 1 || len(audits) != 1 || audits[0] != (chat3ReplayAudit{Message: 0, Block: 9, Reason: tc.reason}) {
				t.Fatalf("result %+v answer %q calls %d audits %+v", result, answer, calls.Load(), audits)
			}
		})
	}
}

func TestChat3ReplayRedactedBytesAndNoVisibleHistory(t *testing.T) {
	seal := []byte(`{"data":"opaque+/=\u003c","type":"redacted_thinking"}`)
	ctx := withChat3Replay(context.Background(), []wire.ChatMessage{{Role: wire.RoleAssistant, Reasoning: []wire.ChatReplay{{Seal: wire.ReasoningSeal{Origin: router.WireAnthropicMessages, Bytes: seal}}}}}, nil)
	req := wire.Request{Messages: []wire.Message{{Role: wire.RoleAssistant, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "answer"}}}}}
	body := chat3AnthropicBody("m", req, nil, nil, nil)
	chat3ApplyReplay(ctx, router.WireAnthropicMessages, req, body, nil)
	raw, opaque, err := chat3ReplayEncoding(body)
	if err != nil || !opaque || !bytes.Contains(raw, seal) || !json.Valid(raw) {
		t.Fatalf("encoding %s opaque %v err %v", raw, opaque, err)
	}
}
