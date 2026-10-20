package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// chat3ReplayAudit reports a whole omitted replay block. It carries no opaque
// bytes or displayed reasoning text.
type chat3ReplayAudit struct {
	Message int
	Block   int64
	Reason  wire.SealOmission
}

type chat3ReplayContextKey struct{}
type chat3ReplayInput struct {
	messages []wire.ChatMessage
	audit    func(chat3ReplayAudit)
}

// withChat3Replay carries the original chat@3 history alongside its legacy
// projection until the selected upstream request is encoded. The caller owns
// admission and keeps messages immutable for the request lifetime.
func withChat3Replay(ctx context.Context, messages []wire.ChatMessage, audit func(chat3ReplayAudit)) context.Context {
	return context.WithValue(ctx, chat3ReplayContextKey{}, chat3ReplayInput{messages: messages, audit: audit})
}

func chat3ReplayOmission(seal wire.ReasoningSeal, origin string) *wire.SealOmission {
	reason := wire.SealOmissionIncompatibleOrigin
	if seal.Origin != origin {
		return &reason
	}
	if len(seal.Bytes) > chat3SealBytes || len(seal.Origin) > 256 {
		reason = wire.SealOmissionEncodedSize
		return &reason
	}
	// The string wire cannot carry invalid UTF-8 byte for byte. Anthropic
	// replay is already a complete JSON block produced by the seal owner.
	if !utf8.Valid(seal.Bytes) {
		return &reason
	}
	if origin == router.WireAnthropicMessages {
		var block struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(seal.Bytes, &block) != nil || block.Type != "thinking" && block.Type != "redacted_thinking" {
			return &reason
		}
	}
	return nil
}

// chat3ApplyReplay follows the request projection's message order. Providers
// receive only opaque seals; public reasoning text never enters this path.
func chat3ApplyReplay(ctx context.Context, origin string, req wire.Request, body map[string]any, images map[string][]byte) {
	input, ok := ctx.Value(chat3ReplayContextKey{}).(chat3ReplayInput)
	if !ok {
		return
	}
	var messages []map[string]any
	for i, message := range req.Messages {
		one := req
		one.Messages = []wire.Message{message}
		var projected []map[string]any
		if origin == router.WireOpenAICompatible {
			projected, _ = openAIBody("", one, images)["messages"].([]map[string]any)
		} else {
			projected, _ = anthropicBody("", one, images)["messages"].([]map[string]any)
		}
		if i >= len(input.messages) || len(input.messages[i].Reasoning) == 0 {
			messages = append(messages, projected...)
			continue
		}
		var replay []wire.ChatReplay
		for _, item := range input.messages[i].Reasoning {
			why := chat3ReplayOmission(item.Seal, origin)
			if message.Role != wire.RoleAssistant || input.messages[i].Role != wire.RoleAssistant {
				reason := wire.SealOmissionIncompatibleOrigin
				why = &reason
			}
			if why != nil {
				if input.audit != nil {
					input.audit(chat3ReplayAudit{Message: i, Block: item.Block, Reason: *why})
				}
				continue
			}
			replay = append(replay, item)
		}
		if len(replay) == 0 {
			messages = append(messages, projected...)
			continue
		}
		if len(projected) == 0 {
			projected = []map[string]any{{"role": "assistant", "content": nil}}
		}
		assistant := projected[len(projected)-1]
		if origin == router.WireOpenAICompatible {
			// OpenAI has one reasoning field per assistant message. Adjacent
			// opaque blocks retain their upstream order in that field.
			var raw []byte
			for _, item := range replay {
				raw = append(raw, item.Seal.Bytes...)
			}
			if len(raw) > chat3SealBytes {
				for _, item := range replay {
					if input.audit != nil {
						input.audit(chat3ReplayAudit{Message: i, Block: item.Block, Reason: wire.SealOmissionEncodedSize})
					}
				}
			} else {
				assistant["reasoning_content"] = string(raw)
			}
		} else {
			blocks := make([]any, 0, len(replay))
			for _, item := range replay {
				blocks = append(blocks, chat3ReplayRaw(item.Seal.Bytes))
			}
			if content, ok := assistant["content"].([]map[string]any); ok {
				for _, block := range content {
					blocks = append(blocks, block)
				}
			}
			assistant["content"] = blocks
		}
		messages = append(messages, projected...)
	}
	body["messages"] = messages
}

// chat3ReplayRaw is already validated JSON. Its lexical bytes belong to the
// originating provider, including escapes and whitespace.
type chat3ReplayRaw []byte

// chat3ReplayEncoding uses the ordinary JSON encoder for ordinary fields and
// writes opaque replay blocks directly. encoding/json compacts RawMessage.
func chat3ReplayEncoding(v any) ([]byte, bool, error) {
	var out bytes.Buffer
	raw := false
	var write func(any) error
	write = func(v any) error {
		switch value := v.(type) {
		case chat3ReplayRaw:
			raw = true
			out.Write(value)
		case map[string]any:
			out.WriteByte('{')
			first := true
			for k, item := range value {
				if !first {
					out.WriteByte(',')
				}
				first = false
				key, _ := json.Marshal(k)
				out.Write(key)
				out.WriteByte(':')
				if err := write(item); err != nil {
					return err
				}
			}
			out.WriteByte('}')
		case []map[string]any:
			out.WriteByte('[')
			for i, item := range value {
				if i > 0 {
					out.WriteByte(',')
				}
				if err := write(item); err != nil {
					return err
				}
			}
			out.WriteByte(']')
		case []any:
			out.WriteByte('[')
			for i, item := range value {
				if i > 0 {
					out.WriteByte(',')
				}
				if err := write(item); err != nil {
					return err
				}
			}
			out.WriteByte(']')
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				return err
			}
			out.Write(encoded)
		}
		return nil
	}
	err := write(v)
	return out.Bytes(), raw, err
}

// chat3Post retains the shared post owner for ordinary requests. Opaque JSON
// replay requires sending its original lexical bytes; HTTP credential and
// status handling follows post's existing boundary.
func chat3Post(ctx context.Context, client *http.Client, address string, body map[string]any, headers map[string]string) (*http.Response, *wire.Reply) {
	raw, opaque, err := chat3ReplayEncoding(body)
	if !opaque {
		return post(ctx, client, address, body, headers)
	}
	if err != nil {
		r := failed(wire.ReplyOutcomeInvalid, "upstream:encoding")
		return nil, &r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		r := failed(wire.ReplyOutcomeUnavailable, "upstream:address")
		return nil, &r
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	generationSent(ctx)
	resp, err := credentialRequestClient(client, headers).Do(req)
	if err != nil {
		r := failed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
		return nil, &r
	}
	postResponse(ctx, resp)
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	//unchecked: a failed bounded read retains the HTTP-status refusal
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	//unchecked: cleanup before returning the status-code outcome
	resp.Body.Close()
	window, input, overflow := contextOverflow(b)
	clear(b)
	if overflow && resp.StatusCode != http.StatusTooManyRequests {
		r := overflowed(window, input)
		return nil, &r
	}
	outcome := wire.ReplyOutcomeRefused
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		outcome = wire.ReplyOutcomeUnavailable
	}
	r := failed(outcome, "upstream:"+strconv.Itoa(resp.StatusCode))
	return nil, &r
}
