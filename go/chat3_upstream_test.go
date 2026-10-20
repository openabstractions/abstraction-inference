package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func chat3Fixture(t *testing.T, frames ...string) (*http.Client, *router.Host) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}
	}))
	t.Cleanup(s.Close)
	return s.Client(), &router.Host{Base: s.URL, Chat: "/chat"}
}

func TestChat3NegativeCostIsUnobservedWithUsagePreserved(t *testing.T) {
	client, host := chat3Fixture(t,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"cost":-0.01}}`, `[DONE]`)
	r := runChat3OpenAI(context.Background(), client, host, "m", wire.Request{}, nil, nil, nil, nil, chat3Sink{})
	if r.Err != nil || r.Cause != "" || r.Cost != nil || r.Usage.Input == nil || *r.Usage.Input != 3 || r.Usage.Output == nil || *r.Usage.Output != 2 {
		t.Fatal(r)
	}
}

func TestChat3OpenAIAliasesUsageAndTokenProgress(t *testing.T) {
	client, host := chat3Fixture(t,
		`{"choices":[{"delta":{"reasoning_content":"r","reasoning":"r"}}]}`,
		`{"choices":[{"delta":{"reasoning":"s"}}]}`,
		`{"choices":[{"delta":{"content":"answer"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"completion_tokens":0,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		`[DONE]`)
	var parts []wire.ChatPart
	var ends []int64
	var seals []string
	tokens := 0
	r := runChat3OpenAI(context.Background(), client, host, "m", wire.Request{}, nil, nil, nil, nil, chat3Sink{
		Part:     func(p wire.ChatPart) error { parts = append(parts, p); return nil },
		BlockEnd: func(b int64) error { ends = append(ends, b); return nil },
		Seal: func(_ int64, origin, field string, b []byte) error {
			if origin != router.WireOpenAICompatible {
				t.Errorf("origin %q", origin)
			}
			seals = append(seals, string(b))
			return nil
		},
		Token: func() { tokens++ },
	})
	if r.Err != nil || r.Cause != "" {
		t.Fatalf("result %+v", r)
	}
	if len(parts) != 3 || parts[0].Kind != wire.ChatPartKindReasoning || parts[1].Block != parts[0].Block || parts[2].Kind != wire.ChatPartKindText || parts[2].Block == parts[0].Block {
		t.Fatalf("parts %+v", parts)
	}
	if tokens != 3 || !reflect.DeepEqual(ends, []int64{0, 1}) || !reflect.DeepEqual(seals, []string{"r", "s"}) {
		t.Fatalf("tokens %d ends %v seals %v", tokens, ends, seals)
	}
	if r.Usage.Input != nil || r.Usage.Output == nil || *r.Usage.Output != 0 || r.Usage.Reasoning == nil || *r.Usage.Reasoning != 0 {
		t.Fatalf("usage %+v", r.Usage)
	}
}

func TestChat3AnthropicBlocksSealAndZeroUsage(t *testing.T) {
	client, host := chat3Fixture(t,
		`{"type":"message_start","message":{"usage":{"input_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":7,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":7,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
		`{"type":"content_block_delta","index":7,"delta":{"type":"signature_delta","signature":"opaque+/="}}`,
		`{"type":"content_block_stop","index":7}`,
		`{"type":"content_block_start","index":9,"content_block":{"type":"tool_use","id":"call","name":"tool","input":{}}}`,
		`{"type":"content_block_delta","index":9,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
		`{"type":"content_block_stop","index":9}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":0}}`,
		`{"type":"message_stop"}`)
	var parts []wire.ChatPart
	var ends []int64
	var seal string
	tokens := 0
	r := runChat3Anthropic(context.Background(), client, host, "m", wire.Request{}, nil, nil, nil, nil, chat3Sink{
		Part:     func(p wire.ChatPart) error { parts = append(parts, p); return nil },
		BlockEnd: func(b int64) error { ends = append(ends, b); return nil },
		Seal: func(block int64, origin, field string, b []byte) error {
			if block != 0 || origin != router.WireAnthropicMessages {
				t.Errorf("seal %d %s", block, origin)
			}
			if field == "signature" {
				seal += string(b)
			}
			return nil
		},
		Token: func() { tokens++ },
	})
	if r.Err != nil || r.Cause != "" || r.Finish != wire.FinishReasonToolCalls {
		t.Fatalf("result %+v", r)
	}
	if len(parts) != 4 || parts[1].Text != "thought" || parts[2].CallID != "call" || parts[3].Arguments != "{}" || tokens != 2 || seal != "opaque+/=" || !reflect.DeepEqual(ends, []int64{0, 1}) {
		t.Fatalf("parts %+v tokens %d seal %q ends %v", parts, tokens, seal, ends)
	}
	if r.Usage.Input == nil || *r.Usage.Input != 9 || r.Usage.Output == nil || *r.Usage.Output != 0 || r.Usage.Reasoning != nil {
		t.Fatalf("usage %+v", r.Usage)
	}
}

func TestChat3SinkFailureStopsItsUpstream(t *testing.T) {
	client, host := chat3Fixture(t, `{"choices":[{"delta":{"reasoning":"large"}}]}`, `[DONE]`)
	want := errors.New("seal governor")
	r := runChat3OpenAI(context.Background(), client, host, "m", wire.Request{}, nil, nil, nil, nil, chat3Sink{Seal: func(_ int64, _, _ string, b []byte) error {
		if string(b) != "large" {
			t.Fatalf("truncated %q", b)
		}
		return want
	}})
	if !errors.Is(r.Err, want) {
		t.Fatalf("result %+v", r)
	}
}

func TestChat3OversizedSealOmissionStillCompletesAnswer(t *testing.T) {
	client, host := chat3Fixture(t,
		fmt.Sprintf(`{"choices":[{"delta":{"reasoning":%q}}]}`, strings.Repeat("r", chat3SealBytes+1)),
		`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		`[DONE]`)
	var seal chat3SealBuilder
	var omitted *wire.SealOmission
	var answer string
	r := runChat3OpenAI(context.Background(), client, host, "m", wire.Request{}, nil, nil, nil, nil, chat3Sink{
		Part: func(p wire.ChatPart) error {
			if p.Kind == wire.ChatPartKindText {
				answer += p.Text
			}
			return nil
		},
		Seal: func(_ int64, origin, field string, b []byte) error {
			seal.append(origin, field, b)
			return nil
		},
		BlockEnd: func(block int64) error {
			if block == 0 {
				value, why := seal.finish()
				if value != nil {
					t.Fatal("an oversized prefix escaped as a seal")
				}
				omitted = why
			}
			return nil
		},
	})
	if r.Err != nil || r.Cause != "" || answer != "answer" || omitted == nil || *omitted != wire.SealOmissionEncodedSize {
		t.Fatalf("answer %q omission %v result %+v", answer, omitted, r)
	}
}

func TestChat3RequestControlsPreserveExplicitFalse(t *testing.T) {
	f := false
	options := &wire.ChatOptions{ToolChoice: &wire.ChatToolChoice{Kind: wire.ChatToolChoiceKindNamed, Name: "tool"}, ParallelTools: &f, JSONSchema: `{"type":"object"}`}
	tools := []wire.ChatTool{{Name: "tool", Parameters: `{"type":"object"}`, Strict: &f}}
	for _, body := range []map[string]any{chat3OpenAIBody("m", wire.Request{}, options, tools, nil), chat3AnthropicBody("m", wire.Request{}, options, tools, nil)} {
		if _, err := json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	oai := chat3OpenAIBody("m", wire.Request{}, options, tools, nil)
	if oai["parallel_tool_calls"] != false || oai["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["strict"] != false {
		t.Fatalf("OpenAI controls %v", oai)
	}
	ant := chat3AnthropicBody("m", wire.Request{}, options, tools, nil)
	choice := ant["tool_choice"].(map[string]any)
	if choice["name"] != "tool" || choice["disable_parallel_tool_use"] != true || ant["tools"].([]any)[0].(map[string]any)["strict"] != false {
		t.Fatalf("Anthropic controls %v", ant)
	}
}
