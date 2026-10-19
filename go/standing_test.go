package inference

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
	router "github.com/openabstractions/abstraction-router/go"
)

// INF-A6: a live session is decided again for each input. After its rule is
// revoked the next Append reads closed, nothing more reaches the upstream
// session, and the session ends cancelled with the refusal. The decision
// names the session's server credential, which this runtime applied.
func TestARevokedLiveSessionRefusesItsNextInput(t *testing.T) {
	f := newLiveFixture(t, false)
	ctx := context.Background()
	a := f.p.StartLive(ctx, caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	pcm := make([]byte, 9600)
	if got := f.p.AppendLive(ctx, caller, a.Operation, 0, pcm); got.Outcome != wire.LiveInputOutcomeAccepted {
		t.Fatalf("a permitted append read %+v", got)
	}
	f.standing.set("rights:denied", false)
	if got := f.p.AppendLive(ctx, caller, a.Operation, 1, pcm); got.Outcome != wire.LiveInputOutcomeClosed {
		t.Fatalf("an append after the revoke read %+v", got)
	}
	if got := f.p.CommitLive(ctx, caller, a.Operation); got.Outcome != wire.LiveInputOutcomeClosed {
		t.Fatalf("a commit after the revoke read %+v", got)
	}
	end, _ := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCancelled || end.Reason != "rights:denied" || end.Delivery.Digest != "" {
		t.Fatalf("revoked session ended %+v", end)
	}
	if f.appends.Load() != 1 || f.commits.Load() != 0 {
		t.Fatalf("after the revoke the upstream saw %d appends and %d commits", f.appends.Load(), f.commits.Load())
	}
	f.standing.mu.Lock()
	credentials := slices.Clone(f.standing.credentials)
	f.standing.mu.Unlock()
	if len(credentials) == 0 || credentials[0] != "live" {
		t.Fatalf("the standing decision named credentials %v, want live", credentials)
	}
}

// INF-A6: an input whose decision cannot be obtained sends nothing and reads
// unavailable; the session stays, and once the decision answers again the same
// input is accepted.
func TestALiveInputSendsNothingWithoutADecision(t *testing.T) {
	f := newLiveFixture(t, false)
	ctx := context.Background()
	a := f.p.StartLive(ctx, caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	pcm := make([]byte, 9600)
	f.standing.set("", true)
	if got := f.p.AppendLive(ctx, caller, a.Operation, 0, pcm); got.Outcome != wire.LiveInputOutcomeUnavailable {
		t.Fatalf("an append without a decision read %+v", got)
	}
	if f.appends.Load() != 0 {
		t.Fatal("an append without a decision reached the upstream session")
	}
	f.standing.set("", false)
	if got := f.p.AppendLive(ctx, caller, a.Operation, 0, pcm); got.Outcome != wire.LiveInputOutcomeAccepted {
		t.Fatalf("the append after the outage read %+v", got)
	}
}

// INF-A6: Recheck, which a composition calls on every committed rights change
// and the watchdog calls on its clock, ends a running operation whose rule a
// revoke refuses, with no input from its caller. An outage ends nothing.
func TestRecheckEndsARunningOperationItsRuleNoLongerPermits(t *testing.T) {
	f := newLiveFixture(t, false)
	ctx := context.Background()
	a := f.p.StartLive(ctx, caller, liveRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	if got := f.p.AppendLive(ctx, caller, a.Operation, 0, make([]byte, 9600)); got.Outcome != wire.LiveInputOutcomeAccepted {
		t.Fatal(got)
	}
	f.standing.set("", true)
	f.p.Recheck(ctx)
	if op := f.p.visibleProfile(caller, a.Operation, router.ProfileLive); op == nil || op.stopped() != "" {
		t.Fatal("an unavailable decision ended the session")
	}
	f.standing.set("credential:not_permitted", false)
	f.p.Recheck(ctx)
	end, _ := liveEnd(t, f.p, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCancelled || end.Reason != "credential:not_permitted" {
		t.Fatalf("revoked session ended %+v", end)
	}
}

// INF-A6: a running durable job is decided again before each poll. Once its
// rule is revoked it polls no more with the plan's headers: the upstream work
// is cancelled and the job fails with the refusal.
func TestARevokedJobStopsPollingAndCancelsItsUpstreamWork(t *testing.T) {
	for _, refusal := range []string{"rights:denied", "credential:not_permitted"} {
		t.Run(refusal, func(t *testing.T) {
			backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobRunning}}
			fixture := newDurableFixture(t, backend)
			var revoked atomic.Bool
			backend.pollHook = func() { revoked.Store(true) }
			fixture.provider.cfg.Standing = func(context.Context, Subject, string, string) (string, error) {
				if revoked.Load() {
					return refusal, nil
				}
				return "", nil
			}
			// The cancel reuses the plan's headers once. Only a refused
			// credential use is that credential's cancel-time use, which the
			// runtime records in the credential audit (CRED-A7).
			var cancelled []string
			var cancelledMu sync.Mutex
			fixture.provider.cfg.CancelledWith = func(_ context.Context, s Subject, consumer, credential, target string) {
				cancelledMu.Lock()
				defer cancelledMu.Unlock()
				cancelled = append(cancelled, s.Program+" "+consumer+" "+credential+" "+target)
			}
			provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
			if err != nil {
				t.Fatal(err)
			}
			submission := durableSubmission(t, provider, "revoked-while-running", jobwire.ProfileVideo)
			if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
				t.Fatalf("submit %+v %v", accepted, submitErr)
			}
			stop := executeUntilCleanup(t, provider)
			defer stop()
			final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
			if final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != refusal {
				t.Fatalf("final %+v", final)
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.submits != 1 || backend.polls != 1 || backend.cancels != 1 {
				t.Fatalf("submits %d polls %d cancels %d, want 1 1 1", backend.submits, backend.polls, backend.cancels)
			}
			cancelledMu.Lock()
			defer cancelledMu.Unlock()
			want := []string{}
			if refusal == "credential:not_permitted" {
				want = []string{caller.Program + " " + ImageContract + " replicate api.replicate.com"}
			}
			if !slices.Equal(cancelled, want) && !(len(cancelled) == 0 && len(want) == 0) {
				t.Fatalf("cancel-time credential use told %q, want %q", cancelled, want)
			}
		})
	}
}
