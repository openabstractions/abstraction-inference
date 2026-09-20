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

// openAI speaks OpenAI chat completions with server-sent events, the wire of
// OpenRouter, OpenAI, Groq, Mistral, LiteLLM and every local runtime's /v1.
type openAI struct{}

var openAIFeatures = map[string]bool{FeatureTools: true, FeatureJSONSchema: true, FeatureVision: true}

// openAIExtensions are the extension keys this wire maps onto request fields.
var openAIExtensions = map[string]string{"openai/reasoning_effort": "reasoning_effort", "openai/user": "user"}

func (openAI) unsupported(req wire.Request) string {
	known := map[string]bool{}
	for k := range openAIExtensions {
		known[k] = true
	}
	return unsupportedBy(req, openAIFeatures, known)
}

func openAIBody(model string, req wire.Request, images map[string][]byte) map[string]any {
	var messages []map[string]any
	for _, m := range req.Messages {
		var text []string
		var content []map[string]any
		var calls []map[string]any
		for _, part := range m.Parts {
			switch part.Kind {
			case wire.PartKindText:
				text = append(text, part.Text)
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			case wire.PartKindImage:
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:" + part.MediaType + ";base64," + base64.StdEncoding.EncodeToString(images[part.Digest])}})
			case wire.PartKindToolCall:
				calls = append(calls, map[string]any{"id": part.CallID, "type": "function",
					"function": map[string]any{"name": part.Name, "arguments": part.Arguments}})
			case wire.PartKindToolResult:
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": part.CallID, "content": part.Text})
			}
		}
		if m.Role == wire.RoleTool {
			continue
		}
		var bodyContent any = strings.Join(text, "")
		if len(content) != len(text) {
			bodyContent = content
		}
		out := map[string]any{"role": m.Role.String(), "content": bodyContent}
		if len(calls) > 0 {
			out["tool_calls"] = calls
			if len(content) == 0 {
				out["content"] = nil
			}
		}
		if len(content) > 0 || len(calls) > 0 {
			messages = append(messages, out)
		}
	}
	body := map[string]any{"model": model, "messages": messages, "stream": true,
		"stream_options": map[string]any{"include_usage": true}}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.Parameters)}})
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
			body["stop"] = o.Stop
		}
		if o.JSONSchema != "" {
			body["response_format"] = map[string]any{"type": "json_schema",
				"json_schema": map[string]any{"name": "reply", "schema": json.RawMessage(o.JSONSchema)}}
		}
	}
	for key, value := range req.Extensions {
		if field, ok := openAIExtensions[key]; ok {
			body[field] = value
		}
	}
	return body
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64       `json:"prompt_tokens"`
		CompletionTokens    int64       `json:"completion_tokens"`
		Cost                json.Number `json:"cost"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func openAIStop(reason string) wire.StopReason {
	switch reason {
	case "stop":
		return wire.StopReasonEnd
	case "length":
		return wire.StopReasonMaxOutput
	case "tool_calls", "function_call":
		return wire.StopReasonToolCalls
	case "content_filter":
		return wire.StopReasonContentFilter
	}
	return wire.StopReasonOther
}

func (openAI) run(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.Request, images map[string][]byte, headers map[string]string, s *stream) wire.Reply {
	resp, refused := post(ctx, client, host.ChatURL(), openAIBody(model, req, images), headers)
	if refused != nil {
		return *refused
	}
	defer resp.Body.Close()
	reply := wire.Reply{Outcome: wire.ReplyOutcomeCompleted, StopReason: wire.StopReasonNoStop}
	next := int64(0)
	textIndex := int64(-1)
	calls := map[int]int64{}
	done, malformed := false, false
	err := events(resp.Body, func(_, data string) bool {
		if data == "[DONE]" {
			done = true
			return false
		}
		var chunk openAIChunk
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		if dec.Decode(&chunk) != nil {
			malformed = true
			return false
		}
		for _, choice := range chunk.Choices {
			if c := choice.Delta.Content; c != nil && *c != "" {
				if textIndex < 0 {
					textIndex, next = next, next+1
				}
				s.part(textIndex, wire.Part{Kind: wire.PartKindText, Text: *c})
			}
			for _, call := range choice.Delta.ToolCalls {
				index, seen := calls[call.Index]
				part := wire.Part{Kind: wire.PartKindToolCall, Arguments: call.Function.Arguments}
				if !seen {
					index, next = next, next+1
					calls[call.Index] = index
					part.CallID, part.Name = call.ID, call.Function.Name
				}
				s.part(index, part)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				reply.StopReason = openAIStop(*choice.FinishReason)
			}
		}
		if u := chunk.Usage; u != nil {
			reply.Usage = wire.Usage{Input: u.PromptTokens, Output: u.CompletionTokens}
			if u.PromptTokensDetails != nil {
				reply.Usage.Cached = u.PromptTokensDetails.CachedTokens
			}
			if m, ok := micros(u.Cost); ok {
				reply.Cost = &wire.Cost{Micros: m, Currency: "USD"}
			}
			s.usage(reply.Usage)
		}
		return true
	})
	switch {
	case malformed:
		return failed(wire.ReplyOutcomeUnavailable, "upstream:malformed")
	case err != nil || (!done && reply.StopReason == wire.StopReasonNoStop):
		broken := streamBroken(ctx, err)
		broken.Usage, broken.Cost = reply.Usage, reply.Cost
		return broken
	}
	if reply.StopReason == wire.StopReasonNoStop {
		reply.StopReason = wire.StopReasonEnd
	}
	return reply
}
