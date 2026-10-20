package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// chat3Sink is the owning operation's bounded event admission boundary. Seal
// admits a whole opaque value or fails; upstream parsing never truncates it.
type chat3Sink struct {
	Part     func(wire.ChatPart) error
	BlockEnd func(int64) error
	Usage    func(wire.ChatUsage) error
	Seal     func(int64, string, string, []byte) error
	Token    func()
	Now      func() time.Time
	Engine   func(*engineTimings)
}

type chat3UpstreamResult struct {
	Finish       wire.FinishReason
	Usage        wire.ChatUsage
	Cost         *wire.Cost
	StopSequence *string
	Cause        string
	Err          error
	RetryAfterMs *int64
}

// The response observer is scoped to this chat@3 request. Legacy post callers
// have no observer and retain their existing reply and header behavior.
type postResponseKey struct{}

func withPostResponse(ctx context.Context, observer func(*http.Response)) context.Context {
	return context.WithValue(ctx, postResponseKey{}, observer)
}

func postResponse(ctx context.Context, response *http.Response) {
	if observer, ok := ctx.Value(postResponseKey{}).(func(*http.Response)); ok {
		observer(response)
	}
}

// chat3RetryAfter accepts only the two HTTP Retry-After forms. The hint is
// evidence about a positive delay, bounded by the common one-day governor.
func chat3RetryAfter(value string, now func() time.Time) *int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	const maximum int64 = 86400000
	seconds, digits := int64(0), true
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
		if seconds <= maximum/1000 {
			seconds = seconds*10 + int64(c-'0')
		}
	}
	var milliseconds int64
	if digits {
		if seconds == 0 {
			return nil
		}
		if seconds > maximum/1000 {
			milliseconds = maximum
		} else {
			milliseconds = seconds * 1000
		}
	} else {
		date, err := http.ParseTime(value)
		if err != nil {
			return nil
		}
		if now == nil {
			now = time.Now
		}
		delay := date.Sub(now())
		if delay <= 0 {
			return nil
		}
		if delay >= 24*time.Hour {
			milliseconds = maximum
		} else {
			milliseconds = int64((delay + time.Millisecond - 1) / time.Millisecond)
		}
	}
	return &milliseconds
}

func chat3ObserveRetry(ctx context.Context, sink chat3Sink, hint **int64) context.Context {
	return withPostResponse(ctx, func(response *http.Response) {
		// Upstream authentication, policy and budget refusals confer no hint.
		// A response body may still refine a 5xx to a context refusal; callers
		// project the captured duration only for an unavailable outcome.
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 && response.StatusCode <= 599 {
			*hint = chat3RetryAfter(response.Header.Get("Retry-After"), sink.Now)
		}
	})
}

func chat3Base(req wire.Request, options *wire.ChatOptions) wire.Request {
	req.Tools = nil
	req.Options = nil
	if options != nil {
		req.Options = &wire.Options{MaxOutput: options.MaxOutput, Temperature: options.Temperature, Stop: options.Stop, JSONSchema: options.JSONSchema}
	}
	return req
}

func chat3OpenAIBody(model string, req wire.Request, options *wire.ChatOptions, tools []wire.ChatTool, images map[string][]byte) map[string]any {
	body := openAIBody(model, chat3Base(req, options), images)
	if len(tools) > 0 {
		out := make([]any, 0, len(tools))
		for _, t := range tools {
			f := map[string]any{"name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Parameters)}
			if t.Strict != nil {
				f["strict"] = *t.Strict
			}
			out = append(out, map[string]any{"type": "function", "function": f})
		}
		body["tools"] = out
	}
	if options != nil {
		if options.ParallelTools != nil {
			body["parallel_tool_calls"] = *options.ParallelTools
		}
		if c := options.ToolChoice; c != nil {
			switch c.Kind {
			case wire.ChatToolChoiceKindAuto:
				body["tool_choice"] = "auto"
			case wire.ChatToolChoiceKindNone:
				body["tool_choice"] = "none"
			case wire.ChatToolChoiceKindRequired:
				body["tool_choice"] = "required"
			case wire.ChatToolChoiceKindNamed:
				body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": c.Name}}
			}
		}
	}
	return body
}

func chat3AnthropicBody(model string, req wire.Request, options *wire.ChatOptions, tools []wire.ChatTool, images map[string][]byte) map[string]any {
	body := anthropicBody(model, chat3Base(req, options), images)
	if len(tools) > 0 {
		out := make([]any, 0, len(tools))
		for _, t := range tools {
			f := map[string]any{"name": t.Name, "description": t.Description, "input_schema": json.RawMessage(t.Parameters)}
			if t.Strict != nil {
				f["strict"] = *t.Strict
			}
			out = append(out, f)
		}
		body["tools"] = out
	}
	if options != nil {
		if options.JSONSchema != "" {
			body["output_config"] = map[string]any{"format": map[string]any{"type": "json_schema", "schema": json.RawMessage(options.JSONSchema)}}
		}
		var choice map[string]any
		if c := options.ToolChoice; c != nil {
			switch c.Kind {
			case wire.ChatToolChoiceKindAuto:
				choice = map[string]any{"type": "auto"}
			case wire.ChatToolChoiceKindNone:
				choice = map[string]any{"type": "none"}
			case wire.ChatToolChoiceKindRequired:
				choice = map[string]any{"type": "any"}
			case wire.ChatToolChoiceKindNamed:
				choice = map[string]any{"type": "tool", "name": c.Name}
			}
		}
		if options.ParallelTools != nil {
			if choice == nil {
				choice = map[string]any{"type": "auto"}
			}
			choice["disable_parallel_tool_use"] = !*options.ParallelTools
		}
		if choice != nil {
			body["tool_choice"] = choice
		}
	}
	return body
}

func chat3Finish(s string) wire.FinishReason {
	switch s {
	case "stop", "end_turn", "stop_sequence":
		return wire.FinishReasonStop
	case "length", "max_tokens":
		return wire.FinishReasonLength
	case "tool_calls", "function_call", "tool_use":
		return wire.FinishReasonToolCalls
	case "content_filter":
		return wire.FinishReasonContentFilter
	case "refusal":
		return wire.FinishReasonModelRefusal
	case "pause_turn":
		return wire.FinishReasonPaused
	case "context_window_exceeded":
		return wire.FinishReasonContextWindow
	case "malformed_tool_call":
		return wire.FinishReasonMalformedToolCall
	default:
		return wire.FinishReasonOther
	}
}

func (s chat3Sink) part(p wire.ChatPart) error {
	if s.Part != nil {
		if err := s.Part(p); err != nil {
			return err
		}
	}
	if (p.Text != "" || p.Arguments != "") && s.Token != nil {
		s.Token()
	}
	return nil
}
func (s chat3Sink) end(block int64) error {
	if s.BlockEnd != nil {
		return s.BlockEnd(block)
	}
	return nil
}
func (s chat3Sink) usage(u wire.ChatUsage) error {
	if s.Usage != nil {
		return s.Usage(u)
	}
	return nil
}
func (s chat3Sink) seal(block int64, origin, field string, b []byte) error {
	if s.Seal != nil {
		return s.Seal(block, origin, field, b)
	}
	return nil
}

type chat3OAIUsage struct {
	Input         *int64      `json:"prompt_tokens"`
	Output        *int64      `json:"completion_tokens"`
	CacheRead     *int64      `json:"prompt_cache_hit_tokens"`
	Cost          json.Number `json:"cost"`
	PromptDetails *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	OutputDetails *struct {
		Reasoning *int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func runChat3OpenAI(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.Request, options *wire.ChatOptions, tools []wire.ChatTool, images map[string][]byte, headers map[string]string, sink chat3Sink) chat3UpstreamResult {
	r := chat3UpstreamResult{Finish: wire.FinishReasonOther}
	var retry *int64
	ctx = chat3ObserveRetry(ctx, sink, &retry)
	body := chat3OpenAIBody(model, req, options, tools, images)
	chat3ApplyReasoning(ctx, body)
	chat3ApplyReplay(ctx, router.WireOpenAICompatible, req, body, images)
	resp, refused := chat3Post(ctx, client, host.ChatURL(), body, headers)
	if refused != nil {
		r.Cause = refused.Reason
		if refused.Outcome == wire.ReplyOutcomeUnavailable {
			r.RetryAfterMs = retry
		}
		return r
	}
	defer resp.Body.Close()
	var next int64
	blocks := map[string]int64{}
	var order []int64
	block := func(key string) int64 {
		if b, ok := blocks[key]; ok {
			return b
		}
		b := next
		next++
		blocks[key] = b
		order = append(order, b)
		return b
	}
	done, finished := false, false
	err := events(resp.Body, func(_, data string) bool {
		if data == "[DONE]" {
			done = true
			return false
		}
		var c struct {
			openAIChunk
			Usage   *chat3OAIUsage `json:"usage"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content, Reasoning, Refusal *string
					ReasoningContent            *string `json:"reasoning_content"`
					ToolCalls                   []struct {
						Index    int
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if e := json.Unmarshal([]byte(data), &c); e != nil {
			r.Cause, r.Err = "upstream:malformed", e
			return false
		}
		if sink.Engine != nil {
			sink.Engine(parseEngineTimings(c.Timings))
		}
		if len(c.Error) > 0 && string(c.Error) != "null" {
			r.Cause = "upstream:error"
			return false
		}
		for _, choice := range c.Choices {
			if choice.Index != 0 {
				r.Cause = "upstream:multiple_choices"
				return false
			}
			d := choice.Delta
			reasoning := d.ReasoningContent
			if reasoning == nil || *reasoning == "" {
				reasoning = d.Reasoning
			}
			for _, value := range []struct {
				key  string
				kind wire.ChatPartKind
				text *string
			}{{"reasoning", wire.ChatPartKindReasoning, reasoning}, {"text", wire.ChatPartKindText, d.Content}, {"refusal", wire.ChatPartKindModelRefusal, d.Refusal}} {
				if value.text != nil && *value.text != "" {
					if e := sink.part(wire.ChatPart{Block: block(value.key), Kind: value.kind, Text: *value.text}); e != nil {
						r.Err = e
						return false
					}
					if value.kind == wire.ChatPartKindReasoning {
						if e := sink.seal(block(value.key), router.WireOpenAICompatible, "reasoning_content", []byte(*value.text)); e != nil {
							r.Err = e
							return false
						}
					}
				}
			}
			for _, call := range d.ToolCalls {
				key := fmt.Sprintf("tool:%d", call.Index)
				_, seen := blocks[key]
				p := wire.ChatPart{Block: block(key), Kind: wire.ChatPartKindToolCall, Arguments: call.Function.Arguments}
				if !seen {
					p.CallID, p.Name = call.ID, call.Function.Name
				}
				if e := sink.part(p); e != nil {
					r.Err = e
					return false
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				r.Finish = chat3Finish(*choice.FinishReason)
				finished = true
			}
		}
		if u := c.Usage; u != nil {
			if u.Input != nil {
				r.Usage.Input = u.Input
			}
			if u.Output != nil {
				r.Usage.Output = u.Output
			}
			if u.CacheRead != nil {
				r.Usage.CacheRead = u.CacheRead
			}
			if u.PromptDetails != nil && u.PromptDetails.Cached != nil {
				r.Usage.CacheRead = u.PromptDetails.Cached
			}
			if u.OutputDetails != nil {
				r.Usage.Reasoning = u.OutputDetails.Reasoning
			}
			if m, ok := micros(u.Cost); ok {
				r.Cost = &wire.Cost{Micros: m, Currency: "USD"}
			}
			if e := sink.usage(r.Usage); e != nil {
				r.Err = e
				return false
			}
		}
		return true
	})
	if r.Err != nil || r.Cause != "" {
		return r
	}
	if err != nil || (!done && !finished) {
		r.Cause, r.Err = "upstream:stream", err
		return r
	}
	for _, b := range order {
		if e := sink.end(b); e != nil {
			r.Err = e
			return r
		}
	}
	return r
}

type chat3AntUsage struct {
	Input  *int64 `json:"input_tokens"`
	Output *int64 `json:"output_tokens"`
	Read   *int64 `json:"cache_read_input_tokens"`
	Write  *int64 `json:"cache_creation_input_tokens"`
}

func runChat3Anthropic(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.Request, options *wire.ChatOptions, tools []wire.ChatTool, images map[string][]byte, headers map[string]string, sink chat3Sink) chat3UpstreamResult {
	r := chat3UpstreamResult{Finish: wire.FinishReasonOther}
	var retry *int64
	ctx = chat3ObserveRetry(ctx, sink, &retry)
	with := map[string]string{"anthropic-version": router.AnthropicVersion}
	for k, v := range headers {
		with[k] = v
	}
	body := chat3AnthropicBody(model, req, options, tools, images)
	chat3ApplyReasoning(ctx, body)
	chat3ApplyReplay(ctx, router.WireAnthropicMessages, req, body, images)
	resp, refused := chat3Post(ctx, client, host.ChatURL(), body, with)
	clear(with)
	if refused != nil {
		r.Cause = refused.Reason
		if refused.Outcome == wire.ReplyOutcomeUnavailable {
			r.RetryAfterMs = retry
		}
		return r
	}
	defer resp.Body.Close()
	type state struct {
		block     int64
		kind      wire.ChatPartKind
		closed    bool
		initial   string
		arguments bool
	}
	blocks := map[int]*state{}
	var next int64
	var baseInput *int64
	mergeUsage := func(u *chat3AntUsage) error {
		if u == nil {
			return nil
		}
		if u.Input != nil {
			baseInput = u.Input
		}
		if u.Output != nil {
			r.Usage.Output = u.Output
		}
		if u.Read != nil {
			r.Usage.CacheRead = u.Read
		}
		if u.Write != nil {
			r.Usage.CacheWrite = u.Write
		}
		if baseInput != nil {
			n := *baseInput
			for _, extra := range []*int64{r.Usage.CacheRead, r.Usage.CacheWrite} {
				if extra != nil {
					if *extra < 0 || n < 0 || n > int64(^uint64(0)>>1)-*extra {
						return fmt.Errorf("invalid upstream usage")
					}
					n += *extra
				}
			}
			r.Usage.Input = &n
		}
		return sink.usage(r.Usage)
	}
	stopped := false
	err := events(resp.Body, func(_, data string) bool {
		var e struct {
			Type         string
			Index        int
			Message      struct{ Usage *chat3AntUsage }
			ContentBlock struct {
				Type, ID, Name, Text, Thinking, Signature, Data string
				Input                                           json.RawMessage
			} `json:"content_block"`
			Delta struct {
				Type, Text, Thinking, Signature string
				PartialJSON                     string  `json:"partial_json"`
				StopReason                      string  `json:"stop_reason"`
				StopSequence                    *string `json:"stop_sequence"`
			}
			Usage *chat3AntUsage
		}
		if x := json.Unmarshal([]byte(data), &e); x != nil {
			r.Cause, r.Err = "upstream:malformed", x
			return false
		}
		var x error
		switch e.Type {
		case "message_start":
			x = mergeUsage(e.Message.Usage)
		case "content_block_start":
			if _, exists := blocks[e.Index]; exists {
				r.Cause = "upstream:malformed"
				return false
			}
			b := &state{block: next}
			next++
			p := wire.ChatPart{Block: b.block}
			switch e.ContentBlock.Type {
			case "text":
				b.kind, p.Text = wire.ChatPartKindText, e.ContentBlock.Text
			case "thinking":
				b.kind, p.Text = wire.ChatPartKindReasoning, e.ContentBlock.Thinking
			case "redacted_thinking":
				b.kind = wire.ChatPartKindReasoning
			case "tool_use":
				b.kind, p.CallID, p.Name = wire.ChatPartKindToolCall, e.ContentBlock.ID, e.ContentBlock.Name
				if len(e.ContentBlock.Input) > 0 && string(e.ContentBlock.Input) != "null" {
					b.initial = string(e.ContentBlock.Input)
				}
			default:
				r.Cause = "upstream:unsupported_block"
				return false
			}
			blocks[e.Index], p.Kind = b, b.kind
			x = sink.part(p)
			if x == nil && e.ContentBlock.Type == "thinking" {
				x = sink.seal(b.block, router.WireAnthropicMessages, "thinking", []byte(e.ContentBlock.Thinking))
			}
			if x == nil && e.ContentBlock.Signature != "" {
				x = sink.seal(b.block, router.WireAnthropicMessages, "signature", []byte(e.ContentBlock.Signature))
			}
			if x == nil && e.ContentBlock.Type == "redacted_thinking" {
				x = sink.seal(b.block, router.WireAnthropicMessages, "data", []byte(e.ContentBlock.Data))
			}
		case "content_block_delta":
			b := blocks[e.Index]
			if b == nil || b.closed {
				r.Cause = "upstream:malformed"
				return false
			}
			p := wire.ChatPart{Block: b.block, Kind: b.kind}
			switch e.Delta.Type {
			case "text_delta":
				if b.kind != wire.ChatPartKindText {
					r.Cause = "upstream:malformed"
					return false
				}
				p.Text = e.Delta.Text
			case "thinking_delta":
				if b.kind != wire.ChatPartKindReasoning {
					r.Cause = "upstream:malformed"
					return false
				}
				p.Text = e.Delta.Thinking
				x = sink.seal(b.block, router.WireAnthropicMessages, "thinking", []byte(e.Delta.Thinking))
			case "input_json_delta":
				if b.kind != wire.ChatPartKindToolCall {
					r.Cause = "upstream:malformed"
					return false
				}
				p.Arguments = e.Delta.PartialJSON
				b.arguments = b.arguments || p.Arguments != ""
			case "signature_delta":
				if b.kind != wire.ChatPartKindReasoning {
					r.Cause = "upstream:malformed"
					return false
				}
				x = sink.seal(b.block, router.WireAnthropicMessages, "signature", []byte(e.Delta.Signature))
			default:
				r.Cause = "upstream:unsupported_delta"
				return false
			}
			if x == nil && e.Delta.Type != "signature_delta" {
				x = sink.part(p)
			}
		case "content_block_stop":
			b := blocks[e.Index]
			if b == nil || b.closed {
				r.Cause = "upstream:malformed"
				return false
			}
			b.closed = true
			if b.kind == wire.ChatPartKindToolCall && !b.arguments && b.initial != "" {
				x = sink.part(wire.ChatPart{Block: b.block, Kind: b.kind, Arguments: b.initial})
			}
			if x == nil {
				x = sink.end(b.block)
			}
		case "message_delta":
			if e.Delta.StopReason != "" {
				r.Finish = chat3Finish(e.Delta.StopReason)
			}
			r.StopSequence = e.Delta.StopSequence
			x = mergeUsage(e.Usage)
		case "message_stop":
			for _, b := range blocks {
				if !b.closed {
					r.Cause = "upstream:malformed"
					return false
				}
			}
			stopped = true
			return false
		case "error":
			r.Cause = "upstream:error"
			return false
		}
		if x != nil {
			r.Err = x
			return false
		}
		return true
	})
	if r.Cause == "" && r.Err == nil && (err != nil || !stopped) {
		r.Cause, r.Err = "upstream:stream", err
	}
	return r
}
