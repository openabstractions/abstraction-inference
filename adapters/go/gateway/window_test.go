package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const model = "fixture-chat:1b"

// upstream is a fake local Ollama host. It lists model, streams "Hello from
// the window" one word per event, streams a replace_lines tool call when the
// request offers tools and holds no tool result, and answers "Edited" after a
// tool result. It counts chat requests.
type upstream struct {
	server *httptest.Server
	chats  atomic.Int64
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"models":[{"name":"` + model + `"}]}`))
	})
	mux.HandleFunc("/api/ps", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"models":[]}`)) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		u.chats.Add(1)
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		send := func(data string) { fmt.Fprintf(w, "data: %s\n\n", data); flusher.Flush() }
		switch {
		case strings.Contains(body, `"tools"`) && !strings.Contains(body, `"role":"tool"`):
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"replace_lines","arguments":"{\"path\":"}}]}}]}`)
			send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.py\"}"}}]}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`)
		case strings.Contains(body, `"role":"tool"`):
			send(`{"choices":[{"delta":{"content":"Edited"}}]}`)
			send(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1}}`)
		default:
			for _, word := range strings.SplitAfter("Hello from the window", " ") {
				send(fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, word))
			}
			send(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`)
		}
		send("[DONE]")
	})
	u.server = httptest.NewServer(mux)
	t.Cleanup(u.server.Close)
	return u
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// fixture is a provider over the fake host and a window over the provider. Keys
// maps a key to the program it was issued to; permitted programs hold
// abstraction.inference/complete on host:ollama.
type fixture struct {
	up       *upstream
	window   *Window
	base     string
	mu       sync.Mutex
	keys     map[string]string
	allowed  map[string]bool
	records  []inference.Record
	recorded chan struct{}
	// decided holds the peer each rights decision carried.
	decided []*identity.Peer
}

func (f *fixture) grant(key, program string, complete bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key] = program
	f.allowed[strings.ToLower(program)] = complete
}

func (f *fixture) logged() []inference.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]inference.Record(nil), f.records...)
}

func (f *fixture) waitLogged(t *testing.T, count int) []inference.Record {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if records := f.logged(); len(records) >= count {
			return records
		}
		select {
		case <-f.recorded:
		case <-timer.C:
			records := f.logged()
			if len(records) >= count {
				return records
			}
			t.Fatalf("audit callback did not record %d entries: %+v", count, records)
		}
	}
}

func account(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Uid
}

func setup(t *testing.T, bind Binder) *fixture {
	t.Helper()
	f := &fixture{up: newUpstream(t), keys: map[string]string{}, allowed: map[string]bool{}, recorded: make(chan struct{}, 64)}
	r := router.New(router.Ollama(f.up.server.URL))
	r.Survey()
	record := func(rec inference.Record) {
		f.mu.Lock()
		f.records = append(f.records, rec)
		f.mu.Unlock()
		select {
		case f.recorded <- struct{}{}:
		default:
		}
	}
	provider, err := inference.New(inference.Config{Router: r, SurveyAge: time.Hour, Record: record,
		Decide: func(ctx context.Context, s inference.Subject, action, resource string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.decided = append(f.decided, inference.PeerFromContext(ctx))
			if action == inference.ActionComplete && resource == "host:ollama" && f.allowed[strings.ToLower(s.Program)] {
				return "permitted", nil
			}
			return "not_granted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "unknown"
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Close() })
	cfg := Config{Chat: provider, Account: account(t), Bind: bind, Record: record,
		Keys: func(_ context.Context, s inference.Subject, key string) (Grant, string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			program, ok := f.keys[key]
			switch {
			case !ok:
				return Grant{}, KeyUnknown
			case !samePath(program, s.Program):
				return Grant{}, KeyWrongProgram
			}
			return Grant{}, KeyVerified
		},
		Models:  func(context.Context, inference.Subject) []string { return []string{model} },
		OnError: func(err error) { t.Log(err) }}
	f.window, err = Listen("127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	go f.window.Serve(context.Background())
	t.Cleanup(func() { f.window.Close() })
	f.base = "http://" + f.window.Addr().String()
	return f
}

// python finds an interpreter and the real path of the image a script runs
// under, which is the program the window binds.
func python(t *testing.T) (string, []string, string) {
	t.Helper()
	candidates := [][]string{{"python3"}, {"python"}}
	if runtime.GOOS == "windows" {
		candidates = [][]string{{"py", "-3"}, {"python"}}
	}
	for _, c := range candidates {
		exe, err := exec.LookPath(c[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(exe, append(c[1:], "-c", "import os,sys;print(os.path.realpath(sys.executable))")...).Output()
		if err != nil {
			continue
		}
		return exe, c[1:], strings.TrimSpace(string(out))
	}
	t.Skip("no Python interpreter on PATH for the fake clients")
	return "", nil, ""
}

func runScript(t *testing.T, script string, args ...string) (map[string]any, int) {
	t.Helper()
	exe, prefix, _ := python(t)
	cmd := exec.Command(exe, append(append(prefix, filepath.Join("testdata", script)), args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if jerr := json.Unmarshal(out, &result); jerr != nil {
		t.Fatalf("%s printed %q (exit %d)", script, out, code)
	}
	return result, code
}

func realBinding(t *testing.T) Binder {
	a := account(t)
	return func(c net.Conn, at time.Time) (Bound, error) { return BindLoopback(c, at, a) }
}

// gatewayTransportProvesProgram measures the same loopback binding required by
// protected window calls. Darwin documents ErrNoBinding for this transport; a
// future transport that proves Program automatically runs the full assertions.
func gatewayTransportProvesProgram(t *testing.T) bool {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connectedAt := time.Now()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	bound, err := BindLoopback(server, connectedAt, account(t))
	if err == nil {
		bound.Close()
		return true
	}
	if runtime.GOOS == "darwin" && errors.Is(err, identity.ErrNoBinding) {
		t.Logf("measured Program proof refusal: %v", err)
		return false
	}
	t.Fatalf("unexpected transport proof failure: %v", err)
	return false
}

// An llm-style streaming client reaches the fake host through the window
// under its own key, and the audit records the window route and the socket
// rung of the program the window bound.
func TestLLMStyleClientStreamsThroughTheWindow(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	_, _, program := python(t)
	f.grant("oalk-llm", program, true)
	result, code := runScript(t, "llm_style.py", f.base+"/v1", "oalk-llm", model)
	if code != 0 || result["text"] != "Hello from the window" || result["finish_reason"] != "stop" {
		t.Fatalf("llm-style client: exit %d %v", code, result)
	}
	usage, _ := result["usage"].(map[string]any)
	if usage["prompt_tokens"] != 5.0 || usage["completion_tokens"] != 4.0 {
		t.Fatalf("usage %v", usage)
	}
	records := f.waitLogged(t, 1)
	last := records[len(records)-1]
	if last.Route != inference.RouteWindow || last.Outcome != "completed" || !samePath(last.Program, program) || last.TokensOut != 4 {
		t.Fatalf("audit record %+v", last)
	}
	want := identity.TransportLoopback + "/" + runtime.GOOS + " user="
	if !strings.HasPrefix(last.Rung, want) || !strings.Contains(last.Rung, "path=bound") {
		t.Fatalf("audit rung %q, want %q... path=bound", last.Rung, want)
	}
	t.Logf("audit: route=%s rung=%q program=%s outcome=%s", last.Route, last.Rung, last.Program, last.Outcome)
}

// An aider-style client lists models, takes a tool call from a non-streaming
// completion and sends the tool's result back.
func TestAiderStyleClientCallsAToolThroughTheWindow(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	_, _, program := python(t)
	f.grant("oalk-aider", program, true)
	result, code := runScript(t, "aider_style.py", f.base+"/v1", "oalk-aider", model)
	if code != 0 {
		t.Fatalf("aider-style client: exit %d %v", code, result)
	}
	calls, _ := result["tool_calls"].([]any)
	if fmt.Sprint(result["models"]) != "["+model+"]" || result["first_finish"] != "tool_calls" || len(calls) != 1 ||
		fmt.Sprint(calls[0]) != "map[arguments:map[path:a.py] id:call_1 name:replace_lines]" ||
		result["text"] != "Edited" || result["finish_reason"] != "stop" {
		t.Fatalf("aider-style client: %v", result)
	}
}

// A client on the Anthropic wire streams named events through the window.
func TestAnthropicStyleClientStreamsThroughTheWindow(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	_, _, program := python(t)
	f.grant("oalk-anthropic", program, true)
	result, code := runScript(t, "anthropic_style.py", f.base, "oalk-anthropic", model)
	events := fmt.Sprint(result["events"])
	if code != 0 || result["text"] != "Hello from the window" || result["stop_reason"] != "end_turn" || result["output_tokens"] != 4.0 ||
		!strings.HasPrefix(events, "[message_start content_block_start content_block_delta") || !strings.HasSuffix(events, "content_block_stop message_delta message_stop]") {
		t.Fatalf("anthropic-style client: exit %d %v", code, result)
	}
}

// A program with a key but no complete rule is refused by the provider's
// rights decision, and the fake host sees nothing.
func TestAKeyWithoutACompleteRuleIsNotPermitted(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	_, _, program := python(t)
	f.grant("oalk-norule", program, false)
	result, code := runScript(t, "llm_style.py", f.base+"/v1", "oalk-norule", model)
	if code != 3 || result["status"] != 403.0 || !strings.Contains(fmt.Sprint(result["body"]), "rights:not_granted") || f.up.chats.Load() != 0 {
		t.Fatalf("no rule: exit %d %v, upstream chats %d", code, result, f.up.chats.Load())
	}
}

// The rights decision for a window request carries the peer the window bound
// (inference.WithPeer), with the code verdict the binding verified, so a
// first-use question the refusal admits names the caller's publisher
// (ASK-A6). Outside Windows a loopback peer carries no code evidence (ID-T4).
func TestTheWindowDecidesWithTheBoundPeersCode(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	_, _, program := python(t)
	f.grant("oalk-peer", program, false)
	if result, code := runScript(t, "llm_style.py", f.base+"/v1", "oalk-peer", model); code != 3 || result["status"] != 403.0 {
		t.Fatalf("no rule: exit %d %v", code, result)
	}
	f.mu.Lock()
	decided := append([]*identity.Peer(nil), f.decided...)
	f.mu.Unlock()
	if len(decided) == 0 || slices.Contains(decided, nil) {
		t.Fatalf("the window decided with peers %v", decided)
	}
	bound, err := LoopbackSubject(decided[0])
	if err != nil || !samePath(bound.Program, program) || bound.Account != account(t) {
		t.Fatalf("the decision's peer binds %+v, the request came from %q: %v", bound, program, err)
	}
	if runtime.GOOS == "windows" {
		if code, proof := decided[0].Code.Get(); proof <= identity.ProofNone {
			t.Fatalf("the window bound no code evidence: %s (%+v)", decided[0].Code.Why(), code)
		}
	}
}

// rawRequest sends a request's head announcing a body it never sends, and
// reads the response. A window that read the body would wait for it, and this
// read would time out.
func rawRequest(t *testing.T, address, head string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if head != "" {
		if _, err := io.WriteString(c, head); err != nil {
			t.Fatal(err)
		}
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no response before the body was sent: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

const bodilessHead = "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n"

// A peer the window cannot bind is answered 403 at accept, before its request
// is read, and the audit records the refusal and the unbound rung.
func TestAnUnboundPeerIsRefusedBeforeTheBody(t *testing.T) {
	f := setup(t, func(net.Conn, time.Time) (Bound, error) {
		return nil, fmt.Errorf("%w: fixture binder", identity.ErrNoBinding)
	})
	status, body := rawRequest(t, f.window.Addr().String(), "")
	if status != http.StatusForbidden || !strings.Contains(body, "no_binding") {
		t.Fatalf("unbound peer: %d %s", status, body)
	}
	records := f.waitLogged(t, 1)
	if len(records) != 1 || records[0].Reason != "binding:no_binding" || records[0].Route != inference.RouteWindow ||
		records[0].Rung != identity.TransportLoopback+"/"+runtime.GOOS+" unbound" || f.up.chats.Load() != 0 {
		t.Fatalf("audit %+v, upstream chats %d", records, f.up.chats.Load())
	}
}

// A bound peer presenting a key minted for another program is refused before
// its body is read, and so is a peer presenting no key.
func TestAWrongProgramPeerIsRefusedBeforeTheBody(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	other := filepath.Join(filepath.Dir(os.Args[0]), "someone-else")
	f.grant("oalk-other", other, true)
	status, body := rawRequest(t, f.window.Addr().String(), fmt.Sprintf(bodilessHead, "oalk-other"))
	if status != http.StatusForbidden || !strings.Contains(body, "another program") {
		t.Fatalf("wrong program: %d %s", status, body)
	}
	status, _ = rawRequest(t, f.window.Addr().String(), strings.Replace(fmt.Sprintf(bodilessHead, ""), "Authorization: Bearer \r\n", "", 1))
	if status != http.StatusUnauthorized {
		t.Fatalf("no key: %d", status)
	}
	records := f.waitLogged(t, 2)
	if len(records) != 2 || records[0].Reason != "key:wrong_program" || records[1].Reason != "key:missing" || f.up.chats.Load() != 0 {
		t.Fatalf("audit %+v", records)
	}
	self, _ := os.Executable()
	if !strings.HasPrefix(records[0].Rung, identity.TransportLoopback+"/") || filepath.Base(records[0].Program) != filepath.Base(self) {
		t.Fatalf("the refusal names %q at %q; this test is %q", records[0].Program, records[0].Rung, self)
	}
}

// movedBound is a binding whose peer moves after the first recheck.
type movedBound struct {
	subject inference.Subject
	checks  atomic.Int64
}

func (m *movedBound) Subject() inference.Subject { return m.subject }
func (m *movedBound) Rung() string               { return "tcp-loopback/fixture user=bound process=bound path=bound" }
func (m *movedBound) Peer() *identity.Peer       { return nil }
func (m *movedBound) Close() error               { return nil }
func (m *movedBound) Recheck() error {
	if m.checks.Add(1) > 1 {
		return fmt.Errorf("%w: fixture", identity.ErrPeerMoved)
	}
	return nil
}

// A connection whose peer moves between the key check and the body is refused
// before anything is admitted.
func TestAPeerThatMovesBeforeAdmissionIsRefused(t *testing.T) {
	self, _ := os.Executable()
	var f *fixture
	f = setup(t, func(net.Conn, time.Time) (Bound, error) {
		return &movedBound{subject: inference.Subject{Account: account(t), Program: self}}, nil
	})
	f.grant("oalk-self", self, true)
	req, _ := http.NewRequest(http.MethodPost, f.base+"/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer oalk-self")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	records := f.waitLogged(t, 1)
	if resp.StatusCode != http.StatusForbidden || len(records) != 1 || records[0].Reason != "binding:peer_moved" || f.up.chats.Load() != 0 {
		t.Fatalf("moved peer: %d %+v", resp.StatusCode, records)
	}
}

type nopChat struct{}

func (nopChat) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{}
}
func (nopChat) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (nopChat) Cancel(inference.Subject, string) wire.Cancellation { return wire.Cancellation{} }

// The window refuses to listen anywhere but IPv4 loopback.
func TestTheWindowListensOnLoopbackOnly(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "localhost:0", "[::1]:0", "192.168.1.2:0"} {
		if w, err := Listen(address, Config{Chat: nopChat{}, Keys: func(context.Context, inference.Subject, string) (Grant, string) { return Grant{}, KeyUnknown }, Account: "a"}); err == nil {
			w.Close()
			t.Fatalf("listened on %s", address)
		}
	}
}

// A connection whose binding is slow is bound off the accept loop: while the
// first connection's code verification is still running, a second
// connection is accepted, bound and served.
func TestASlowBindDoesNotHoldOtherConnections(t *testing.T) {
	self, _ := os.Executable()
	subject := inference.Subject{Account: account(t), Program: self}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int64
	f := setup(t, func(net.Conn, time.Time) (Bound, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return fixedBound{subject: subject}, nil
	})
	// Registered after setup, so it runs before the window closes.
	t.Cleanup(unblock)
	f.grant("oalk-self", self, true)
	slow, err := net.Dial("tcp4", f.window.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	if _, err := io.WriteString(slow, "GET /v1/models HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer oalk-self\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first connection was never bound")
	}
	req, _ := http.NewRequest(http.MethodGet, f.base+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer oalk-self")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("a second connection waited for the first one's binding: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second connection: %d", resp.StatusCode)
	}
	unblock()
	slow.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := http.ReadResponse(bufio.NewReader(slow), nil)
	if err != nil {
		t.Fatalf("the slow connection was not served once bound: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("slow connection: %d", first.StatusCode)
	}
}

// A request that reaches the handler on a connection whose binding failed is
// refused with the binding's word, recorded once, and starts nothing.
func TestARequestOnAConnectionWhoseBindFailedIsRefused(t *testing.T) {
	chat := &countingChat{}
	var records []inference.Record
	var mu sync.Mutex
	w, err := Listen("127.0.0.1:0", Config{Chat: chat, Account: "a",
		Keys: func(context.Context, inference.Subject, string) (Grant, string) { return Grant{}, KeyVerified },
		Bind: func(net.Conn, time.Time) (Bound, error) {
			return nil, fmt.Errorf("%w: fixture binder", identity.ErrPeerGone)
		},
		Record: func(r inference.Record) { mu.Lock(); records = append(records, r); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	client, err := net.Dial("tcp4", w.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := w.listener.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	var c net.Conn
	select {
	case c = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept bound the connection instead of handing it to its own goroutine")
	}
	defer c.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer oalk-any")
	rec := httptest.NewRecorder()
	w.serveHTTP(rec, req.WithContext(w.server.ConnContext(req.Context(), c)))
	mu.Lock()
	defer mu.Unlock()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "peer_gone") || chat.starts.Load() != 0 ||
		len(records) != 1 || records[0].Reason != "binding:peer_gone" {
		t.Fatalf("refused bind: %d %s, starts %d, records %+v", rec.Code, rec.Body.String(), chat.starts.Load(), records)
	}
}

type countingChat struct {
	nopChat
	starts atomic.Int64
}

func (c *countingChat) Start(ctx context.Context, s inference.Subject, r wire.Request) wire.Admission {
	c.starts.Add(1)
	return c.nopChat.Start(ctx, s, r)
}

// A keep-alive connection whose code verdict has outlived its five minutes
// is verified again before its next decision, through the identity layer's
// own binding.
func TestAConnectionPastItsEvidenceLifetimeIsVerifiedAgain(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connectedAt := time.Now()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	bound, err := BindLoopback(server, connectedAt, account(t))
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	lb := bound.(*loopbackBound)
	first, subject := lb.binding, bound.Subject()
	until := first.EvidenceValidUntil()
	if until.IsZero() {
		t.Skip("this platform records no evidence lifetime for a loopback peer")
	}
	if err := bound.Recheck(); err != nil || lb.binding != first {
		t.Fatalf("a fresh binding was replaced or refused: %v", err)
	}
	clock = func() time.Time { return until.Add(time.Second) }
	t.Cleanup(func() { clock = time.Now })
	if err := bound.Recheck(); err != nil {
		t.Fatalf("re-verification refused the same peer: %v", err)
	}
	if lb.binding == first {
		t.Fatal("a connection past its evidence lifetime decided with the old verdict")
	}
	if bound.Subject() != subject || bound.Peer() == first.Captured() {
		t.Fatalf("re-verified subject %+v, want %+v", bound.Subject(), subject)
	}
}
