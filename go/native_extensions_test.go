package inference

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestNativeExtensionForwardingPreservesRequirementsAndBoundClaim(t *testing.T) {
	values := map[string]string{"vendor/steps": "4", ClaimExtension: "forged-app"}
	required := []string{"vendor/steps"}
	gotValues, gotRequired := forwardedNativeExtensions(values, required, "bound-app")
	if gotValues["vendor/steps"] != "4" || gotValues[ClaimExtension] != "bound-app" || len(gotRequired) != 1 || gotRequired[0] != "vendor/steps" {
		t.Fatalf("forwarded values %#v required %#v", gotValues, gotRequired)
	}
	gotValues["vendor/steps"] = "changed"
	gotRequired[0] = "changed"
	if values["vendor/steps"] != "4" || required[0] != "vendor/steps" {
		t.Fatal("forwarder mutated caller's request")
	}
}

func TestSeparateExtensionsTrustsOnlyDelegatingRuntimes(t *testing.T) {
	values := map[string]string{"vendor/steps": "4", ClaimExtension: "bound-app", ReservedExtensionPrefix + "other": "x"}
	native := WithRoute(context.Background(), RouteNative, "program")
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		claim string
	}{
		{"application", native, ""},
		{"designated runtime", WithDesignatedRuntime(native), "bound-app"},
		{"remote runtime", WithRemote(context.Background(), "peer.example", "tls-remote/peer.example"), "bound-app"},
	} {
		got := separateExtensions(tc.ctx, values, []string{"vendor/steps"})
		if got.claim != tc.claim || got.required != "" || len(got.vendor) != 1 || got.vendor["vendor/steps"] != "4" {
			t.Fatalf("%s: %+v", tc.name, got)
		}
	}
	if len(values) != 3 || values[ClaimExtension] != "bound-app" {
		t.Fatal("separation mutated caller's request")
	}
	if got := separateExtensions(native, values, []string{"vendor/steps", ClaimExtension}); got.required != ClaimExtension {
		t.Fatalf("required reserved key %+v", got)
	}
}

type claimCheckingEngine struct {
	mu   sync.Mutex
	seen []wire.ImageRequest
}

func (e *claimCheckingEngine) Unsupported(req wire.ImageRequest) string {
	e.mu.Lock()
	e.seen = append(e.seen, req)
	e.mu.Unlock()
	for key := range req.Extensions {
		if key != "vendor/steps" {
			return "extension:" + key
		}
	}
	return ""
}

func (e *claimCheckingEngine) Run(_ context.Context, _ string, req wire.ImageRequest, _ map[string]ImageInput, _ func(int64, string)) ImageEngineReply {
	e.mu.Lock()
	e.seen = append(e.seen, req)
	e.mu.Unlock()
	return ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: pngOne, MediaType: "image/png"}}}
}

func TestImageEngineReceivesVendorKeysAndRecordsDesignatedClaim(t *testing.T) {
	engine := &claimCheckingEngine{}
	p, _ := imageEngineFixture(t, engine, "permitted")
	records := make(chan Record, 4)
	p.cfg.Record = func(r Record) { records <- r }
	req := imageEngineRequest()
	req.Extensions = map[string]string{"vendor/steps": "4", ClaimExtension: "bound-app"}
	req.RequiredExtensions = []string{"vendor/steps"}
	ctx := WithDesignatedRuntime(WithRoute(context.Background(), RouteNative, "program"))
	a := p.StartImage(ctx, caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	select {
	case r := <-records:
		if r.Operation != a.Operation || r.Outcome != "completed" || r.Claim != "bound-app" || r.Program != caller.Program {
			t.Fatalf("record %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no operation record")
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.seen) != 2 {
		t.Fatalf("engine calls %d", len(engine.seen))
	}
	for _, seen := range engine.seen {
		if len(seen.Extensions) != 1 || seen.Extensions["vendor/steps"] != "4" || !slices.Equal(seen.RequiredExtensions, []string{"vendor/steps"}) {
			t.Fatalf("engine request extensions %v required %v", seen.Extensions, seen.RequiredExtensions)
		}
	}
}

func TestRequiredReservedExtensionRefusedBeforeAuthority(t *testing.T) {
	host, err := router.NewNative("native-chat", "unused", listen.ServerExpectation{Principal: identity.User{Kind: "posix", UID: 1}, Program: "/provider"}, []string{"fixture-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	// The host advertises the reserved key, so only the provider's own seam
	// can refuse it before the rights decision.
	host.SupportedExtensions = map[string]map[string]bool{router.ProfileChat: {ClaimExtension: true}}
	rights := 0
	p, err := New(Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { rights++; return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	req := wire.Request{Model: "fixture-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
		Extensions: map[string]string{ClaimExtension: "forged-app"}, RequiredExtensions: []string{ClaimExtension}}
	if got := p.Start(context.Background(), Subject{Account: "test", Program: "caller"}, req); got.Outcome != wire.StartOutcomeUnsupportedFeature || got.Reason != "extension:"+ClaimExtension || rights != 0 {
		t.Fatalf("required reserved key %+v, rights %d", got, rights)
	}
}

func TestNativeChatRequiredExtensionUsesSelectedProfileBeforeAuthority(t *testing.T) {
	host, err := router.NewNative("native-chat", "unused", listen.ServerExpectation{Principal: identity.User{Kind: "posix", UID: 1}, Program: "/provider"}, []string{"fixture-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	host.SupportedExtensions = map[string]map[string]bool{router.ProfileImage: {"vendor/steps": true}}
	rights, admits := 0, 0
	p, err := New(Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { rights++; return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		Admit: func(context.Context, *router.Host, string) (string, bool) { admits++; return "", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	req := wire.Request{Model: "fixture-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}},
		Extensions: map[string]string{"vendor/steps": "4"}, RequiredExtensions: []string{"vendor/steps"}}
	if got := p.Start(context.Background(), Subject{Account: "test", Program: "caller"}, req); got.Outcome != wire.StartOutcomeUnsupportedFeature || got.Reason != "extension:vendor/steps" || rights != 0 || admits != 0 {
		t.Fatalf("unadvertised chat admission %+v, rights %d, admits %d", got, rights, admits)
	}
	host.SupportedExtensions[router.ProfileChat] = map[string]bool{"vendor/steps": true}
	if got := p.Start(context.Background(), Subject{Account: "test", Program: "caller"}, req); got.Outcome != wire.StartOutcomeAccepted || rights != 1 || admits != 1 {
		t.Fatalf("advertised chat admission %+v, rights %d, admits %d", got, rights, admits)
	}
}
