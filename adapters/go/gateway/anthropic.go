package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/client"
)

type antRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int64           `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []antMessage    `json:"messages"`
	Tools         []antTool       `json:"tools"`
	StopSequences []string        `json:"stop_sequences"`
	Temperature   *float64        `json:"temperature"`
	Stream        bool            `json:"stream"`
}

type antMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type antBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type antTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type"`
}

// antBlocks reads Anthropic content: a string or an array of blocks.
func antBlocks(raw json.RawMessage) ([]antBlock, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []antBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []antBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content is neither a string nor an array of blocks")
	}
	return blocks, nil
}

// fromAnthropic translates a messages request into chat@1. A user message's
// tool_result blocks become a tool message ahead of its text.
func fromAnthropic(in antRequest) (wire.Request, error) {
	req := wire.Request{Model: in.Model, Messages: []wire.Message{}}
	system, err := antBlocks(in.System)
	if err != nil {
		return req, fmt.Errorf("system: %w", err)
	}
	if len(system) > 0 {
		var parts []wire.Part
		for _, b := range system {
			if b.Type != "text" {
				return req, fmt.Errorf("system block type %q", b.Type)
			}
			parts = append(parts, wire.Part{Kind: wire.PartKindText, Text: b.Text})
		}
		req.Messages = append(req.Messages, wire.Message{Role: wire.RoleSystem, Parts: parts})
	}
	for _, m := range in.Messages {
		blocks, err := antBlocks(m.Content)
		if err != nil {
			return req, err
		}
		var parts, results []wire.Part
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, wire.Part{Kind: wire.PartKindText, Text: b.Text})
			case "tool_use":
				args := string(bytes.TrimSpace(b.Input))
				if args == "" || args == "null" {
					args = "{}"
				}
				parts = append(parts, wire.Part{Kind: wire.PartKindToolCall, CallID: b.ID, Name: b.Name, Arguments: args})
			case "tool_result":
				inner, err := antBlocks(b.Content)
				if err != nil {
					return req, fmt.Errorf("tool_result: %w", err)
				}
				var text strings.Builder
				for _, ib := range inner {
					if ib.Type != "text" {
						return req, errUnsupported{inference.FeatureVision}
					}
					//unchecked: strings.Builder.WriteString never returns a non-nil error
				text.WriteString(ib.Text)
				}
				results = append(results, wire.Part{Kind: wire.PartKindToolResult, CallID: b.ToolUseID, Text: text.String()})
			case "image", "document":
				return req, errUnsupported{inference.FeatureVision}
			case "thinking", "redacted_thinking":
			default:
				return req, fmt.Errorf("content block type %q", b.Type)
			}
		}
		switch m.Role {
		case "user":
			if len(results) > 0 {
				req.Messages = append(req.Messages, wire.Message{Role: wire.RoleTool, Parts: results})
			}
			if len(parts) > 0 || len(results) == 0 {
				if len(parts) == 0 {
					parts = []wire.Part{{Kind: wire.PartKindText}}
				}
				req.Messages = append(req.Messages, wire.Message{Role: wire.RoleUser, Parts: parts})
			}
		case "assistant":
			if len(results) > 0 {
				return req, errors.New("tool_result in an assistant message")
			}
			if len(parts) == 0 {
				parts = []wire.Part{{Kind: wire.PartKindText}}
			}
			req.Messages = append(req.Messages, wire.Message{Role: wire.RoleAssistant, Parts: parts})
		default:
			return req, fmt.Errorf("message role %q", m.Role)
		}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			return req, errUnsupported{"anthropic/tool:" + t.Type}
		}
		params := string(t.InputSchema)
		if params == "" || params == "null" {
			params = `{"type":"object","properties":{}}`
		}
		req.Tools = append(req.Tools, wire.Tool{Name: t.Name, Description: t.Description, Parameters: params})
	}
	options := wire.Options{MaxOutput: in.MaxTokens, Stop: in.StopSequences}
	if in.Temperature != nil {
		options.Temperature = milli(*in.Temperature)
	}
	if options.MaxOutput != 0 || options.Temperature != nil || len(options.Stop) > 0 {
		req.Options = &options
	}
	return req, nil
}

func anthropicStop(reason wire.StopReason) string {
	switch reason {
	case wire.StopReasonMaxOutput:
		return "max_tokens"
	case wire.StopReasonStopSequence:
		return "stop_sequence"
	case wire.StopReasonToolCalls:
		return "tool_use"
	}
	return "end_turn"
}

func anthropicUsage(u wire.Usage) map[string]any {
	return map[string]any{"input_tokens": u.Input - u.Cached, "output_tokens": u.Output, "cache_read_input_tokens": u.Cached}
}

func toolInput(arguments string) json.RawMessage {
	if strings.TrimSpace(arguments) == "" || !json.Valid([]byte(arguments)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(arguments)
}

func (w *Window) anthropicMessages(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	var in antRequest
	if !w.readBody(rw, r, b, &in) {
		return
	}
	req, err := fromAnthropic(in)
	if err != nil {
		w.badRequest(rw, b, in.Model, err)
		return
	}
	admission := w.start(r, b, grant, req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		writeError(rw, status(admission.Outcome.String()), admission.Outcome.String(), refusalMessage(admission.Outcome.String(), admission.Reason))
		return
	}
	id := "msg_" + admission.Operation
	if !in.Stream {
		var fold client.Fold
		end, ok := w.observe(r, b, admission.Operation, func(d wire.Delta) bool { fold.Add(d); return true })
		if !ok {
			return
		}
		if end.Outcome != wire.ReplyOutcomeCompleted {
			writeError(rw, status(end.Outcome.String()), end.Outcome.String(), refusalMessage(end.Outcome.String(), end.Reason))
			return
		}
		reply, _ := fold.Reply()
		content := []any{}
		for _, part := range reply.Message.Parts {
			switch part.Kind {
			case wire.PartKindText:
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			case wire.PartKindToolCall:
				content = append(content, map[string]any{"type": "tool_use", "id": part.CallID, "name": part.Name, "input": toolInput(part.Arguments)})
			}
		}
		rw.Header().Set("Content-Type", "application/json")
		//unchecked: terminal write of the response; headers and status are already committed and this package has no logger to report a write failure to (matches writeError and the other Encode calls in this package)
		json.NewEncoder(rw).Encode(map[string]any{"id": id, "type": "message", "role": "assistant", "model": reply.Model, "content": content,
			"stop_reason": anthropicStop(reply.StopReason), "stop_sequence": nil, "usage": anthropicUsage(reply.Usage)})
		return
	}
	flusher, _ := rw.(http.Flusher)
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	send := func(event string, v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(rw, "event: %s\ndata: %s\n\n", event, raw); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	if !send("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant",
		"model": admission.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}) {
		w.cfg.Chat.Cancel(b.Subject(), admission.Operation)
		return
	}
	// Anthropic blocks open and close in order; each chat@1 part index opens
	// the next block and closes the one before it.
	blocks := map[int64]int{}
	open := -1
	closeOpen := func() bool {
		if open < 0 {
			return true
		}
		ok := send("content_block_stop", map[string]any{"type": "content_block_stop", "index": open})
		open = -1
		return ok
	}
	end, ok := w.observe(r, b, admission.Operation, func(d wire.Delta) bool {
		if d.Kind != wire.DeltaKindPart || d.Part == nil {
			return true
		}
		index, seen := blocks[d.Index]
		if !seen {
			if !closeOpen() {
				return false
			}
			index = len(blocks)
			blocks[d.Index] = index
			block := map[string]any{"type": "text", "text": ""}
			if d.Part.Kind == wire.PartKindToolCall {
				block = map[string]any{"type": "tool_use", "id": d.Part.CallID, "name": d.Part.Name, "input": map[string]any{}}
			}
			if !send("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block}) {
				return false
			}
			open = index
		}
		switch d.Part.Kind {
		case wire.PartKindText:
			return send("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": d.Part.Text}})
		case wire.PartKindToolCall:
			if d.Part.Arguments == "" {
				return true
			}
			return send("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": d.Part.Arguments}})
		}
		return true
	})
	if !ok {
		return
	}
	closeOpen()
	if end.Outcome != wire.ReplyOutcomeCompleted {
		send("error", errorBody(status(end.Outcome.String()), end.Outcome.String(), refusalMessage(end.Outcome.String(), end.Reason)))
		return
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicStop(end.StopReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": end.Usage.Output, "input_tokens": end.Usage.Input - end.Usage.Cached}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
