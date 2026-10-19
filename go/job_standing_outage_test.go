package inference

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
)

// standingJob is an inference job accepted under a recorded subject from an
// acceptance provider whose standing decision (abstraction-job JOB-A18) a
// test switches between permitted and unavailable. Its upstream work runs
// until finish.
type standingJob struct {
	backend *fakeDurableBackend
	fixture *durableFixture
	p       *acceptanceprovider.Provider
	root    string
	binding acceptanceprovider.Binding
	outage  atomic.Bool
	// unansweredRenewals counts the lease renewals decided while the outage
	// was on.
	unansweredRenewals atomic.Int64
	identity           acceptance.RequestIdentity
	op                 string
}

func newStandingJob(t *testing.T, ttl time.Duration) *standingJob {
	t.Helper()
	j := &standingJob{backend: &fakeDurableBackend{poll: durableJobStatus{State: durableJobRunning}}}
	j.fixture = newDurableFixture(t, j.backend)
	j.fixture.execution.leaseTTL = ttl
	j.fixture.execution.pollInterval = 10 * time.Millisecond
	j.fixture.execution.SetOperationSubjectResolver(func(context.Context, string, string, string) (Subject, ContentOutcome) {
		return caller, ContentResolved
	})
	j.root = t.TempDir()
	p, err := acceptanceprovider.OpenWithExecutor(j.root, "owner", boundInferenceExecutor{j.fixture.execution})
	if err != nil {
		t.Fatal(err)
	}
	j.p = p
	p.SetStanding(func(ctx context.Context, _ acceptanceprovider.AuthenticatedSubject, _ string) (string, error) {
		if j.outage.Load() {
			if acceptanceprovider.StandingCallOf(ctx) == acceptanceprovider.StandingRenew {
				j.unansweredRenewals.Add(1)
			}
			return "", errors.New("rights unavailable")
		}
		return "", nil
	})
	executable := filepath.Join(t.TempDir(), "caller")
	subject := acceptanceprovider.AuthenticatedSubject{AccountKind: "posix", Account: caller.Account, Executable: executable, Program: caller.Program}
	scope, err := acceptanceprovider.LocalSubjectScope(subject.AccountKind, subject.Account, executable)
	if err != nil {
		t.Fatal(err)
	}
	j.binding = acceptanceprovider.Binding{Scope: scope, Subject: &subject, Origin: "local"}
	submission := durableSubmission(t, p, "standing-outage", jobwire.ProfileVideo)
	j.identity = submission.Identity
	accepted, err := p.BindBinding(j.binding).Submit(submission)
	if err != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	j.op = accepted.Receipt.OperationID
	t.Cleanup(executeUntilCleanup(t, p))
	j.waitPolls(t, 3)
	return j
}

// epoch is the job's current lease epoch, which a lost and reclaimed lease
// moves.
func (j *standingJob) epoch(t *testing.T) int64 {
	t.Helper()
	store, err := job.NewFileStore(j.root)
	if err != nil {
		t.Fatal(err)
	}
	r, err := store.Load(j.op)
	if err != nil {
		t.Fatal(err)
	}
	return r.Lease.Epoch
}

func (j *standingJob) polls() int {
	j.backend.mu.Lock()
	defer j.backend.mu.Unlock()
	return j.backend.polls
}

func (j *standingJob) waitPolls(t *testing.T, more int) {
	t.Helper()
	want := j.polls() + more
	for deadline := time.Now().Add(5 * time.Second); j.polls() < want; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the upstream work was not polled %d more times", more)
		}
	}
}

func (j *standingJob) finish(t *testing.T) acceptance.OperationSnapshot {
	t.Helper()
	video := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	j.backend.mu.Lock()
	j.backend.poll = durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: video, MediaType: "video/mp4"}}}
	j.backend.mu.Unlock()
	return waitDurable(t, j.p.BindOperationsBinding(j.binding), j.identity, true)
}

// JOB-A18 with the inference executor: a standing decision that cannot be
// obtained for less than the lease's lifetime ends no upstream work. The job
// keeps polling its one submission through the outage and completes once the
// decision answers again.
func TestAShortStandingOutageEndsNoInferenceJob(t *testing.T) {
	j := newStandingJob(t, 600*time.Millisecond)
	before := j.epoch(t)
	j.outage.Store(true)
	// Longer than one renewal interval (a third of the lease), shorter
	// than the lease.
	time.Sleep(350 * time.Millisecond)
	j.outage.Store(false)
	// The outage is only shown harmless if renewals met it.
	if n := j.unansweredRenewals.Load(); n == 0 {
		t.Fatal("no lease renewal was decided during the outage")
	}
	j.waitPolls(t, 3)
	// A lost lease is claimed again under a new epoch before polling resumes.
	if after := j.epoch(t); after != before {
		t.Fatalf("the outage cost the job its lease: epoch %d, then %d", before, after)
	}
	final := j.finish(t)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	j.backend.mu.Lock()
	defer j.backend.mu.Unlock()
	if j.backend.submits != 1 || j.backend.reconciles != 0 || j.backend.cancels != 0 {
		t.Fatalf("the outage interrupted the job: submits %d reconciles %d cancels %d", j.backend.submits, j.backend.reconciles, j.backend.cancels)
	}
}

// JOB-A18 with the inference executor: an outage that outlasts the lease
// ends the lease at its expiry, so the job stops polling and nothing claims
// it again while the decision cannot be obtained; it ends nothing, and once
// decisions answer the job resumes its recorded upstream work.
func TestALongStandingOutageLapsesAnInferenceJobsLease(t *testing.T) {
	j := newStandingJob(t, 600*time.Millisecond)
	j.outage.Store(true)
	store, err := job.NewFileStore(j.root)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if r, err := store.Load(j.op); err == nil && !r.Lease.Held(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lease did not lapse")
		}
	}
	time.Sleep(100 * time.Millisecond)
	stopped := j.polls()
	time.Sleep(400 * time.Millisecond)
	if j.polls() != stopped {
		t.Fatal("the job kept polling after its lease lapsed")
	}
	if r, err := store.Load(j.op); err != nil || r.State.Terminal() || r.Lease.Held(time.Now()) {
		t.Fatalf("during the outage the job was ended or claimed again: %+v %v", r, err)
	}
	j.outage.Store(false)
	j.waitPolls(t, 2)
	if final := j.finish(t); final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	j.backend.mu.Lock()
	defer j.backend.mu.Unlock()
	if j.backend.submits != 1 || j.backend.cancels != 0 {
		t.Fatalf("submits %d cancels %d after resuming", j.backend.submits, j.backend.cancels)
	}
}
