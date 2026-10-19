package inference

import (
	"context"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// TestJobBackendsAreChosenByWhatTheyServe: with a native video provider and a
// Replicate host side by side, a video job for a model the provider lists goes
// to the native video backend, a video job for a Replicate model still goes to
// Replicate, and an image batch never reaches the video backend.
func TestJobBackendsAreChosenByWhatTheyServe(t *testing.T) {
	expect := listen.ServerExpectation{Principal: identity.User{Kind: "posix", UID: 1}, Program: "/provider"}
	native, err := router.NewNative("video-provider", "unused", expect, []string{testVideoModel}, []string{router.ProfileVideo})
	if err != nil {
		t.Fatal(err)
	}
	replicate := router.NewHosted("replicate", "https://api.replicate.com", router.WireReplicatePredictions, "replicate")
	routes := router.New(native, replicate)
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	routes.Survey()
	provider, err := New(Config{Router: routes,
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return map[string]string{"Authorization": "Bearer fixture"}, "applied"
		},
		PrepareContentWrite: func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
			return func(context.Context, string, []byte) ContentOutcome { return ContentResolved }, ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	video := nativeVideoJobBackend{}
	replicateBackend := replicateJobBackend{}
	e := &JobExecution{provider: provider, backends: []durableJobBackend{video, replicateBackend},
		resolveSubject: func(context.Context, string, string) (Subject, ContentOutcome) { return caller, ContentResolved }}
	local := []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}
	hosted := []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}
	for _, c := range []struct {
		name    string
		request jobwire.Request
		host    string
		backend durableJobBackend
	}{
		{"video-native", jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 2000, Image: jobwire.ImageRequest{Model: testVideoModel, Mode: jobwire.ModeGenerate, Prompt: "p", Size: "auto", Count: 1, Guarantees: local}}, "video-provider", video},
		{"video-replicate", jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 2000, Image: jobwire.ImageRequest{Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "p", Size: "auto", Count: 1, Guarantees: hosted, Credential: "replicate"}}, "replicate", replicateBackend},
		{"batch-replicate", jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "p", Size: "auto", Count: 1, Guarantees: hosted, Credential: "replicate"}}, "replicate", replicateBackend},
	} {
		plan, outcome, reason := e.plan(context.Background(), inferenceJobWork{Scope: "caller-scope", Request: c.request}, inferenceJobCheckpoint{}, "")
		if outcome != acceptance.AcceptanceOutcomeAccepted || plan.host == nil || plan.host.Name != c.host || plan.backend != c.backend {
			t.Fatalf("%s: outcome %s %q host %v backend %T", c.name, outcome, reason, plan.host, plan.backend)
		}
		pinned, outcome, reason := e.plan(context.Background(), inferenceJobWork{Scope: "caller-scope", Request: c.request},
			inferenceJobCheckpoint{Phase: "submitted", Host: plan.host.Name, Binding: inferenceHostBinding(plan.host), Model: plan.model, Adapter: plan.adapter}, "operation")
		if outcome != acceptance.AcceptanceOutcomeAccepted || pinned.backend != c.backend {
			t.Fatalf("%s recovered: outcome %s %q backend %T", c.name, outcome, reason, pinned.backend)
		}
	}
}
