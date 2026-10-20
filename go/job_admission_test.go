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
	started, allowAdmission := false, false
	var startErr error
	v := newVideoRuntimeWith(t, endpoint, nil, func(c *Config) {
		c.Admit = func(ctx context.Context, host *router.Host, model string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, AdmissionProfile(ctx)+" "+host.Name+" "+model)
			if !started {
				started = true
				startErr = start()
			}
			if !allowAdmission {
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
	waitSnapshot(t, v, submission.Identity, func(s *acceptance.OperationSnapshot) bool {
		return s.State == acceptance.WorkStateRunning && s.Waiting == "activating:video-provider"
	})
	if starts, _ := fake.counts(); starts != 0 {
		t.Fatalf("the provider saw %d starts before its activation was admitted", starts)
	}
	mu.Lock()
	allowAdmission = true
	mu.Unlock()
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

// A job whose provider stopped after failed starts fails at once with
// host:start_failed:<name> and never reaches the provider. On the R34 live
// test a picture job onto Lemonade without Setup waited "activating" for
// more than 411 s while the provider crash-looped (D1).
func TestNativeJobFailsAtOnceOnAStoppedProvider(t *testing.T) {
	fake := &fakeVideoProvider{script: []wire.VideoObservation{{Status: wire.VideoStateGenerating, Step: 1, Steps: 3}}}
	endpoint := videoEndpoint(t)
	var mu sync.Mutex
	asked := 0
	v := newVideoRuntimeWith(t, endpoint, nil, func(c *Config) {
		c.Admit = func(ctx context.Context, host *router.Host, model string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			asked++
			return StartFailedPrefix + host.Name, false
		}
	})
	submission, accepted := v.submit(t, "video-stopped", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "host:start_failed:video-provider" {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Fatalf("admission asked %d times", asked)
	}
	if starts, _ := fake.counts(); starts != 0 {
		t.Fatalf("the provider saw %d starts", starts)
	}
}
