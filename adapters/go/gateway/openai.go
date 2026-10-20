package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/client"
)

// errUnsupported is a request field the window cannot express in chat@1.
// plain, when set, is the message the client reads in place of the reason.
type errUnsupported struct{ feature, plain string }

func (e errUnsupported) Error() string { return "feature:" + e.feature }

// oaiModeled names the chat completions fields the window translates. Any
// other top-level field is ignored and named in one log line per request.
var oaiModeled = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true,
	"max_tokens": true, "max_completion_tokens": true, "temperature": true,
	"stop": true, "stream": true, "stream_options": true, "response_format": true, "n": true,
	"reasoning_effort": true,
}

// oaiIgnored returns the sorted names of the top-level fields of body that
// the window does not translate.
func oaiIgnored(body map[string]json.RawMessage) []string {
	var names []string
	for name := range body {
		if !oaiModeled[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// oaiToolChoice reads tool_choice. "auto" and an absent value keep the
// tools; "none" withholds them from the model. "required" and a named
// function are unsupported: chat@1 carries no tool choice.
func oaiToolChoice(raw json.RawMessage) (withhold bool, err error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	const accepted = `the gateway accepts tool_choice "auto" or "none"`
	var word string
	if json.Unmarshal(raw, &word) != nil {
		return false, errUnsupported{"openai/tool_choice:function", "tool_choice naming a function is unsupported: " + accepted}
	}
	switch word {
	case "auto":
		return false, nil
	case "none":
		return true, nil
	}
	return false, errUnsupported{"openai/tool_choice:" + word, fmt.Sprintf("tool_choice %q is unsupported: %s", word, accepted)}
}

type oaiRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	Tools               []oaiTool       `json:"tools"`
	MaxTokens           int64           `json:"max_tokens"`
	MaxCompletionTokens int64           `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ResponseFormat *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	N               *int64          `json:"n"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	ReasoningEffort *string         `json:"reasoning_effort"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// oaiText reads OpenAI content: a string, null, or an array of parts of which
// only text parts are expressible without a storage digest.
func oaiText(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("content is neither a string nor an array of parts")
	}
	var out []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, p.Text)
		case "image_url", "input_image", "image":
			return nil, errUnsupported{feature: inference.FeatureVision}
		default:
			return nil, fmt.Errorf("content part type %q", p.Type)
		}
	}
	return out, nil
}

func textParts(texts []string) []wire.Part {
	parts := make([]wire.Part, 0, len(texts))
	for _, t := range texts {
		parts = append(parts, wire.Part{Kind: wire.PartKindText, Text: t})
	}
	return parts
}

func milli(t float64) *wire.Temperature {
	return &wire.Temperature{Milli: int64(math.Round(t * 1000))}
}

// fromOpenAI translates a chat completions request into chat@1.
func fromOpenAI(in oaiRequest) (wire.Request, error) {
	req := wire.Request{Model: in.Model, Messages: []wire.Message{}}
	if in.N != nil && *in.N != 1 {
		return req, errUnsupported{feature: "openai/n"}
	}
	for _, m := range in.Messages {
		texts, err := oaiText(m.Content)
		if err != nil {
			return req, err
		}
		switch m.Role {
		case "system", "developer":
			req.Messages = append(req.Messages, wire.Message{Role: wire.RoleSystem, Parts: textParts(texts)})
		case "user":
			req.Messages = append(req.Messages, wire.Message{Role: wire.RoleUser, Parts: textParts(texts)})
		case "assistant":
			parts := textParts(texts)
			for _, call := range m.ToolCalls {
				parts = append(parts, wire.Part{Kind: wire.PartKindToolCall, CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
			}
			if len(parts) == 0 {
				parts = []wire.Part{{Kind: wire.PartKindText}}
			}
			req.Messages = append(req.Messages, wire.Message{Role: wire.RoleAssistant, Parts: parts})
		case "tool":
			req.Messages = append(req.Messages, wire.Message{Role: wire.RoleTool, Parts: []wire.Part{{Kind: wire.PartKindToolResult, CallID: m.ToolCallID, Text: strings.Join(texts, "")}}})
		default:
			return req, fmt.Errorf("message role %q", m.Role)
		}
	}
	withhold, err := oaiToolChoice(in.ToolChoice)
	if err != nil {
		return req, err
	}
	tools := in.Tools
	if withhold {
		tools = nil
	}
	for _, t := range tools {
		if t.Type != "function" {
			return req, fmt.Errorf("tool type %q", t.Type)
		}
		params := string(t.Function.Parameters)
		if params == "" || params == "null" {
			params = `{"type":"object","properties":{}}`
		}
		req.Tools = append(req.Tools, wire.Tool{Name: t.Function.Name, Description: t.Function.Description, Parameters: params})
	}
	options := wire.Options{MaxOutput: in.MaxTokens}
	if in.MaxCompletionTokens > 0 {
		options.MaxOutput = in.MaxCompletionTokens
	}
	if in.Temperature != nil {
		options.Temperature = milli(*in.Temperature)
	}
	if stop := bytes.TrimSpace(in.Stop); len(stop) > 0 && string(stop) != "null" {
		var one string
		if json.Unmarshal(stop, &one) == nil {
			options.Stop = []string{one}
		} else if err := json.Unmarshal(stop, &options.Stop); err != nil {
			return req, errors.New("stop is neither a string nor an array of strings")
		}
	}
	if f := in.ResponseFormat; f != nil {
		switch f.Type {
		case "", "text":
		case "json_object":
			// JSON mode is the schema every JSON object meets.
			options.JSONSchema = `{"type":"object"}`
		case "json_schema":
			if f.JSONSchema == nil || len(f.JSONSchema.Schema) == 0 {
				return req, errors.New("response_format json_schema without a schema")
			}
			options.JSONSchema = string(f.JSONSchema.Schema)
		default:
			return req, errUnsupported{feature: "openai/response_format:" + f.Type}
		}
	}
	if options.MaxOutput != 0 || options.Temperature != nil || len(options.Stop) > 0 || options.JSONSchema != "" {
		req.Options = &options
	}
	if e := in.ReasoningEffort; e != nil && *e != "" {
		// An optional extension: a wire that maps it sends it, and the rest
		// serve the request without it (INF-W7).
		req.Extensions = map[string]string{"openai/reasoning_effort": *e}
	}
	return req, nil
}

func openAIFinish(reason wire.StopReason) string {
	switch reason {
	case wire.StopReasonMaxOutput:
		return "length"
	case wire.StopReasonToolCalls:
		return "tool_calls"
	case wire.StopReasonContentFilter:
		return "content_filter"
	}
	return "stop"
}

func openAIUsage(u wire.Usage) map[string]any {
	out := map[string]any{"prompt_tokens": u.Input, "completion_tokens": u.Output, "total_tokens": u.Input + u.Output}
	if u.Cached > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.Cached}
	}
	return out
}

// badRequest answers a request the window cannot translate. An unsupported
// feature is recorded like a provider refusal of the same word.
func (w *Window) badRequest(rw http.ResponseWriter, b Bound, model string, err error) {
	var unsupported errUnsupported
	outcome, reason := "invalid", "window:"+err.Error()
	if errors.As(err, &unsupported) {
		outcome, reason = "unsupported_feature", unsupported.Error()
	}
	subject := b.Subject()
	w.record(inference.Record{Rung: b.Rung(), Account: subject.Account, Program: subject.Program, Model: model, Outcome: outcome, Reason: reason})
	answer := refusalOf(outcome, reason)
	if unsupported.plain != "" {
		answer.message = unsupported.plain
	}
	answer.write(rw)
}

func (w *Window) openAIChat(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	var body json.RawMessage
	if !w.readBody(rw, r, b, &body) {
		return
	}
	var in oaiRequest
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		writeError(rw, http.StatusBadRequest, "invalid", "the request body is not a chat completions request: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		writeError(rw, http.StatusBadRequest, "invalid", "the request body is not a JSON object: "+err.Error())
		return
	}
	req, err := fromOpenAI(in)
	if err != nil {
		w.badRequest(rw, b, in.Model, err)
		return
	}
	if ignored := oaiIgnored(fields); len(ignored) > 0 {
		// Names only: a field's value can carry the caller's data.
		log.Printf("inference gateway: chat completions request from %s for model %q ignored fields: %s",
			b.Subject().Program, in.Model, strings.Join(ignored, ", "))
	}
	admission := w.start(r, b, grant, req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		refusalOf(admission.Outcome.String(), admission.Reason).write(rw)
		return
	}
	id := "chatcmpl-" + admission.Operation
	created := time.Now().Unix()
	if !in.Stream {
		var fold client.Fold
		end, ok := w.observe(r, b, admission.Operation, func(d wire.Delta) bool { fold.Add(d); return true })
		if !ok {
			return
		}
		if end.Outcome != wire.ReplyOutcomeCompleted {
			refusalOf(end.Outcome.String(), end.Reason).write(rw)
			return
		}
		reply, _ := fold.Reply()
		message := map[string]any{"role": "assistant", "content": nil}
		var text strings.Builder
		var calls []map[string]any
		for _, part := range reply.Message.Parts {
			switch part.Kind {
			case wire.PartKindText:
				//unchecked: strings.Builder.WriteString never returns a non-nil error
				text.WriteString(part.Text)
			case wire.PartKindToolCall:
				calls = append(calls, map[string]any{"id": part.CallID, "type": "function", "function": map[string]any{"name": part.Name, "arguments": part.Arguments}})
			}
		}
		if text.Len() > 0 || len(calls) == 0 {
			message["content"] = text.String()
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		rw.Header().Set("Content-Type", "application/json")
		//unchecked: terminal write of the response; headers and status are already committed and this package has no logger to report a write failure to (matches writeError and the other Encode calls in this package)
		json.NewEncoder(rw).Encode(map[string]any{"id": id, "object": "chat.completion", "created": created, "model": reply.Model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": openAIFinish(reply.StopReason)}},
			"usage":   openAIUsage(reply.Usage)})
		return
	}
	flusher, _ := rw.(http.Flusher)
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	send := func(v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(rw, "data: %s\n\n", raw); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": admission.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	if !send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) {
		w.cfg.Chat.Cancel(b.Subject(), admission.Operation)
		return
	}
	toolIndex := map[int64]int{}
	end, ok := w.observe(r, b, admission.Operation, func(d wire.Delta) bool {
		if d.Kind != wire.DeltaKindPart || d.Part == nil {
			return true
		}
		switch d.Part.Kind {
		case wire.PartKindText:
			return send(chunk(map[string]any{"content": d.Part.Text}, nil))
		case wire.PartKindToolCall:
			index, seen := toolIndex[d.Index]
			call := map[string]any{"function": map[string]any{"arguments": d.Part.Arguments}}
			if !seen {
				index = len(toolIndex)
				toolIndex[d.Index] = index
				call["id"], call["type"] = d.Part.CallID, "function"
				call["function"].(map[string]any)["name"] = d.Part.Name
			}
			call["index"] = index
			return send(chunk(map[string]any{"tool_calls": []any{call}}, nil))
		}
		return true
	})
	if !ok {
		return
	}
	if end.Outcome != wire.ReplyOutcomeCompleted {
		send(refusalOf(end.Outcome.String(), end.Reason).body())
	} else {
		send(chunk(map[string]any{}, openAIFinish(end.StopReason)))
		if in.StreamOptions != nil && in.StreamOptions.IncludeUsage {
			send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": end.Model, "choices": []any{}, "usage": openAIUsage(end.Usage)})
		}
	}
	if _, err := fmt.Fprint(rw, "data: [DONE]\n\n"); err == nil && flusher != nil {
		flusher.Flush()
	}
}
