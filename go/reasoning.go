package inference

import (
	"strings"
	"sync"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// ReasonReasoningOnly is the reason of a reply whose only output arrived as
// reasoning: no visible text and no tool call (INF-Q8).
const ReasonReasoningOnly = "upstream:reasoning_only"

// reasoningText accumulates streamed reasoning without exposing it as text.
// A trace longer than reasoningEchoBytes is counted as seen and not kept, so
// a trace is either echoed whole or not at all.
type reasoningText struct {
	b    strings.Builder
	seen bool
	over bool
}

func (r *reasoningText) add(s *string) {
	if s == nil || *s == "" {
		return
	}
	r.seen = true
	if r.over || r.b.Len()+len(*s) > reasoningEchoBytes {
		r.over = true
		r.b.Reset()
		return
	}
	//unchecked: strings.Builder.WriteString never returns a non-nil error
	r.b.WriteString(*s)
}

// addChunk adds one OpenAI-compatible delta's trace. Some servers (vLLM)
// send the same trace as both reasoning_content and reasoning; the delta's
// reasoning_content is taken when present and reasoning otherwise, so the
// echo stays byte for byte.
func (r *reasoningText) addChunk(content, reasoning *string) {
	if content != nil && *content != "" {
		r.add(content)
		return
	}
	r.add(reasoning)
}

// whole is the complete trace, or "" when it was too long to keep.
func (r *reasoningText) whole() string {
	if r.over {
		return ""
	}
	return r.b.String()
}

// reasoningEchoLimit and reasoningEchoBytes bound the remembered reasoning
// traces by count and by total bytes; the oldest go first.
const (
	reasoningEchoLimit = 1024
	reasoningEchoBytes = 32 << 20
)

// reasoningEcho remembers the reasoning an OpenAI-compatible model server sent
// with a tool-call reply, keyed by the caller's account and program, the model
// server, the model and the call id, and puts it back byte for byte when the
// same caller returns that assistant turn (INF-Q9). chat@1 has no reasoning
// part, so clients cannot carry the trace; deepseek-reasoner refuses a
// tool-call turn without it (HTTP 400). Another program presenting the same
// call id receives nothing.
type reasoningEcho struct {
	mu    sync.Mutex
	trace map[string]string
	order []string
	bytes int
}

var echoes = &reasoningEcho{trace: map[string]string{}}

func echoKey(subject Subject, host, model, callID string) string {
	return subject.Account + "\x00" + subject.Program + "\x00" + host + "\x00" + model + "\x00" + callID
}

func (e *reasoningEcho) remember(subject Subject, host, model string, callIDs []string, trace string) {
	if trace == "" || len(trace) > reasoningEchoBytes {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range callIDs {
		if id == "" {
			continue
		}
		key := echoKey(subject, host, model, id)
		if old, seen := e.trace[key]; seen {
			e.bytes -= len(old)
		} else {
			e.order = append(e.order, key)
		}
		e.trace[key] = trace
		e.bytes += len(trace)
	}
	for len(e.order) > reasoningEchoLimit || e.bytes > reasoningEchoBytes {
		e.bytes -= len(e.trace[e.order[0]])
		delete(e.trace, e.order[0])
		e.order = e.order[1:]
	}
}

func (e *reasoningEcho) recall(subject Subject, host, model, callID string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	trace, ok := e.trace[echoKey(subject, host, model, callID)]
	return trace, ok
}

// caller is the subject the stream's operation runs for, or none.
func (s *stream) caller() Subject {
	if s == nil || s.op == nil {
		return Subject{}
	}
	return s.op.subject
}

// echoReasoning sets reasoning_content on each assistant tool-call message of
// an OpenAI-compatible body whose call the same caller, server and model
// answered with a remembered trace.
func echoReasoning(body map[string]any, subject Subject, host, model string) {
	messages, _ := body["messages"].([]map[string]any)
	for _, m := range messages {
		calls, _ := m["tool_calls"].([]map[string]any)
		for _, call := range calls {
			id, _ := call["id"].(string)
			if trace, ok := echoes.recall(subject, host, model, id); ok {
				m["reasoning_content"] = trace
				break
			}
		}
	}
}

// reasoningEffortExtension is the request extension that names an
// OpenAI-compatible reasoning effort.
const reasoningEffortExtension = "openai/reasoning_effort"

// withoutReasoning is req asking for no reasoning, for the one further
// attempt a reasoning-only reply gets (INF-Q8). A thinking model given a
// small output limit spends it all reasoning; asked with reasoning_effort
// none, a provider that supports that effort can turn thinking off and answer
// within the same limit. Only a host on
// this machine is asked again: a hosted server bills each attempt. A caller
// that named a reasoning effort keeps its own.
func withoutReasoning(host *router.Host, req wire.Request) (wire.Request, bool) {
	if host == nil || host.Hosted {
		return req, false
	}
	if _, named := req.Extensions[reasoningEffortExtension]; named {
		return req, false
	}
	extensions := make(map[string]string, len(req.Extensions)+1)
	for key, value := range req.Extensions {
		extensions[key] = value
	}
	extensions[reasoningEffortExtension] = "none"
	req.Extensions = extensions
	return req, true
}

// addUsage is the usage of two attempts of one reply.
func addUsage(a, b wire.Usage) wire.Usage {
	return wire.Usage{Input: a.Input + b.Input, Output: a.Output + b.Output, Cached: a.Cached + b.Cached}
}

// addCost is the spend of two attempts of one reply; nil when neither
// reported one, and the reported one when only one did or their currencies
// differ.
func addCost(a, b *wire.Cost) *wire.Cost {
	switch {
	case a == nil:
		return b
	case b == nil || a.Currency != b.Currency:
		return a
	}
	return &wire.Cost{Micros: a.Micros + b.Micros, Currency: a.Currency}
}

// addChatUsage is the chat@3 usage of two attempts of one reply: each count
// either attempt reported, summed.
func addChatUsage(a, b wire.ChatUsage) wire.ChatUsage {
	add := func(x, y *int64) *int64 {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		sum := *x + *y
		return &sum
	}
	return wire.ChatUsage{Input: add(a.Input, b.Input), Output: add(a.Output, b.Output), CacheRead: add(a.CacheRead, b.CacheRead),
		CacheWrite: add(a.CacheWrite, b.CacheWrite), Reasoning: add(a.Reasoning, b.Reasoning)}
}
