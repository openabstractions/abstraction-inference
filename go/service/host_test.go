package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const secret = "sk-SERVICE-FIXTURE-0d44"

func TestNativeAdmissionNilAwaitKeepsImmediateRefusal(t *testing.T) {
	r := &receiver{ctx: context.Background(), host: &Host{}}
	attempts := 0
	reason := "rights:not_granted"
	err := r.awaitAdmission(inference.Subject{}, "", nil, func(context.Context) (string, *string) {
		attempts++
		return "not_permitted", &reason
	})
	if err != nil || attempts != 1 {
		t.Fatalf("nil hook: attempts=%d err=%v", attempts, err)
	}
}

func TestNativeAdmissionRetriesOnlyPermissionPreflights(t *testing.T) {
	for _, v := range []struct {
		outcome, reason string
		eligible        bool
	}{
		{"not_permitted", "rights:not_granted content:read:forbidden output:forbidden", true},
		{"not_permitted", "credential:not_permitted:fixture", true},
		{"not_permitted", "credential:not_permitted", true},
		{"forbidden", "output:forbidden", true},
		{"accepted", "rights:not_granted", false},
		{"unavailable", "rights:not_granted", false},
		{"not_permitted", "rights:denied", false},
		{"forbidden", "caller", false},
		{"not_permitted", "rights:not_granted upstream:forbidden", false},
		{"exhausted", "capacity", false},
		{"not_permitted", "rights:not_granted rights:question_pending:q1", true},
		{"not_permitted", "rights:question_pending:q1", false},
		{"not_permitted", "rights:not_granted rights:question_pending:", false},
	} {
		if got := pendingPermissionRefusal(v.outcome, v.reason); got != v.eligible {
			t.Errorf("%s/%s: retry=%v", v.outcome, v.reason, got)
		}
	}
}

func TestNativeQuestionHookFailureIsUnavailable(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	upstream, auth, mu := fakeUpstream(t, false)
	routes := router.New(router.NewHosted("openrouter", upstream.URL, router.WireOpenAICompatible, "openrouter"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	provider, err := inference.New(inference.Config{Router: routes,
		Decide: func(ctx context.Context, _ inference.Subject, _, _ string) (string, error) {
			if inference.AskedFrom(ctx) == nil {
				t.Error("hook admission omitted collector")
			}
			inference.AskedFrom(ctx).Add("fixture-question")
			return "not_granted", nil
		}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			t.Error("refused preflight applied credential")
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	host, err := Listen(endpoint, provider)
	if err != nil {
		provider.Close()
		t.Fatal(err)
	}
	waits := make(chan struct{}, 2)
	host.AwaitQuestions = func(context.Context, inference.Subject, []string) (bool, error) {
		waits <- struct{}{}
		return true, errors.New("fixture wait unavailable")
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	defer func() { stop(); <-done; provider.Close() }()
	v, err := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: 3 * time.Second}).Start(request())
	if err != nil || v.Outcome != wire.StartOutcomeUnavailable || v.Reason != "admission:unavailable" || len(waits) != 1 {
		t.Fatalf("failed wait: %+v %v waits=%d", v, err, len(waits))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*auth) != 0 {
		t.Fatal("failed wait reached upstream")
	}
}

func endpointFor(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\inference-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	}
	dir, err := os.MkdirTemp("/tmp", "inference-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// fakeUpstream streams "Hello!" as OpenAI chat completion chunks; with hold it
// keeps the stream open until the client goes away.
func fakeUpstream(t *testing.T, hold bool) (*httptest.Server, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var auth []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[{"id":"anthropic/claude-sonnet-5"}]}`))
			return
		}
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		f := w.(http.Flusher)
		for _, c := range []string{"Hel", "lo", "!"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", c)
			f.Flush()
		}
		if hold {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(s.Close)
	return s, &auth, &mu
}

type served struct {
	client   *wire.ChatClient
	endpoint string
	host     *Host
	records  func() []inference.Record
	recorded chan struct{}
	program  string
}

func (s *served) waitRecords(t *testing.T, count int) []inference.Record {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if records := s.records(); len(records) >= count {
			return records
		}
		select {
		case <-s.recorded:
		case <-timer.C:
			records := s.records()
			if len(records) >= count {
				return records
			}
			t.Fatalf("audit callback did not record %d entries: %+v", count, records)
		}
	}
}

func serve(t *testing.T, hold bool) (*served, *[]string, *sync.Mutex) {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	up, auth, mu := fakeUpstream(t, hold)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(exe))
	r := router.New(router.NewHosted("openrouter", up.URL+"/api/v1", router.WireOpenAICompatible, "openrouter"))
	r.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer " + secret}, nil
	})
	var recMu sync.Mutex
	var records []inference.Record
	recorded := make(chan struct{}, 8)
	p, err := inference.New(inference.Config{Router: r,
		Decide: func(_ context.Context, s inference.Subject, action, resource string) (string, error) {
			if strings.EqualFold(s.Program, program) && action == inference.ActionComplete && resource == "host:openrouter" {
				return "permitted", nil
			}
			return "not_granted", nil
		},
		Apply: func(_ context.Context, s inference.Subject, consumer, name, target string) (map[string]string, string) {
			if consumer != inference.Contract || name != "openrouter" || target != "127.0.0.1" || !strings.EqualFold(s.Program, program) {
				return nil, "not_permitted"
			}
			return map[string]string{"Authorization": "Bearer " + secret}, "applied"
		},
		Record: func(rec inference.Record) {
			recMu.Lock()
			records = append(records, rec)
			recMu.Unlock()
			select {
			case recorded <- struct{}{}:
			default:
			}
		},
		Idle: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; p.Close() })
	transport := listen.FrameClient{Endpoint: endpoint}.WithDefaults(35*time.Second, MaxFrameBytes)
	return &served{client: wire.NewChatClient(transport), endpoint: endpoint, host: h, program: program, recorded: recorded, records: func() []inference.Record {
		recMu.Lock()
		defer recMu.Unlock()
		return append([]inference.Record(nil), records...)
	}}, auth, mu
}

func request() wire.Request {
	return wire.Request{Model: "anthropic/claude-sonnet-5", Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "openrouter",
		Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hi"}}}}}
}

func TestChatV2SharesCallerBoundOperationRegistry(t *testing.T) {
	s, _, _ := serve(t, false)
	transport := listen.FrameClient{Endpoint: s.endpoint}.WithDefaults(35*time.Second, MaxFrameBytes)
	v2 := wire.NewChatV2Client(transport)
	legacy := request()
	a, err := v2.Start(wire.RequestV2{Target: wire.Target{Model: legacy.Model}, Content: wire.RequestContent{
		Messages: legacy.Messages, Tools: legacy.Tools, Options: legacy.Options, Extensions: legacy.Extensions,
		RequiredExtensions: legacy.RequiredExtensions, Guarantees: legacy.Guarantees, Credential: legacy.Credential}})
	if err != nil || a.Outcome != wire.StartOutcomeAccepted || a.SelectionReason == nil || *a.SelectionReason != wire.SelectionReasonExplicitModel {
		t.Fatalf("chat@2 admission: %+v %v", a, err)
	}
	page, err := s.client.Observe(a.Operation, 0, 256, 65536, 2000)
	if err != nil || page.Outcome != wire.PageOutcomePage || len(page.Deltas) == 0 {
		t.Fatalf("chat@1 Observe of chat@2 operation: %+v %v", page, err)
	}
	if cancelled, err := v2.Cancel(a.Operation); err != nil || (cancelled.Outcome != wire.CancelOutcomeEnded && cancelled.Outcome != wire.CancelOutcomeCancelled) {
		t.Fatalf("chat@2 Cancel: %+v %v", cancelled, err)
	}
}

func TestCallerGuaranteesOverNativeIPC(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("hosted effect") }))
	defer cloud.Close()
	routes := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		t.Error("survey credentials")
		return nil, nil
	})
	p, err := inference.New(inference.Config{Router: routes,
		ResolveSelectionV2: func(_ context.Context, subject inference.Subject, name string) (wire.SelectionPolicyV2, bool, error) {
			if subject.Account == "" || subject.Program == "" {
				t.Error("unbound caller")
			}
			return wire.SelectionPolicyV2{Name: name, Candidates: []wire.SelectionCandidateV2{{Model: "cloud-model", Placement: wire.CandidatePlacementHosted, Credential: "cloudcred"}, {Model: "local-model", Placement: wire.CandidatePlacementLocal}}}, true, nil
		},
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			t.Error("admission")
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			t.Error("credential")
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	defer func() { stop(); <-done }()
	transport := listen.FrameClient{Endpoint: endpoint, Timeout: 3 * time.Second}
	chat := wire.NewChatV2Client(transport)
	image := wire.NewImageV2Client(transport)
	q := wire.RequestV2{Target: wire.Target{SelectionPolicy: "default"}, Content: wire.RequestContent{Messages: request().Messages, Guarantees: []wire.RequestGuarantee{}}}
	iq := wire.ImageRequestV2{Target: q.Target, Content: wire.ImageRequestContent{Mode: wire.ImageModeGenerate, Prompt: "image", Size: "1024x1024", Count: 1, Guarantees: []wire.RequestGuarantee{}}}
	local := []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	a, err := chat.StartWithGuarantees(q, local)
	if err != nil || a.Reason != "selection:placement" || a.Outcome != wire.StartOutcomeNoHost {
		t.Fatalf("chat: %+v %v", a, err)
	}
	a, err = image.StartWithGuarantees(iq, local)
	if err != nil || a.Reason != "selection:placement" || a.Outcome != wire.StartOutcomeNoHost {
		t.Fatalf("image: %+v %v", a, err)
	}
	q.Content.Guarantees = local
	a, err = chat.Start(q)
	if err != nil || a.Reason != "selection:override" {
		t.Fatalf("old Start: %+v %v", a, err)
	}
}

func TestStartObserveOverIPCBindsTheCallingProgram(t *testing.T) {
	s, auth, mu := serve(t, false)
	a, err := s.client.Start(request())
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("start %+v %v", a, err)
	}
	var text strings.Builder
	var pages []wire.DeltaPage
	cursor := int64(0)
	for i := 0; i < 50; i++ {
		page, err := s.client.Observe(a.Operation, cursor, 256, 65536, 2000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v %v", page, err)
		}
		pages = append(pages, page)
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindPart {
				text.WriteString(d.Part.Text)
			}
		}
		cursor = page.Next
		if page.AtEnd {
			break
		}
	}
	if text.String() != "Hello!" {
		t.Fatalf("text %q", text.String())
	}
	records := s.waitRecords(t, 1)
	if len(records) != 1 || !strings.EqualFold(records[0].Program, s.program) || records[0].Outcome != "completed" {
		t.Fatalf("records %+v, program %s", records, s.program)
	}
	raw, _ := json.Marshal([]any{a, pages, records})
	if strings.Contains(string(raw), secret) {
		t.Fatal("a reply or record carries the secret")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*auth) != 1 || (*auth)[0] != "Bearer "+secret {
		t.Fatalf("upstream Authorization %q", *auth)
	}
	if page, err := s.client.Observe("op-unknown", 0, 1, 1024, 0); err != nil || page.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("unknown operation %+v %v", page, err)
	}
}

func TestNativeAdmissionCancellationEndsItsDecision(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	upstream, _, _ := fakeUpstream(t, false)
	routes := router.New(router.NewHosted("openrouter", upstream.URL, router.WireOpenAICompatible, "openrouter"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	entered, ended, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	provider, err := inference.New(inference.Config{Router: routes,
		Decide: func(ctx context.Context, _ inference.Subject, _, _ string) (string, error) {
			if inference.AskedFrom(ctx) != nil {
				t.Error("nil native hook changed legacy collector context")
			}
			close(entered)
			select {
			case <-ctx.Done():
			case <-release:
			}
			close(ended)
			return "not_granted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	host, err := Listen(endpoint, provider)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	defer func() { close(release); stop(); <-done; provider.Close() }()
	call, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: 5 * time.Second}.WithContext(call))
	returned := make(chan error, 1)
	go func() { _, err := client.Start(request()); returned <- err }()
	select {
	case <-entered:
	case err := <-returned:
		t.Fatalf("Start missed its decision: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not enter its decision")
	}
	cancel()
	<-returned
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("caller cancellation left native admission deciding")
	}
}

func TestNativeProviderIsMediatedAndForcedToLocalExecution(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(exe))
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}

	var localCalls, hostedCalls int
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags", "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"fixture-model"}]}`)
		case "/v1/chat/completions":
			localCalls++
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"mediated\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer local.Close()
	hosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostedCalls++
		fmt.Fprint(w, `{"data":[{"id":"fixture-model"}]}`)
	}))
	defer hosted.Close()

	downstreamRouter := router.New(router.Ollama(local.URL), router.NewHosted("hosted", hosted.URL+"/v1", router.WireOpenAICompatible, "fixture"))
	downstream, err := inference.New(inference.Config{Router: downstreamRouter,
		Decide: func(_ context.Context, subject inference.Subject, _, _ string) (string, error) {
			if strings.EqualFold(subject.Program, program) {
				return "permitted", nil
			}
			return "not_granted", nil
		}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	downstreamEndpoint := endpointFor(t)
	// A production leaf designates its runtime, which forwards the caller's
	// cache partition.
	downstreamHost, err := ListenDesignatedLeaf(downstreamEndpoint, downstream, inference.ExecutionAny, program)
	if err != nil {
		t.Fatal(err)
	}
	downstreamCtx, stopDownstream := context.WithCancel(context.Background())
	downstreamDone := make(chan error, 1)
	go func() { downstreamDone <- downstreamHost.Serve(downstreamCtx) }()
	defer func() { stopDownstream(); <-downstreamDone; downstream.Close() }()

	native, err := router.NewNative("declared", downstreamEndpoint, listen.ServerExpectation{Principal: principal, Program: program}, []string{"fixture-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	upstreamRouter := router.New(native)
	upstream, err := inference.New(inference.Config{Router: upstreamRouter,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	upstreamEndpoint := downstreamEndpoint + "-oa"
	upstreamHost, err := ListenForPlacement(upstreamEndpoint, upstream, inference.ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	upstreamCtx, stopUpstream := context.WithCancel(context.Background())
	upstreamDone := make(chan error, 1)
	go func() { upstreamDone <- upstreamHost.Serve(upstreamCtx) }()
	defer func() { stopUpstream(); <-upstreamDone; upstream.Close() }()

	client := wire.NewChatClient(listen.FrameClient{Endpoint: upstreamEndpoint}.WithDefaults(35*time.Second, MaxFrameBytes))
	admission, err := client.Start(wire.Request{Model: "fixture-model", Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "fixture",
		Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}}})
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v, %v", admission, err)
	}
	var page wire.DeltaPage
	for attempts := 0; attempts < 8 && !page.AtEnd; attempts++ {
		page, err = client.Observe(admission.Operation, page.Next, 256, 65536, 2000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("page %+v, %v", page, err)
		}
	}
	if !page.AtEnd {
		t.Fatalf("operation did not end: %+v", page)
	}
	if localCalls != 1 || hostedCalls != 0 {
		t.Fatalf("local calls %d, hosted calls %d", localCalls, hostedCalls)
	}
}

func TestTwoRuntimeNativeCycleStopsAtTheLeafBeforeCallingBack(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(exe))
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	aEndpoint := endpointFor(t)
	bEndpoint := aEndpoint + "-b"
	aToB, err := router.NewNative("runtime-b", bEndpoint, listen.ServerExpectation{Principal: principal, Program: program}, []string{"cycle-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	bToA, err := router.NewNative("runtime-a", aEndpoint, listen.ServerExpectation{Principal: principal, Program: program}, []string{"cycle-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	var decisionsMu sync.Mutex
	aDecisions, bDecisions := 0, 0
	newProvider := func(r *router.Router, decisions *int) *inference.Provider {
		p, err := inference.New(inference.Config{Router: r,
			Decide: func(context.Context, inference.Subject, string, string) (string, error) {
				decisionsMu.Lock()
				(*decisions)++
				decisionsMu.Unlock()
				return "permitted", nil
			},
			Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
				return nil, "not_permitted"
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	aProvider := newProvider(router.New(aToB), &aDecisions)
	bProvider := newProvider(router.New(bToA), &bDecisions)
	aHost, err := ListenForPlacement(aEndpoint, aProvider, inference.ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	bHost, err := ListenDesignatedLeaf(bEndpoint, bProvider, inference.ExecutionLocal, program)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- aHost.Serve(ctx) }()
	go func() { done <- bHost.Serve(ctx) }()
	defer func() {
		cancel()
		<-done
		<-done
		aProvider.Close()
		bProvider.Close()
	}()

	client := wire.NewChatClient(listen.FrameClient{Endpoint: aEndpoint}.WithDefaults(5*time.Second, MaxFrameBytes))
	admission, err := client.Start(wire.Request{Model: "cycle-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "cycle"}}}}})
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("cycle admission %+v, %v", admission, err)
	}
	var page wire.DeltaPage
	for attempts := 0; attempts < 8 && !page.AtEnd; attempts++ {
		page, err = client.Observe(admission.Operation, page.Next, 256, 65536, 2000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("cycle observe %+v, %v", page, err)
		}
	}
	if !page.AtEnd || len(page.Deltas) == 0 {
		t.Fatalf("cycle did not end: %+v", page)
	}
	end := page.Deltas[len(page.Deltas)-1].End
	if end == nil || end.Outcome != wire.ReplyOutcomeNoHost {
		t.Fatalf("cycle end %+v", end)
	}
	decisionsMu.Lock()
	defer decisionsMu.Unlock()
	// The originating provider repeats authority before admission effects and
	// after readiness. The leaf makes no decision and never calls back.
	if aDecisions != 3 || bDecisions != 0 {
		t.Fatalf("cycle decisions runtime-a=%d runtime-b=%d", aDecisions, bDecisions)
	}
}

func TestRetireAdmissionsDrainsAnInFlightStartBeforeRefusingNewStarts(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags", "/api/ps":
			fmt.Fprint(w, `{"models":[{"name":"fixture-model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"retained\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	route := router.Ollama(upstream.URL)
	route.Name, route.BindingID = "declared", "generation-one"
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	provider, err := inference.New(inference.Config{Router: router.New(route),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			once.Do(func() { close(entered) })
			<-release
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	host, err := ListenMediated(endpoint, provider, inference.ExecutionLocal, inference.ExecutionBinding{Host: route.Name, BindingID: route.BindingID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	defer func() { cancel(); <-done; provider.Close() }()
	client := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint}.WithDefaults(5*time.Second, MaxFrameBytes))
	request := wire.Request{Model: "fixture-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "admit"}}}}}
	admitted := make(chan wire.Admission, 1)
	failed := make(chan error, 1)
	go func() {
		admission, err := client.Start(request)
		if err != nil {
			failed <- err
			return
		}
		admitted <- admission
	}()
	select {
	case <-entered:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not enter admission")
	}
	retired := make(chan struct{})
	go func() { host.RetireAdmissions(); close(retired) }()
	select {
	case <-retired:
		t.Fatal("retirement passed an in-flight admission")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	var first wire.Admission
	select {
	case first = <-admitted:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight Start did not finish")
	}
	if first.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("in-flight admission %+v", first)
	}
	select {
	case <-retired:
	case <-time.After(3 * time.Second):
		t.Fatal("retirement did not finish after admission")
	}
	second, err := client.Start(request)
	if err != nil || second.Outcome != wire.StartOutcomeNoHost || second.Reason != "binding:retired" {
		t.Fatalf("post-retirement Start %+v, %v", second, err)
	}
	var page wire.DeltaPage
	for attempts := 0; attempts < 8 && !page.AtEnd; attempts++ {
		page, err = client.Observe(first.Operation, page.Next, 256, 65536, 2000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("retained Observe %+v, %v", page, err)
		}
	}
	if !page.AtEnd {
		t.Fatalf("retained operation did not end: %+v", page)
	}
}

func TestADisconnectMidObserveStopsTheWaitAndIdleClosesTheUpstream(t *testing.T) {
	s, _, _ := serve(t, true)
	a, err := s.client.Start(request())
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("start %+v %v", a, err)
	}
	var first wire.DeltaPage
	for first.Next < 3 {
		if first, err = s.client.Observe(a.Operation, first.Next, 256, 65536, 2000); err != nil {
			t.Fatal(err)
		}
	}
	// A caller that leaves mid-wait: its connection closes when ctx ends.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	abandoned := wire.NewChatClient(listen.FrameClient{Endpoint: s.endpoint}.WithDefaults(35*time.Second, MaxFrameBytes).WithContext(ctx))
	if _, err := abandoned.Observe(a.Operation, first.Next, 256, 65536, 30000); err == nil {
		t.Fatal("the abandoned observe returned a page")
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("the abandoned observe took %v", took)
	}
	// Nobody observes now: past idle_ms (1 s) the operation is cancelled, and
	// a later read finds its end cancelled. A read that returned a page
	// before that would hold the operation as a handle (INF-Q3).
	time.Sleep(1500 * time.Millisecond)
	cursor := first.Next
	for attempts := 0; attempts < 8; attempts++ {
		page, err := s.client.Observe(a.Operation, cursor, 256, 65536, 2000)
		if err != nil {
			t.Fatal(err)
		}
		if page.AtEnd {
			end := page.Deltas[len(page.Deltas)-1].End
			if end.Outcome != wire.ReplyOutcomeCancelled || end.Reason != "idle" {
				t.Fatalf("end %+v", end)
			}
			return
		}
		cursor = page.Next
	}
	t.Fatal("the unobserved operation did not end")
}

func TestNativeHostBoundsConcurrentConnectionsAndRecovers(t *testing.T) {
	s, _, _ := serve(t, false)
	connections := make([]io.Closer, 0, 64)
	defer func() {
		for _, connection := range connections {
			connection.Close()
		}
	}()
	for i := 0; i < 64; i++ {
		connection, err := listen.Dial(s.endpoint)
		if err != nil {
			t.Fatalf("occupy slot %d: %v", i, err)
		}
		connections = append(connections, connection)
		// Session admission waits for the first header byte. A plain frame
		// header then occupies a Host call slot while its body is missing.
		if _, err := connection.Write([]byte{0}); err != nil {
			t.Fatalf("start held call %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(s.host.slots) != cap(s.host.slots) {
		if time.Now().After(deadline) {
			t.Fatalf("held calls occupy %d of %d slots", len(s.host.slots), cap(s.host.slots))
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := s.client.Start(request()); err == nil {
		t.Fatal("connection above the 64-slot bound was served")
	}
	for _, connection := range connections {
		connection.Close()
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		admission, err := s.client.Start(request())
		if err == nil {
			if admission.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("recovered admission %+v", admission)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener did not recover after releasing slots: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
