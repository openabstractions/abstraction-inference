package service

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	router "github.com/openabstractions/abstraction-router/go"
)

// Owner, 2026-11-10: "oa panel models loaded shows nothing although i am
// talking with local OA model". The model host's llama.cpp held the model,
// and the runtime's router listed no native host's model as resident: a
// native host's residency was always "unreported". A designated leaf now
// states its residents on Describe (MODELHOST-H10), and the router reads
// them as the host's residency.
func TestDesignatedLeafStatesItsResidentsToTheRouter(t *testing.T) {
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

	provider, err := inference.New(inference.Config{Router: router.New(),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	leaf, err := ListenDesignatedLeaf(endpoint, provider, inference.ExecutionLocal, program)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	loaded := []string{"qwen3.6-35b-a3b"}
	leaf.Residents = func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(loaded) }
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- leaf.Serve(ctx) }()
	defer func() { stop(); <-done; provider.Close() }()

	native, err := router.NewNative("modelhost", endpoint, listen.ServerExpectation{Principal: principal, Program: program},
		[]string{"chat:qwen3.6-35b-a3b", "chat:qwen3-0.6b"}, []string{router.ProfileChat})
	if err != nil {
		t.Fatal(err)
	}
	routes := router.New(native)
	resident := func() map[string]bool {
		t.Helper()
		if err := routes.SurveyContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		families, _ := routes.Models(false)
		out := map[string]bool{}
		for _, family := range families {
			for _, alias := range family.Names {
				if alias.Host == "modelhost" {
					out[alias.Name] = alias.Resident
				}
			}
		}
		return out
	}
	if got := resident(); !got["qwen3.6-35b-a3b"] || got["qwen3-0.6b"] {
		t.Fatalf("residency %v, want qwen3.6-35b-a3b alone resident", got)
	}
	states, _, _, _, _ := routes.Residency(false)
	if len(states) != 1 || !slices.Equal(states[0].Resident, []string{"qwen3.6-35b-a3b"}) || states[0].Why != "" {
		t.Fatalf("host state %+v", states)
	}
	decision, _ := routes.Route(router.Request{Model: "qwen3.6-35b-a3b", Profile: router.ProfileChat})
	if decision.Verdict != router.Resident {
		t.Fatalf("route %s, want resident", decision.Verdict)
	}

	// The idle time unloaded it: the next survey reports nothing loaded,
	// with residency reported rather than unknown.
	mu.Lock()
	loaded = nil
	mu.Unlock()
	if got := resident(); got["qwen3.6-35b-a3b"] || got["qwen3-0.6b"] {
		t.Fatalf("residency after unload %v", got)
	}
	states, _, _, _, _ = routes.Residency(false)
	if len(states) != 1 || len(states[0].Resident) != 0 || states[0].Why != "" {
		t.Fatalf("host state after unload %+v", states)
	}
}

func TestResidentCapabilitiesRoundTrip(t *testing.T) {
	names := []string{"a", "", "b", "a"}
	capabilities := router.ResidentCapabilities(names)
	got, ok := router.ParseResidentCapabilities(capabilities)
	if !ok || !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("parsed %v, %v from %v", got, ok, capabilities)
	}
	if _, ok := router.ParseResidentCapabilities(map[string]string{"profiles": "chat"}); ok {
		t.Fatal("a description without the resident key reported residency")
	}
	many := make([]string, 40)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if capabilities := router.ResidentCapabilities(many); len(capabilities) > 32 {
		t.Fatalf("%d capabilities exceed the description's 32", len(capabilities))
	}
}
