package service

import (
	"context"
	"runtime"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// A chat@3 Start over the native transport onto a host whose provider is
// still launching reads its definite refusal before the client's default
// call budget ends. On R28 the runtime's launch wait ran to the 35 s
// connection budget, OpenCode hung up at five seconds, and the refusal
// unavailable activating:modelhost was written into a closed pipe; the
// plugin then showed "The last request may still have run".
func TestNativeStartOntoALaunchingHostAnswersWhileTheCallerListens(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	host := router.OpenAICompatible("launching", "http://launching.invalid", "/v1", []string{"fixture-model"})
	host.Profiles = []string{router.ProfileChat}
	p, err := inference.New(inference.Config{ChatLogicalOwner: "fixture-owner", Router: router.New(host),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		// The runtime's launch wait: until the provider is ready, its own
		// budget, or the admitting context's deadline.
		Admit: func(ctx context.Context, _ *router.Host, _ string) (string, bool) {
			select {
			case <-ctx.Done():
			case <-time.After(30 * time.Second):
			}
			return "activating:launching", false
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	h, err := ListenForPlacement(endpoint, p, inference.ExecutionLocal)
	if err != nil {
		p.Close()
		t.Fatal(err)
	}
	h.ApplicationChatV3 = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		p.Close()
	})
	transport := listen.FrameClient{Endpoint: endpoint}.WithDefaults(listen.DefaultFrameTimeout, MaxFrameBytes)
	client := wire.NewChatV3Client(transport)
	window, err := client.GetHistoryWindow()
	if err != nil || window.Window == nil {
		t.Fatalf("history window: %+v %v", window, err)
	}
	request := wire.RequestV3{Identity: acceptance.RequestIdentity{Key: "launching", HistoryEpoch: window.Window.HistoryEpoch},
		Target: wire.ChatTarget{Model: "fixture-model"},
		Content: wire.ChatContent{Messages: []wire.ChatMessage{{Role: wire.RoleUser,
			Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "hi"}}}}}}
	began := time.Now()
	admission, err := client.Start(request)
	if err != nil {
		t.Fatalf("Start after %v: %v; the caller never read the refusal", time.Since(began), err)
	}
	if admission.Outcome != wire.ChatStartOutcomeUnavailable || admission.Reason != "activating:launching" {
		t.Fatalf("Start onto a launching host: %s %q", admission.Outcome, admission.Reason)
	}
}
