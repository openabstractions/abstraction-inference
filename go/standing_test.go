package inference

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
	router "github.com/openabstractions/abstraction-router/go"
)

type standingAuthorityKey struct{}

func TestRecheckCheckedPreservesUnavailableAndReportsDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			unavailable := errors.New("cannot read standing decision")
			p := &Provider{ops: map[string]*operation{}, cfg: Config{Standing: func(ctx context.Context, _ Subject, _, _ string) (string, error) {
				if deadline {
					<-ctx.Done()
					return "rights:denied", nil
				}
				return "", unavailable
			}}}
			op := newOperation("live", caller, &router.Host{Name: "owned"}, "model", "", 1, time.Now())
			op.cancel = func() {}
			p.ops[op.id] = op
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
			defer cancel()
			err := p.RecheckChecked(ctx)
			if err == nil || deadline && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("checked result: %v", err)
			}
			if op.stopped() != "" {
				t.Fatal("uncertain decision stopped operation")
			}
			p.cfg.Standing = func(context.Context, Subject, string, string) (string, error) { return "rights:denied", nil }
			if err := p.RecheckChecked(t.Context()); err != nil || op.stopped() != "rights:denied" {
				t.Fatalf("refusal not settled: %q %v", op.stopped(), err)
			}
		})
	}
}

func TestRemoteOperationStandingRetainsValuesAndCurrentCancellation(t *testing.T) {
	p := &Provider{}
	ingress, cancelIngress := context.WithCancel(WithRemote(context.WithValue(context.Background(), standingAuthorityKey{}, "original"), "peer", "tls"))
	op := newOperation("owned", caller, &router.Host{Name: "owned"}, "model", "", 1, time.Now())
	p.bindOperation(ingress, op)
	cancelIngress()
	current, cancelCurrent := context.WithDeadline(context.WithValue(context.Background(), standingAuthorityKey{}, "foreign"), time.Now().Add(time.Hour))
	defer cancelCurrent()
	standing := op.standingContext(current)
	if standing.Value(standingAuthorityKey{}) != "original" || standing.Err() != nil || standing.Done() != current.Done() {
		t.Fatal("ingress lifetime or foreign authority retained")
	}
	got, ok := standing.Deadline()
	want, wantOK := current.Deadline()
	if !ok || !wantOK || !got.Equal(want) {
		t.Fatal("current deadline lost")
	}
	cancelCurrent()
	if standing.Err() != context.Canceled {
		t.Fatal("current cancellation lost")
	}
	legacy := newOperation("legacy", caller, op.host, "model", "", 1, time.Now())
	p.bindOperation(WithRemote(context.Background(), "peer", "tls"), legacy)
	if legacy.standingContext(current).Value(standingAuthorityKey{}) != nil {
		t.Fatal("legacy operation borrowed current authority")
	}
}

func TestRemoteRecheckDoesNotShareDistinctOperationAuthorities(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			calls := map[string]int{}
			p := &Provider{ops: map[string]*operation{}, cfg: Config{Standing: func(ctx context.Context, _ Subject, _, _ string) (string, error) {
				value, _ := ctx.Value(standingAuthorityKey{}).(string)
				calls[value]++
				if value == "minimum-two" {
					return "rights:unavailable", nil
				}
				return "", nil
			}}}
			for _, value := range []string{"minimum-one", "minimum-two"} {
				op := newOperation(value, caller, &router.Host{Name: "same"}, "model", "", 1, time.Now())
				op.cancel = func() {}
				ctx := context.Background()
				if remote {
					ctx = WithRemote(context.WithValue(ctx, standingAuthorityKey{}, value), "peer", "tls")
				}
				p.bindOperation(ctx, op)
				p.ops[value] = op
			}
			p.Recheck(context.Background())
			if remote {
				if calls["minimum-one"] != 1 || calls["minimum-two"] != 1 || p.ops["minimum-one"].stopped() != "" || p.ops["minimum-two"].stopped() != "rights:unavailable" {
					t.Fatalf("remote decisions=%v", calls)
				}
			} else if calls[""] != 1 {
				t.Fatalf("local dedup changed: %v", calls)
			}
		})
	}
}

func TestRemoteStandingOfUsesCurrentEvaluationContext(t *testing.T) {
	p := &Provider{}
	op := newOperation("owned", caller, &router.Host{Name: "owned"}, "model", "", 1, time.Now())
	ingress, cancel := context.WithCancel(WithRemote(context.WithValue(context.Background(), standingAuthorityKey{}, "authority"), "peer", "tls"))
	p.bindOperation(ingress, op)
	cancel()
	current, stop := context.WithCancel(context.Background())
	stop()
	p.cfg.Standing = func(ctx context.Context, _ Subject, _, _ string) (string, error) {
		if ctx.Err() != context.Canceled || ctx.Done() != current.Done() || ctx.Value(standingAuthorityKey{}) != "authority" {
			t.Fatal("standing used ingress lifecycle")
		}
		return "", nil
	}
	if _, err := p.standingOf(current, op); err == nil {
		t.Fatal("canceled recheck accepted")
	}
}

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
