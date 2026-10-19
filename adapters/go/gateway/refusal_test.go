package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// endingChat admits every request and ends it with one partial text part and
// the terminal reply end.
type endingChat struct{ end wire.Reply }

func (*endingChat) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "op", Model: model}
}
func (c *endingChat) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	end := c.end
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, AtEnd: true, Next: 2, Deltas: []wire.Delta{
		{Sequence: 0, Kind: wire.DeltaKindPart, Index: 0, Part: &wire.Part{Kind: wire.PartKindText, Text: "partial"}},
		{Sequence: 1, Kind: wire.DeltaKindEnd, End: &end}}}
}
func (*endingChat) Cancel(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeEnded}
}

type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// chatError sends one chat request to the window's handler for path and
// returns the HTTP status and the error body, read from the response or from
// the stream's error event.
func chatError(t *testing.T, end wire.Reply, path string, stream bool) (int, errorEnvelope) {
	t.Helper()
	w := &Window{cfg: Config{Chat: &endingChat{end: end}}}
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"max_tokens":16`
	if stream {
		body += `,"stream":true`
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body+"}"))
	rw := httptest.NewRecorder()
	b := fixedBound{inference.Subject{Account: "account", Program: "/apps/opencode"}}
	if path == "/v1/messages" {
		w.anthropicMessages(rw, r, b, Grant{})
	} else {
		w.openAIChat(rw, r, b, Grant{})
	}
	raw := rw.Body.String()
	if stream {
		raw = ""
		scanner := bufio.NewScanner(strings.NewReader(rw.Body.String()))
		previous := ""
		for scanner.Scan() {
			line, ok := strings.CutPrefix(scanner.Text(), "data: ")
			if ok && strings.Contains(line, `"error"`) {
				raw = line
				// Anthropic clients dispatch on the event name before reading data.
				if path == "/v1/messages" && previous != "event: error" {
					t.Errorf("%s: the error data follows %q, want event: error", path, previous)
				}
			}
			previous = scanner.Text()
		}
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("%s stream=%v: no error body in %q", path, stream, rw.Body.String())
	}
	return rw.Code, env
}

// A context overflow reaches an OpenAI-compatible or Anthropic client as a
// 400 invalid_request_error with the code context_length_exceeded, in a plain
// answer and in a stream's error event. The message names the window and the
// input when they are known and carries nothing else from the reason.
func TestAContextOverflowReachesTheClientAsAnInvalidRequest(t *testing.T) {
	cases := []struct {
		reason string
		want   []string
	}{
		{"upstream:context_length_exceeded:4096:8219", []string{"8219", "4096"}},
		{"upstream:context_length_exceeded:4096:0", []string{"4096"}},
		{"upstream:context_length_exceeded:0:8219", []string{"8219"}},
		{"upstream:context_length_exceeded:0:0", nil},
	}
	for _, c := range cases {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				code, env := chatError(t, wire.Reply{Outcome: wire.ReplyOutcomeRefused, Reason: c.reason, Host: "ollama", Model: model}, path, stream)
				if !stream && code != http.StatusBadRequest {
					t.Errorf("%s %s: status %d, want 400", c.reason, path, code)
				}
				if env.Type != "error" || env.Error.Type != "invalid_request_error" || env.Error.Code != "context_length_exceeded" {
					t.Errorf("%s %s stream=%v: body %+v", c.reason, path, stream, env)
				}
				if !strings.Contains(env.Error.Message, "exceeds the context window") || strings.Contains(env.Error.Message, "upstream") {
					t.Errorf("%s %s stream=%v: message %q", c.reason, path, stream, env.Error.Message)
				}
				for _, n := range c.want {
					if !strings.Contains(env.Error.Message, n) {
						t.Errorf("%s %s stream=%v: message %q does not name %s", c.reason, path, stream, env.Error.Message, n)
					}
				}
				if len(c.want) == 0 && strings.ContainsAny(env.Error.Message, "0123456789") {
					t.Errorf("%s %s stream=%v: message %q names an unknown number", c.reason, path, stream, env.Error.Message)
				}
			}
		}
	}
}

// A model server's other refusals keep INF-W7's 502 and the outcome word.
func TestAnotherUpstreamRefusalStaysABadGateway(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		code, env := chatError(t, wire.Reply{Outcome: wire.ReplyOutcomeRefused, Reason: "upstream:500"}, path, false)
		if code != http.StatusBadGateway || env.Error.Code != "refused" || env.Error.Type != "api_error" {
			t.Errorf("%s: status %d body %+v", path, code, env)
		}
	}
}

// The overflow answer is decided once for every route that answers a refused
// reply, embeddings included.
func TestRefusalAnswersAnOverflowOnEveryRoute(t *testing.T) {
	got := refusalOf("refused", "upstream:context_length_exceeded:8192:9000")
	if got.status != http.StatusBadRequest || got.code != "context_length_exceeded" {
		t.Fatalf("refusal %+v", got)
	}
	for _, malformed := range []string{"upstream:context_length_exceeded:", "upstream:context_length_exceeded:a:b", "upstream:context_length_exceeded:1:2:3", "upstream:context_length_exceeded:-1:2"} {
		if got := refusalOf("refused", malformed); got.status != http.StatusBadGateway || got.code != "refused" {
			t.Errorf("%q: refusal %+v", malformed, got)
		}
	}
	if got := refusalOf("unavailable", "upstream:context_length_exceeded:1:2"); got.status != http.StatusServiceUnavailable {
		t.Errorf("an overflow reason under another outcome: %+v", got)
	}
}
