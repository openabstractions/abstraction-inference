package service

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// pendingHost serves one native endpoint whose every complete decision reads
// not_granted and raises the question q-pending.
func pendingHost(t *testing.T, configure func(*Host)) string {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	upstream, _, _ := fakeUpstream(t, false)
	routes := router.New(router.NewHosted("openrouter", upstream.URL, router.WireOpenAICompatible, "openrouter"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	provider, err := inference.New(inference.Config{Router: routes,
		Decide: func(ctx context.Context, _ inference.Subject, _, _ string) (string, error) {
			inference.AskedFrom(ctx).Add("q-pending")
			return "not_granted", nil
		}, Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
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
	configure(host)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	t.Cleanup(func() { stop(); <-done; provider.Close() })
	return endpoint
}

// A first-use refusal whose question stays pending reaches a client with the
// default call budget as a typed refusal naming the question. The frame
// carries no client deadline: before INF-W11 the native hold outlived that
// budget and the client saw its own transport timeout instead (2026-11-01,
// "Error: exchange" in OpenCode, a deadline in the MCP gateway).
func TestNativePendingQuestionRefusesWithinDefaultBudget(t *testing.T) {
	for name, hold := range map[string]bool{"held for the person": true, "no hold": false} {
		t.Run(name, func(t *testing.T) {
			endpoint := pendingHost(t, func(h *Host) {
				if hold {
					h.AwaitQuestions = func(ctx context.Context, _ inference.Subject, _ []string) (bool, error) {
						<-ctx.Done() // the person has not answered
						return false, ctx.Err()
					}
				}
				h.PendingQuestions = func(_ context.Context, _ inference.Subject, ids []string) ([]string, error) {
					return ids, nil
				}
			})
			started := time.Now()
			v, err := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: listen.DefaultFrameTimeout}).Start(request())
			if err != nil {
				t.Fatalf("client with the default budget: %v after %v", err, time.Since(started))
			}
			if v.Outcome != wire.StartOutcomeNotPermitted || v.Reason != "rights:not_granted rights:question_pending:q-pending" {
				t.Fatalf("refusal %+v", v)
			}
			if elapsed := time.Since(started); elapsed >= listen.DefaultFrameTimeout-time.Second || !hold && elapsed > 2*time.Second {
				t.Fatalf("refusal took %v", elapsed)
			}
		})
	}
}

// A question the person already answered is not named.
func TestNativeAnsweredQuestionIsNotNamed(t *testing.T) {
	endpoint := pendingHost(t, func(h *Host) {
		h.PendingQuestions = func(context.Context, inference.Subject, []string) ([]string, error) { return nil, nil }
	})
	v, err := wire.NewChatClient(listen.FrameClient{Endpoint: endpoint, Timeout: listen.DefaultFrameTimeout}).Start(request())
	if err != nil || v.Outcome != wire.StartOutcomeNotPermitted || v.Reason != "rights:not_granted" {
		t.Fatalf("refusal %+v %v", v, err)
	}
}

func TestPendingQuestionWords(t *testing.T) {
	if got := questionWords([]string{"a", "b", "a", "", "c d"}); got != "rights:question_pending:a rights:question_pending:b" {
		t.Fatalf("words %q", got)
	}
	if got := questionWords(nil); got != "" {
		t.Fatalf("no question names %q", got)
	}
	reason := "rights:not_granted content:read:forbidden " + questionWords([]string{"x"})
	if !pendingPermissionRefusal("not_permitted", reason) || !strings.HasSuffix(reason, "rights:question_pending:x") {
		t.Fatalf("a refusal naming its question is a permission refusal: %q", reason)
	}
}
