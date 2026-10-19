package service

import (
	"context"
	"encoding/json"
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

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// embedUpstream is an OpenAI-compatible embeddings server that keeps each
// request body it receives.
type embedUpstream struct {
	mu     sync.Mutex
	bodies []string
}

func (u *embedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/models"):
		_, _ = io.WriteString(w, `{"data":[{"id":"embed-model"}]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/embeddings"):
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(body))
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"index": 0, "embedding": []float64{0.5, 0.25}}}, "usage": map[string]int{"prompt_tokens": 1}})
	default:
		http.NotFound(w, r)
	}
}

// An embed leaf served to a designated runtime records the runtime's bound
// original caller, and an application's forged claim never becomes it.
func TestDesignatedEmbedLeafRecordsOriginalBoundCaller(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	upstream := &embedUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()
	backend := router.OpenAICompatible("vectors", server.URL, "/v1", []string{"embed-model"})
	backend.Profiles = []string{router.ProfileEmbed}
	leafRecords := &recordLog{}
	leaf, err := inference.New(inference.Config{Router: router.New(backend), Record: leafRecords.add,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(exe))
	endpoint := endpointFor(t)
	h, err := ListenDesignatedLeaf(endpoint, leaf, inference.ExecutionLocal, exe)
	if err != nil {
		leaf.Close()
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	defer func() { stop(); <-done; leaf.Close() }()

	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("native-embed", endpoint, listen.ServerExpectation{Principal: principal, Program: program}, []string{"embed-model"}, []string{router.ProfileEmbed})
	if err != nil {
		t.Fatal(err)
	}
	mediatorRecords := &recordLog{}
	mediator, err := inference.New(inference.Config{Router: router.New(native), Record: mediatorRecords.add,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mediator.Close()

	appCtx := inference.WithRoute(context.Background(), inference.RouteNative, "program")
	req := wire.EmbedRequest{Model: "embed-model", Inputs: []string{"alpha"}, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly},
		Extensions: map[string]string{"vendor/shape": "flat", inference.ClaimExtension: "forged-app"}}
	result := mediator.Embed(appCtx, claimApp, req)
	if result.Outcome != wire.EmbedOutcomeCompleted || len(result.Vectors) != 1 {
		t.Fatalf("embed %+v", result)
	}
	record := leafRecords.wait(t, func(r inference.Record) bool { return r.Profile == router.ProfileEmbed })
	if record.Outcome != "completed" || record.Claim != claimApp.Program || !identity.SameSubjectProgram(record.Program, program) {
		t.Fatalf("leaf embed record %+v", record)
	}
	if record := mediatorRecords.wait(t, func(r inference.Record) bool { return r.Profile == router.ProfileEmbed }); record.Claim != "" || record.Program != claimApp.Program {
		t.Fatalf("runtime embed record %+v", record)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.bodies) != 1 || strings.Contains(upstream.bodies[0], reservedPrefix) || strings.Contains(upstream.bodies[0], "forged-app") {
		t.Fatalf("vendor bodies %q", upstream.bodies)
	}
}
