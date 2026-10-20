package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	inference "github.com/openabstractions/abstraction-inference/go"
)

// fieldsWindow is the fixture window over the real provider and the fake
// Ollama host, with every connection bound to one permitted program.
func fieldsWindow(t *testing.T) *fixture {
	t.Helper()
	const program = "/apps/fields-client"
	subject := inference.Subject{Account: account(t), Program: program}
	f := setup(t, func(net.Conn, time.Time) (Bound, error) { return fixedBound{subject}, nil })
	f.grant("oalk-fields", program, true)
	return f
}

// postChat sends one chat completions body and returns the status and the
// decoded response.
func postChat(t *testing.T, f *fixture, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer oalk-fields")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("status %d, body %q is not JSON", resp.StatusCode, raw)
	}
	return resp.StatusCode, out
}

// captureLog collects the standard logger's output for one test; serve
// routes it to the host log.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	saved, flags := log.Writer(), log.Flags()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(saved); log.SetFlags(flags) })
	return &out
}

const toolsBody = `{"model":"` + model + `","messages":[{"role":"user","content":"edit a.py"}],` +
	`"tools":[{"type":"function","function":{"name":"replace_lines","parameters":{"type":"object","properties":{}}}}]`

// tool_choice "required" or a named function changes what the model must do
// and chat@1 cannot carry it: the window refuses it before admission with a
// 400 in the OpenAI error shape and a plain message, and records the refusal.
func TestToolChoiceTheWindowCannotCarryIsRefused(t *testing.T) {
	f := fieldsWindow(t)
	cases := []struct{ choice, reason, says string }{
		{`"required"`, "feature:openai/tool_choice:required", `tool_choice "required" is unsupported`},
		{`{"type":"function","function":{"name":"replace_lines"}}`, "feature:openai/tool_choice:function", "tool_choice naming a function is unsupported"},
	}
	for _, c := range cases {
		code, out := postChat(t, f, toolsBody+`,"tool_choice":`+c.choice+`}`)
		e, _ := out["error"].(map[string]any)
		if code != http.StatusBadRequest || e["type"] != "invalid_request_error" || e["code"] != "unsupported_feature" {
			t.Fatalf("%s: status %d body %v", c.choice, code, out)
		}
		message, _ := e["message"].(string)
		if !strings.HasPrefix(message, c.says) || !strings.Contains(message, `"auto" or "none"`) || strings.Contains(message, "feature:") {
			t.Fatalf("%s: message %q", c.choice, message)
		}
		records := f.waitLogged(t, 1)
		last := records[len(records)-1]
		if last.Outcome != "unsupported_feature" || last.Reason != c.reason || last.Route != inference.RouteWindow {
			t.Fatalf("%s: record %+v", c.choice, last)
		}
	}
	if f.up.chats.Load() != 0 {
		t.Fatalf("a refused tool_choice reached the model server %d times", f.up.chats.Load())
	}
}

// tool_choice "none" withholds the tools from the model, and "auto" keeps
// them.
func TestToolChoiceAutoAndNoneAreServed(t *testing.T) {
	f := fieldsWindow(t)
	code, out := postChat(t, f, toolsBody+`,"tool_choice":"none"}`)
	choice := out["choices"].([]any)[0].(map[string]any)
	if code != http.StatusOK || choice["finish_reason"] != "stop" || choice["message"].(map[string]any)["content"] != "Hello from the window" {
		t.Fatalf("tool_choice none: status %d body %v", code, out)
	}
	code, out = postChat(t, f, toolsBody+`,"tool_choice":"auto"}`)
	choice = out["choices"].([]any)[0].(map[string]any)
	if code != http.StatusOK || choice["finish_reason"] != "tool_calls" {
		t.Fatalf("tool_choice auto: status %d body %v", code, out)
	}
}

// Fields the window does not translate are served without them, and one log
// line per request names them without their values.
func TestUnmodeledFieldsAreServedAndNamedInOneLogLine(t *testing.T) {
	f := fieldsWindow(t)
	logged := captureLog(t)
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],` +
		`"top_p":0.31415,"seed":271828,"presence_penalty":0.5,"frequency_penalty":0.25,"logprobs":true,"n":1,"temperature":0.2}`
	code, out := postChat(t, f, body)
	if code != http.StatusOK || out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "Hello from the window" {
		t.Fatalf("status %d body %v", code, out)
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines %q, want one", lines)
	}
	if !strings.Contains(lines[0], "ignored fields: frequency_penalty, logprobs, presence_penalty, seed, top_p") ||
		!strings.Contains(lines[0], "/apps/fields-client") {
		t.Fatalf("log line %q", lines[0])
	}
	for _, value := range []string{"0.31415", "271828", "0.25", "true"} {
		if strings.Contains(lines[0], value) {
			t.Fatalf("log line %q carries the value %s", lines[0], value)
		}
	}

	logged.Reset()
	if code, _ := postChat(t, f, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`); code != http.StatusOK {
		t.Fatalf("modeled-only request: status %d", code)
	}
	if logged.Len() != 0 {
		t.Fatalf("a request with only modeled fields logged %q", logged.String())
	}
}

// n other than 1 stays refused as INF-W7 says.
func TestNOtherThanOneStaysUnsupported(t *testing.T) {
	f := fieldsWindow(t)
	code, out := postChat(t, f, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"n":2}`)
	if e, _ := out["error"].(map[string]any); code != http.StatusBadRequest || e["code"] != "unsupported_feature" {
		t.Fatalf("n=2: status %d body %v", code, out)
	}
}

// response_format json_object reaches the model server as the schema every
// JSON object meets, and reasoning_effort reaches it as the request field
// through the openai/reasoning_effort extension. Neither is logged as
// ignored.
func TestJSONObjectAndReasoningEffortReachTheModelServer(t *testing.T) {
	f := fieldsWindow(t)
	logged := captureLog(t)
	code, out := postChat(t, f, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],`+
		`"response_format":{"type":"json_object"},"reasoning_effort":"low"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, out)
	}
	var sent struct {
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	last, _ := f.up.last.Load().(string)
	if err := json.Unmarshal([]byte(last), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ResponseFormat.Type != "json_schema" || sent.ResponseFormat.JSONSchema.Schema["type"] != "object" {
		t.Fatalf("response_format sent upstream: %s", last)
	}
	if sent.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort sent upstream: %s", last)
	}
	if logged.Len() != 0 {
		t.Fatalf("modeled fields logged as ignored: %q", logged.String())
	}
}
