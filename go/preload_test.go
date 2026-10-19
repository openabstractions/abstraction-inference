package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func preloadRequest(model string) wire.Request {
	return wire.Request{Model: model,
		Messages: []wire.Message{{Role: wire.RoleSystem, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "You are a careful assistant."}}}},
		Tools:    []wire.Tool{{Name: "read", Description: "Read a file.", Parameters: `{"type":"object","properties":{"filePath":{"type":"string"}}}`}}}
}

// preloadRig is a provider over one host with recorded admission, rights
// and evaluator calls.
type preloadRig struct {
	mu        sync.Mutex
	admitted  []string
	evaluated [][]byte
	records   []Record
	decision  string
}

func (r *preloadRig) provider(t *testing.T, host *router.Host, evaluate bool) *Provider {
	t.Helper()
	config := Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.decision == "" {
				return "permitted", nil
			}
			return r.decision, nil
		},
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) { return nil, "not_permitted" },
		Admit: func(_ context.Context, _ *router.Host, model string) (string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.admitted = append(r.admitted, model)
			return "", true
		},
		Record: func(rec Record) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.records = append(r.records, rec)
		}}
	if evaluate {
		config.EvaluatePrefix = func(_ context.Context, _ *router.Host, _ string, body []byte) (wire.PreloadOutcome, string, int64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.evaluated = append(r.evaluated, body)
			return wire.PreloadOutcomeEvaluated, "", 1234
		}
	}
	p, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// INF-PL2: a preload on a server the provider sends to itself loads the
// model through Admit and hands its evaluator the engine request the chat
// path makes of the prefix: the same openAIBody bytes.
func TestPreloadHandsTheEvaluatorTheChatPathsEngineRequest(t *testing.T) {
	rig := &preloadRig{}
	host := router.OpenAICompatible("engine", "http://engine0.modelhost.invalid", "/v1", []string{"tiny:1b"})
	p := rig.provider(t, host, true)
	req := preloadRequest("tiny:1b")
	reply := p.Preload(context.Background(), caller, req)
	if reply.Outcome != wire.PreloadOutcomeEvaluated || reply.Tokens != 1234 || reply.Host != "engine" || reply.Model != "tiny:1b" {
		t.Fatalf("preload %+v", reply)
	}
	want, err := json.Marshal(openAIBody("tiny:1b", req, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rig.admitted) != 1 || len(rig.evaluated) != 1 || !bytes.Equal(rig.evaluated[0], want) {
		t.Fatalf("admitted %v; evaluated %q, want %q", rig.admitted, rig.evaluated, want)
	}
	if len(rig.records) != 1 || rig.records[0].Profile != "preload" || rig.records[0].Outcome != "evaluated" || rig.records[0].TokensIn != 1234 {
		t.Fatalf("records %+v", rig.records)
	}
}

// INF-PL1, INF-PL2: what a preload refuses, it refuses before any load or
// evaluation: a prefix ending with the user's message, a caller the rights
// decision does not permit, and a server with no evaluator.
func TestPreloadRefusesBeforeAnyLoad(t *testing.T) {
	local := router.OpenAICompatible("engine", "http://engine0.modelhost.invalid", "/v1", []string{"tiny:1b"})
	withUser := preloadRequest("tiny:1b")
	withUser.Messages = append(withUser.Messages, wire.Message{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hi"}}})
	for _, c := range []struct {
		name     string
		host     *router.Host
		evaluate bool
		decision string
		req      wire.Request
		outcome  wire.PreloadOutcome
		reason   string
	}{
		{"the user's message last", local, true, "", withUser, wire.PreloadOutcomeInvalid, "messages:last_user"},
		{"not granted", local, true, "not_granted", preloadRequest("tiny:1b"), wire.PreloadOutcomeNotPermitted, "rights:not_granted"},
		{"no evaluator", local, false, "", preloadRequest("tiny:1b"), wire.PreloadOutcomeUnsupportedFeature, "preload"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := &preloadRig{decision: c.decision}
			p := rig.provider(t, c.host, c.evaluate)
			reply := p.Preload(context.Background(), caller, c.req)
			if reply.Outcome != c.outcome || reply.Reason != c.reason || reply.Host != "" || reply.Tokens != 0 {
				t.Fatalf("preload %+v, want %v %s", reply, c.outcome, c.reason)
			}
			if len(rig.admitted) != 0 || len(rig.evaluated) != 0 {
				t.Fatalf("a refused preload admitted %v and evaluated %d", rig.admitted, len(rig.evaluated))
			}
		})
	}
}

// preloaderFunc adapts a function to wire.Preloader.
type preloaderFunc func(wire.Request) (wire.PreloadReply, error)

func (f preloaderFunc) Preload(req wire.Request) (wire.PreloadReply, error) { return f(req) }

// nativePreloadFixture serves preload@1 alone on the identity-bound native
// transport, as a declared provider's inference service host serves it
// beside chat@1, and returns the router host the runtime builds for it.
func nativePreloadFixture(t *testing.T, preloads bool, preload func(wire.Request) (wire.PreloadReply, error)) *router.Host {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listen.Endpoint("inf-preload-" + fixtureSuffix())
	l, err := listen.ListenFramed(endpoint, listen.Program)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Close() })
	dispatcher := &wire.PreloaderDispatcher{Handler: preloaderFunc(preload)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				callCtx, done := context.WithTimeout(ctx, 5*time.Second)
				defer done()
				call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, 1<<20)
				if err != nil {
					return
				}
				defer call.Close()
				if reply, err := dispatcher.ExchangeFrame(call.Frame); err == nil {
					call.Reply(reply)
				}
			}()
		}
	}()
	host, err := router.NewNative("fixture-native", endpoint, listen.ServerExpectation{Principal: principalOfThisProcess(t), Program: exe},
		[]string{"fixture-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	host.Preloads = preloads
	return host
}

// INF-PL2: a declared native provider that serves preload@1 receives the
// prefix with the chosen model and the caller's claim; one that does not
// declare it is never asked.
func TestNativePreloadReachesOnlyAProviderThatDeclaresIt(t *testing.T) {
	var got wire.Request
	host := nativePreloadFixture(t, true, func(req wire.Request) (wire.PreloadReply, error) {
		got = req
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeEvaluated, Tokens: 77, Host: "inner", Model: "inner"}, nil
	})
	rig := &preloadRig{}
	reply := rig.provider(t, host, false).Preload(context.Background(), caller, preloadRequest("fixture-model"))
	if reply.Outcome != wire.PreloadOutcomeEvaluated || reply.Tokens != 77 || reply.Host != "fixture-native" || reply.Model != "fixture-model" {
		t.Fatalf("native preload %+v", reply)
	}
	if got.Model != "fixture-model" || got.Extensions[ClaimExtension] != caller.Program || len(got.Messages) != 1 || len(got.Tools) != 1 {
		t.Fatalf("forwarded %+v", got)
	}

	called := false
	undeclared := nativePreloadFixture(t, false, func(wire.Request) (wire.PreloadReply, error) {
		called = true
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeEvaluated}, nil
	})
	reply = (&preloadRig{}).provider(t, undeclared, false).Preload(context.Background(), caller, preloadRequest("fixture-model"))
	if reply.Outcome != wire.PreloadOutcomeUnsupportedFeature || called {
		t.Fatalf("an undeclared provider: %+v, called %v", reply, called)
	}
}

// INF-PL2 (decision N2): a preload is speculative work. Its admission tells
// the provider's Admit hook so (PreloadAdmission), a load that would ask
// another holder to yield answers busy, resources:held, and the evaluation
// and the evaluation run with no model activity, so neither an idle clock
// nor a yield waits for it.
func TestAPreloadHoldsTheLowestStanding(t *testing.T) {
	host := router.OpenAICompatible("engine", "http://engine0.modelhost.invalid", "/v1", []string{"tiny:1b"})
	var p *Provider
	var marked bool
	activeDuring, activeLoading := true, true
	config := Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply:  func(context.Context, Subject, string, string, string) (map[string]string, string) { return nil, "not_permitted" },
		Admit: func(ctx context.Context, _ *router.Host, _ string) (string, bool) {
			marked = PreloadAdmission(ctx)
			activeLoading = p.ActiveForModel("tiny:1b")
			return "", true
		},
		EvaluatePrefix: func(context.Context, *router.Host, string, []byte) (wire.PreloadOutcome, string, int64) {
			activeDuring = p.ActiveForModel("tiny:1b")
			return wire.PreloadOutcomeEvaluated, "", 300
		}}
	var err error
	if p, err = New(config); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if reply := p.Preload(context.Background(), caller, preloadRequest("tiny:1b")); reply.Outcome != wire.PreloadOutcomeEvaluated {
		t.Fatalf("preload %+v", reply)
	}
	if !marked || activeDuring || activeLoading {
		t.Fatalf("the admission was marked a preload: %v; the model read active during the load: %v, the evaluation: %v", marked, activeLoading, activeDuring)
	}
	if PreloadAdmission(context.Background()) {
		t.Fatal("an unmarked context reads as a preload")
	}

	held := &preloadRig{}
	config.Admit = func(context.Context, *router.Host, string) (string, bool) { return ReasonResourcesHeld, false }
	config.EvaluatePrefix = func(_ context.Context, _ *router.Host, _ string, body []byte) (wire.PreloadOutcome, string, int64) {
		held.evaluated = append(held.evaluated, body)
		return wire.PreloadOutcomeEvaluated, "", 1
	}
	q, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	reply := q.Preload(context.Background(), caller, preloadRequest("tiny:1b"))
	if reply.Outcome != wire.PreloadOutcomeBusy || reply.Reason != ReasonResourcesHeld || reply.Host != "" || len(held.evaluated) != 0 {
		t.Fatalf("a held card: %+v, evaluated %d", reply, len(held.evaluated))
	}
}
