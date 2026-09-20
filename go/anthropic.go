package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// anthropic speaks Anthropic messages with server-sent events.
type anthropic struct{}

var anthropicFeatures = map[string]bool{FeatureTools: true, FeatureVision: true}

// anthropicDefaultMaxOutput is sent when a request leaves max_output to the
// host, because the messages wire requires max_tokens.
const anthropicDefaultMaxOutput = 4096

func (anthropic) unsupported(req wire.Request) string {
	return unsupportedBy(req, anthropicFeatures, map[string]bool{})
}

func anthropicBody(model string, req wire.Request, images map[string][]byte) map[string]any {
	var system []string
	var messages []map[string]any
	for _, m := range req.Messages {
		var blocks []map[string]any
		for _, part := range m.Parts {
			switch part.Kind {
			case wire.PartKindText:
				if m.Role == wire.RoleSystem {
					system = append(system, part.Text)
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			case wire.PartKindImage:
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": part.MediaType, "data": base64.StdEncoding.EncodeToString(images[part.Digest])}})
			case wire.PartKindToolCall:
				input := part.Arguments
				if input == "" {
					input = "{}"
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": part.CallID, "name": part.Name, "input": json.RawMessage(input)})
			case wire.PartKindToolResult:
				blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": part.CallID, "content": part.Text})
			}
		}
		if m.Role == wire.RoleSystem || len(blocks) == 0 {
			continue
		}
		role := "user"
		if m.Role == wire.RoleAssistant {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": blocks})
	}
	body := map[string]any{"model": model, "messages": messages, "stream": true, "max_tokens": anthropicDefaultMaxOutput}
	if len(system) > 0 {
		body["system"] = strings.Join(system, "\n\n")
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": json.RawMessage(t.Parameters)})
		}
		body["tools"] = tools
	}
	if o := req.Options; o != nil {
		if o.MaxOutput > 0 {
			body["max_tokens"] = o.MaxOutput
		}
		if o.Temperature != nil {
			body["temperature"] = temperature(o.Temperature)
		}
		if len(o.Stop) > 0 {
			body["stop_sequences"] = o.Stop
		}
	}
	return body
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
}

func anthropicStop(reason string) wire.StopReason {
	switch reason {
	case "end_turn":
		return wire.StopReasonEnd
	case "max_tokens":
		return wire.StopReasonMaxOutput
	case "stop_sequence":
		return wire.StopReasonStopSequence
	case "tool_use":
		return wire.StopReasonToolCalls
	case "refusal":
		return wire.StopReasonContentFilter
	}
	return wire.StopReasonOther
}

func (anthropic) run(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.Request, images map[string][]byte, headers map[string]string, s *stream) wire.Reply {
	with := map[string]string{"anthropic-version": router.AnthropicVersion}
	for k, v := range headers {
		with[k] = v
	}
	resp, refused := post(ctx, client, host.ChatURL(), anthropicBody(model, req, images), with)
	clear(with)
	if refused != nil {
		return *refused
	}
	defer resp.Body.Close()
	reply := wire.Reply{Outcome: wire.ReplyOutcomeCompleted, StopReason: wire.StopReasonNoStop}
	blocks := map[int]int64{}
	next := int64(0)
	stopped, malformed := false, false
	var upstreamError string
	err := events(resp.Body, func(event, data string) bool {
		var e anthropicEvent
		if json.Unmarshal([]byte(data), &e) != nil {
			malformed = true
			return false
		}
		switch e.Type {
		case "message_start":
			reply.Usage.Input = e.Message.Usage.InputTokens + e.Message.Usage.CacheReadInputTokens
			reply.Usage.Cached = e.Message.Usage.CacheReadInputTokens
			reply.Usage.Output = e.Message.Usage.OutputTokens
		case "content_block_start":
			part := wire.Part{}
			switch e.ContentBlock.Type {
			case "text":
				part.Kind = wire.PartKindText
			case "tool_use":
				part = wire.Part{Kind: wire.PartKindToolCall, CallID: e.ContentBlock.ID, Name: e.ContentBlock.Name}
			default:
				return true
			}
			blocks[e.Index], next = next, next+1
			if part.Kind == wire.PartKindToolCall {
				s.part(blocks[e.Index], part)
			}
		case "content_block_delta":
			index, ok := blocks[e.Index]
			if !ok {
				return true
			}
			switch e.Delta.Type {
			case "text_delta":
				s.part(index, wire.Part{Kind: wire.PartKindText, Text: e.Delta.Text})
			case "input_json_delta":
				s.part(index, wire.Part{Kind: wire.PartKindToolCall, Arguments: e.Delta.PartialJSON})
			}
		case "message_delta":
			if e.Delta.StopReason != "" {
				reply.StopReason = anthropicStop(e.Delta.StopReason)
			}
			if e.Usage.OutputTokens > 0 {
				reply.Usage.Output = e.Usage.OutputTokens
			}
			s.usage(reply.Usage)
		case "message_stop":
			stopped = true
			return false
		case "error":
			upstreamError = "upstream:error"
			return false
		}
		return true
	})
	switch {
	case malformed:
		return failed(wire.ReplyOutcomeUnavailable, "upstream:malformed")
	case upstreamError != "":
		r := failed(wire.ReplyOutcomeUnavailable, upstreamError)
		r.Usage = reply.Usage
		return r
	case err != nil || !stopped:
		broken := streamBroken(ctx, err)
		broken.Usage = reply.Usage
		return broken
	}
	if reply.StopReason == wire.StopReasonNoStop {
		reply.StopReason = wire.StopReasonEnd
	}
	return reply
}
