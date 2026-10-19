package inference

import (
	"context"
	"slices"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// INF-A5, INF-G5, facade FAC-R8: a job admits its host before any call
// reaches it, as a Start does, so a declared on-demand native provider starts
// for a video job. The provider here listens only once admission has started
// it, and the first admission answers that it is still starting. On
// 2026-10-02 the runtime never started an on-demand video provider for a job,
// which waited upstream:uncertain until it was cancelled
// (research/live-runs/2026-10-02.md, phase 2 finding 1).
func TestNativeVideoJobStartsAnOnDemandProviderThroughAdmission(t *testing.T) {
	fake := &fakeVideoProvider{script: []wire.VideoObservation{{Status: wire.VideoStateGenerating, Step: 1, Steps: 3}}}
	endpoint := videoEndpoint(t)
	start := fakeVideoProviderStarter(t, fake, endpoint)
	var mu sync.Mutex
	var asked []string
	started := false
	var startErr error
	v := newVideoRuntimeWith(t, endpoint, nil, func(c *Config) {
		c.Admit = func(ctx context.Context, host *router.Host, model string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, AdmissionProfile(ctx)+" "+host.Name+" "+model)
			if !started {
				started = true
				startErr = start()
				return "activating:" + host.Name, false
			}
			return "", startErr == nil
		}
	})
	submission, accepted := v.submit(t, "video-on-demand", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	final := v.waitVideo(t, submission.Identity)
	mu.Lock()
	defer mu.Unlock()
	if startErr != nil {
		t.Fatalf("starting the provider: %v", startErr)
	}
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v; admission asked %v", final, final.Failure, asked)
	}
	want := router.ProfileVideo + " video-provider " + testVideoModel
	if len(asked) < 2 || slices.ContainsFunc(asked, func(a string) bool { return a != want }) {
		t.Fatalf("admission asked %v, want %q at least twice", asked, want)
	}
	if starts, _ := fake.counts(); starts != 1 {
		t.Fatalf("the provider saw %d starts", starts)
	}
}
