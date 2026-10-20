package inference

import (
	"context"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// The catalogue-MTP short reply (research/live-runs/2026-10-03-catalogue-mtp.md):
// a model server spends the whole output budget in reasoning_content. The
// reply is a typed refusal, never a silent empty completion, and the trace
// never becomes answer text (INF-Q8).
func TestAReasoningOnlyReplyIsATypedRefusalNotEmptyText(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.up.chunks = nil
	f.up.reasoning = []string{"Let me think", " about hello."}
	f.up.input, f.up.output = 22, 128
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	text, reply, deltas := drain(t, f.provider, caller, a.Operation)
	if text != "" || reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != ReasonReasoningOnly {
		t.Fatalf("text %q reply %+v", text, reply)
	}
	if reply.Usage.Output != 128 {
		t.Fatalf("usage lost: %+v", reply.Usage)
	}
	for _, d := range deltas {
		if d.Part != nil && strings.Contains(d.Part.Text, "think") {
			t.Fatalf("reasoning delivered as a part: %+v", d)
		}
	}
}

// Reasoning followed by visible text completes with the text alone.
func TestReasoningBeforeTextKeepsOnlyTheText(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.up.chunks = []string{"Hello."}
	f.up.reasoning = []string{"Greeting requested."}
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	text, reply, _ := drain(t, f.provider, caller, a.Operation)
	if text != "Hello." || reply.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("text %q reply %+v", text, reply)
	}
}

// A thinking-only reply on the Anthropic wire ends refused with
// upstream:reasoning_only, as on the OpenAI-compatible wire (INF-Q8), and its
// thinking never becomes a text part.
func TestAnAnthropicThinkingOnlyReplyIsATypedRefusal(t *testing.T) {
	f := setup(t, router.WireAnthropicMessages, nil)
	f.up.chunks = nil
	f.up.reasoning = []string{"Weighing", " the greeting."}
	f.up.input, f.up.output = 9, 64
	req := hostedRequest("anthropic")
	req.Model = "claude-sonnet-5"
	a := f.provider.Start(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	text, reply, deltas := drain(t, f.provider, caller, a.Operation)
	if text != "" || reply.Outcome != wire.ReplyOutcomeRefused || reply.Reason != ReasonReasoningOnly || reply.Usage.Output != 64 {
		t.Fatalf("text %q reply %+v", text, reply)
	}
	for _, d := range deltas {
		if d.Part != nil && strings.Contains(d.Part.Text, "Weighing") {
			t.Fatalf("thinking delivered as a part: %+v", d)
		}
	}

	// Thinking followed by text completes with the text alone.
	f.up.chunks = []string{"Hello!"}
	a = f.provider.Start(context.Background(), caller, req)
	if text, reply, _ := drain(t, f.provider, caller, a.Operation); text != "Hello!" || reply.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("text %q reply %+v", text, reply)
	}
}

// The echo is the caller's own: another program presenting the same call id
// receives no trace (INF-Q9).
func TestAReasoningEchoIsKeptForItsCallerOnly(t *testing.T) {
	e := &reasoningEcho{trace: map[string]string{}}
	jan := Subject{Account: "S-1", Program: `C:\apps\jan.exe`}
	other := Subject{Account: "S-1", Program: `C:\apps\other.exe`}
	e.remember(jan, "deepseek", "deepseek-reasoner", []string{"call_1"}, "trace")
	if got, ok := e.recall(jan, "deepseek", "deepseek-reasoner", "call_1"); !ok || got != "trace" {
		t.Fatalf("the caller's own trace: %q %v", got, ok)
	}
	if _, ok := e.recall(other, "deepseek", "deepseek-reasoner", "call_1"); ok {
		t.Fatal("another program received the caller's trace")
	}
}

// The remembered traces are bounded in bytes as well as in count: the oldest
// trace goes once the total passes reasoningEchoBytes, and a trace longer
// than the bound is never kept, whole or in part.
func TestReasoningEchoIsBoundedInBytes(t *testing.T) {
	e := &reasoningEcho{trace: map[string]string{}}
	s := Subject{Account: "S-1", Program: "p"}
	half := strings.Repeat("r", reasoningEchoBytes/2+1)
	e.remember(s, "h", "m", []string{"a"}, half)
	e.remember(s, "h", "m", []string{"b"}, half)
	if _, ok := e.recall(s, "h", "m", "a"); ok {
		t.Fatal("the oldest trace was kept past the byte bound")
	}
	if _, ok := e.recall(s, "h", "m", "b"); !ok || e.bytes != len(half) {
		t.Fatalf("the newest trace was lost, %d bytes held", e.bytes)
	}
	var long reasoningText
	piece := strings.Repeat("x", 1<<20)
	for i := 0; i <= reasoningEchoBytes>>20; i++ {
		long.add(&piece)
	}
	if !long.seen || long.whole() != "" {
		t.Fatal("a trace past the bound was kept in part")
	}
}

// A delta carrying the trace as both reasoning_content and reasoning (some
// vLLM versions) adds it once, so the echo is byte for byte.
func TestAChunkWithBothReasoningFieldsAddsTheTraceOnce(t *testing.T) {
	var r reasoningText
	content, reasoning := "step one", "step one"
	r.addChunk(&content, &reasoning)
	only := "step two"
	r.addChunk(nil, &only)
	if got := r.whole(); got != "step onestep two" {
		t.Fatalf("trace %q", got)
	}
}
