package service

import (
	"context"
	"runtime"
	"sync"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// OpenCode's hosted heading sends hosted-allowed and no credential name. On
// R10 the router withheld the DeepSeek server (router:unauthorised) before
// any right was decided, so no first-use question was ever asked
// (2026-11-08). A native caller now reaches the hosted server under its
// declared credential: complete is decided for that server, and its refusal
// is the one that asks the person.
func TestNativeHostedCallWithoutCredentialNameReachesTheDeclaredServer(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	for name, placement := range map[string]inference.ExecutionPlacement{"remote endpoint": inference.ExecutionRemote, "any endpoint": inference.ExecutionAny} {
		t.Run(name, func(t *testing.T) {
			upstream, _, _ := fakeUpstream(t, false)
			routes := router.New(router.NewHosted("deepseek", upstream.URL, router.WireOpenAICompatible, "oa-panel-deepseek"))
			routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
			routes.Survey()
			var mu sync.Mutex
			var decided []string
			provider, err := inference.New(inference.Config{Router: routes,
				Decide: func(ctx context.Context, _ inference.Subject, action, resource string) (string, error) {
					mu.Lock()
					decided = append(decided, action+" "+resource)
					mu.Unlock()
					return "not_granted", nil
				}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
					t.Error("credential applied before complete was granted")
					return nil, "not_permitted"
				}})
			if err != nil {
				t.Fatal(err)
			}
			endpoint := endpointFor(t)
			host, err := ListenForPlacement(endpoint, provider, placement)
			if err != nil {
				provider.Close()
				t.Fatal(err)
			}
			ctx, stop := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- host.Serve(ctx) }()
			t.Cleanup(func() { stop(); <-done; provider.Close() })
			q := request()
			q.Credential = ""
			a, err := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: listen.DefaultFrameTimeout}).Start(q)
			if err != nil {
				t.Fatal(err)
			}
			if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:not_granted" {
				t.Fatalf("admission %+v, want the host's own rights refusal", a)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(decided) != 1 || decided[0] != inference.ActionComplete+" host:deepseek" {
				t.Fatalf("decisions %q", decided)
			}
		})
	}
}

// A local-only endpoint still never reaches a hosted server.
func TestLocalEndpointKeepsHostedServersOut(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	upstream, _, _ := fakeUpstream(t, false)
	routes := router.New(router.NewHosted("deepseek", upstream.URL, router.WireOpenAICompatible, "deepseek"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	routes.Survey()
	provider, err := inference.New(inference.Config{Router: routes,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			t.Error("a local endpoint decided a hosted server")
			return "permitted", nil
		}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			t.Error("a local endpoint applied a hosted credential")
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	host, err := ListenForPlacement(endpoint, provider, inference.ExecutionLocal)
	if err != nil {
		provider.Close()
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	t.Cleanup(func() { stop(); <-done; provider.Close() })
	q := request()
	q.Credential = ""
	a, err := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: listen.DefaultFrameTimeout}).Start(q)
	if err != nil || a.Outcome != wire.StartOutcomeNoHost {
		t.Fatalf("admission %+v %v", a, err)
	}
}
