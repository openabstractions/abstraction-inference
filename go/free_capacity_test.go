package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3FreeCapacityUnsupportedDirectSendsNoSurveyBytes(t *testing.T) {
	var bytes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { bytes.Add(1) }))
	defer server.Close()
	routes := router.New()
	routes.SetHosts(router.OpenAICompatible("direct", server.URL, "/v1", nil))
	p, err := New(Config{Router: routes, ChatLogicalOwner: "fixture-job-owner",
		Decide: func(context.Context, Subject, string, string) (string, error) {
			t.Error("unsupported reached authority")
			return "permitted", nil
		},
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			t.Error("unsupported applied credential")
			return nil, "applied"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = p.chat3History.window().HistoryEpoch
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly, wire.ChatGuaranteeFreeCapacity}
	if a := p.StartV3(context.Background(), caller, r); a.Outcome != wire.ChatStartOutcomeUnsupported || bytes.Load() != 0 {
		t.Fatalf("unsupported=%+v requests=%d", a, bytes.Load())
	}
}

func TestChat3FreeCapacityAdmissionAndExecution(t *testing.T) {
	for _, mode := range []string{"unsupported", "nil-admit", "denied", "held", "free"} {
		t.Run(mode, func(t *testing.T) {
			seen := make(chan bool, 1)
			calls := 0
			f := setup(t, router.WireOpenAICompatible, func(config *Config) {
				config.FreeCapacity = mode != "unsupported"
				config.Admit = nil
				if mode != "unsupported" && mode != "nil-admit" {
					config.Admit = func(ctx context.Context, _ *router.Host, _ string) (string, bool) {
						calls++
						if !FreeCapacityAdmission(ctx) || AdmissionProfile(ctx) != "chat" {
							t.Error("admission lost required guarantee")
						}
						if mode == "denied" {
							return "model:restricted", false
						}
						return "resource:held", mode == "free"
					}
				}
				config.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
					seen <- FreeCapacityAdmission(ctx)
					return ExecutionPreparation{Reason: "fixture:stop"}
				}
			})
			r := chat3RequestFixture()
			r.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
			r.Target.Model, r.Content.Credential = "anthropic/claude-sonnet-5", "openrouter"
			r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed, wire.ChatGuaranteeFreeCapacity}
			a := f.provider.StartV3(context.Background(), caller, r)
			switch mode {
			case "unsupported", "nil-admit":
				if a.Outcome != wire.ChatStartOutcomeUnsupported || calls != 0 {
					t.Fatalf("unsupported=%+v calls=%d", a, calls)
				}
			case "denied":
				if a.Outcome != wire.ChatStartOutcomeUnavailable || a.Reason != "model:restricted" || calls != 1 {
					t.Fatalf("Admit restriction bypassed: %+v calls=%d", a, calls)
				}
			case "held":
				if a.Outcome != wire.ChatStartOutcomeUnavailable || a.Reason != "resource:held" || calls != 1 {
					t.Fatalf("held=%+v calls=%d", a, calls)
				}
			case "free":
				if a.Outcome != wire.ChatStartOutcomeAccepted || calls != 1 {
					t.Fatalf("free=%+v calls=%d", a, calls)
				}
				select {
				case required := <-seen:
					if !required {
						t.Fatal("worker lost free-capacity guarantee")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("execution preparation missing")
				}
			}
			if len(f.up.posts()) != 0 {
				t.Fatal("fixture sent upstream bytes")
			}
		})
	}
}

func TestChat3FreeCapacityPolicyAndRelay(t *testing.T) {
	r := chat3RequestFixture()
	r.Target = wire.ChatTarget{SelectionPolicy: "title"}
	r.Content.Credential = ""
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly, wire.ChatGuaranteeFreeCapacity}
	if reason := validateChat3Request(r); reason != "" {
		t.Fatal(reason)
	}
	r.Content.Guarantees = append(r.Content.Guarantees, wire.ChatGuaranteeFreeCapacity)
	if reason := validateChat3Request(r); reason == "" {
		t.Fatal("duplicate restriction admitted")
	}
	for _, wireKind := range []string{router.WireNative, router.WireRemote} {
		f := setup(t, router.WireOpenAICompatible, nil)
		op := chat3RelayOperation(t, f.provider)
		op.host.Wire = wireKind
		op.chat3.request.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeFreeCapacity}
		relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnsupported}}
		f.provider.runChat3Relay(context.Background(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
		if len(relay.starts) != 1 || !chat3HasGuarantee(relay.starts[0], wire.ChatGuaranteeFreeCapacity) {
			t.Fatalf("%s lost restriction: %+v", wireKind, relay.starts)
		}
	}
}
