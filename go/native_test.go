package inference

import (
	"context"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestNativeChatForwardsToolsButRefusesUnrepresentedFeatures(t *testing.T) {
	tools := wire.Request{Tools: []wire.Tool{{Name: "glob", Description: "find files", Parameters: `{}`}}}
	if reason := (nativeChat{}).unsupported(tools); reason != "" {
		t.Fatalf("native tools refused: %s", reason)
	}
	json := wire.Request{Options: &wire.Options{JSONSchema: `{}`}}
	if reason := (nativeChat{}).unsupported(json); reason != "feature:"+FeatureJSONSchema {
		t.Fatalf("JSON schema reason = %q", reason)
	}
}

func TestLeafExecutionRefusesAnotherNativeHop(t *testing.T) {
	host, err := router.NewNative("next", "unused", listen.ServerExpectation{Principal: identity.User{Kind: "posix", UID: 1}, Program: "/provider"}, []string{"fixture-model"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Router: router.New(host),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if selected, _, _ := p.pick(context.Background(), "fixture-model", nil, "", router.ProfileChat); selected == nil {
		t.Fatal("ordinary OA endpoint did not select the native provider")
	}
	if selected, _, reason := p.pick(WithLeafExecution(context.Background()), "fixture-model", nil, "", router.ProfileChat); selected != nil || reason == "" {
		t.Fatalf("leaf selected %v with reason %q", selected, reason)
	}
}

func TestExecutionBindingPinsNameAndGeneration(t *testing.T) {
	expect := listen.ServerExpectation{Principal: identity.User{Kind: "posix", UID: 1}, Program: "/provider"}
	first, _ := router.NewNative("first", "unused-first", expect, []string{"shared-model"}, []string{router.ProfileChat})
	second, _ := router.NewNative("second", "unused-second", expect, []string{"shared-model"}, []string{router.ProfileChat})
	first.BindingID, second.BindingID = "generation-first", "generation-second"
	p, err := New(Config{Router: router.New(first, second),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := WithExecutionBinding(context.Background(), ExecutionBinding{Host: "second", BindingID: "generation-second"})
	if selected, _, _ := p.pick(ctx, "shared-model", nil, "", router.ProfileChat); selected != second {
		t.Fatalf("selected %v, want second", selected)
	}
	stale := WithExecutionBinding(context.Background(), ExecutionBinding{Host: "second", BindingID: "old-generation"})
	if selected, _, reason := p.pick(stale, "shared-model", nil, "", router.ProfileChat); selected != nil || reason == "" {
		t.Fatalf("stale binding selected %v with reason %q", selected, reason)
	}
	// An excluded duplicate name cannot overwrite the eligible generation used
	// to turn the router's selected name back into a host pointer.
	shadow, _ := router.NewNative("second", "unused-shadow", expect, []string{"shared-model"}, []string{router.ProfileChat})
	shadow.BindingID = "replacement-generation"
	p.cfg.Router.SetHosts(first, second, shadow)
	if selected, _, _ := p.pick(ctx, "shared-model", nil, "", router.ProfileChat); selected != second {
		t.Fatalf("duplicate name selected %v, want pinned generation", selected)
	}
}
