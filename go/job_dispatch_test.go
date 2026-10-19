package inference

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	router "github.com/openabstractions/abstraction-router/go"
)

// hostGatedBackend runs a job on a gated host until its gate opens and
// finishes a job on any other host at the first poll. It counts the jobs
// each host runs at once, from submission to the poll that ends them.
type hostGatedBackend struct {
	*fakeDurableBackend
	gates    map[string]chan struct{}
	polling  chan string
	gateMu   sync.Mutex
	active   map[string]int
	maxAtOne map[string]int
	byHandle map[string]string
	handles  int
	pollsOf  map[string]int
	// lost names operations whose submission reply is lost.
	lost map[string]bool
	// succeeded runs for each poll that reports success, before it returns.
	succeeded func(handle string)
}

var dispatchVideo = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")

func newHostGatedBackend(gated ...string) *hostGatedBackend {
	b := &hostGatedBackend{fakeDurableBackend: &fakeDurableBackend{}, gates: map[string]chan struct{}{}, polling: make(chan string, 64),
		active: map[string]int{}, maxAtOne: map[string]int{}, byHandle: map[string]string{}, pollsOf: map[string]int{}, lost: map[string]bool{}}
	for _, name := range gated {
		b.gates[name] = make(chan struct{})
	}
	return b
}

func (b *hostGatedBackend) Submit(ctx context.Context, submission durableJobSubmission) (durableJobStatus, error) {
	status, err := b.fakeDurableBackend.Submit(ctx, submission)
	if err != nil {
		return status, err
	}
	host := submission.host
	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	if b.lost[submission.operation] {
		return durableJobStatus{}, errors.New("reply lost")
	}
	b.handles++
	status.Handle = host.Name + "-" + submission.operation
	b.byHandle[status.Handle] = host.Name
	b.active[host.Name]++
	b.maxAtOne[host.Name] = max(b.maxAtOne[host.Name], b.active[host.Name])
	return status, nil
}

func (b *hostGatedBackend) Poll(_ context.Context, host *router.Host, handle string, _ jobwire.Request, _ map[string]string) (durableJobStatus, error) {
	b.gateMu.Lock()
	gate := b.gates[host.Name]
	b.pollsOf[host.Name]++
	b.pollsOf[handle]++
	b.gateMu.Unlock()
	select {
	case b.polling <- host.Name:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		default:
			return durableJobStatus{State: durableJobRunning, Handle: handle}, nil
		}
	}
	b.gateMu.Lock()
	b.active[host.Name]--
	succeeded := b.succeeded
	b.gateMu.Unlock()
	if succeeded != nil {
		succeeded(handle)
	}
	return durableJobStatus{State: durableJobSucceeded, Handle: handle, Outputs: []durableJobOutput{{Data: dispatchVideo, MediaType: "video/mp4"}}}, nil
}

func (b *hostGatedBackend) open(host string) {
	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	close(b.gates[host])
}

func (b *hostGatedBackend) polls(host string) int {
	b.gateMu.Lock()
	defer b.gateMu.Unlock()
	return b.pollsOf[host]
}

// newDispatchFixture serves two hosted servers, replicate and replicate-b,
// each chosen by its own credential.
func newDispatchFixture(t *testing.T, backend *hostGatedBackend) *durableFixture {
	t.Helper()
	fixture := newDurableFixture(t, backend.fakeDurableBackend)
	routes := fixture.provider.cfg.Router
	routes.SetHosts(append(routes.Hosts(), router.NewHosted("replicate-b", "https://b.replicate.example", router.WireReplicatePredictions, "replicate-b"))...)
	routes.Survey()
	fixture.execution.resolveSubject = func(_ context.Context, scope, credential string) (Subject, ContentOutcome) {
		if scope != "caller-scope" || (credential != "replicate" && credential != "replicate-b") {
			return Subject{}, ContentForbidden
		}
		return caller, ContentResolved
	}
	fixture.execution.backends = []durableJobBackend{backend}
	return fixture
}

func submitDispatchJob(t *testing.T, fixture *durableFixture, store job.Store, id, credential string) {
	t.Helper()
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 8000, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "a lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: credential, Extensions: map[string]string{},
	}}}
	prepared, _, err := fixture.execution.PrepareScoped("caller-scope", id, InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Submit(job.Record{ID: id, Kind: InferenceJobKind, State: job.StatePending, Spec: prepared}); err != nil {
		t.Fatal(err)
	}
}

// dispatchFailsafe bounds every wait in these tests for an event that is
// signalled; it only ends a test whose event never comes. No test measures
// elapsed time against it.
const dispatchFailsafe = 60 * time.Second

func serveDispatch(t *testing.T, fixture *durableFixture, store job.Store) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fixture.execution.Serve(ctx, store) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(dispatchFailsafe):
			t.Fatal("Serve did not return after shutdown")
			return nil
		}
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func waitPolling(t *testing.T, backend *hostGatedBackend, host string) {
	t.Helper()
	deadline := time.After(dispatchFailsafe)
	for {
		select {
		case name := <-backend.polling:
			if name == host {
				return
			}
		case <-deadline:
			t.Fatalf("no job polled %s", host)
		}
	}
}

func waitRecord(t *testing.T, store job.Store, id string, what string, done func(*job.Record, inferenceJobCheckpoint) bool) *job.Record {
	t.Helper()
	deadline := time.Now().Add(dispatchFailsafe)
	for time.Now().Before(deadline) {
		record, err := store.Load(id)
		if err == nil {
			var checkpoint inferenceJobCheckpoint
			if record.DecodeCheckpoint(&checkpoint) == nil && done(record, checkpoint) {
				return record
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	record, _ := store.Load(id)
	if record == nil {
		t.Fatalf("%s: %s never happened; no record", id, what)
	}
	t.Fatalf("%s: %s never happened; state %s epoch %d owner %q checkpoint %s", id, what, record.State, record.Lease.Epoch, record.Lease.Owner, record.Checkpoint)
	return nil
}

func complete(record *job.Record, _ inferenceJobCheckpoint) bool {
	return record.State == job.StateComplete
}

// TestInferenceJobOnOneHostDoesNotDelayAnother: a long job on host A keeps
// running while a job on host B is claimed, run and completed.
func TestInferenceJobOnOneHostDoesNotDelayAnother(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	store := job.NewMemoryStore()
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "long-on-a", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "short-on-b", "replicate-b")
	waitRecord(t, store, "short-on-b", "completion while host A runs", complete)
	if record, _ := store.Load("long-on-a"); record.State.Terminal() {
		t.Fatalf("the long job ended early: %+v", record)
	}
	backend.open("replicate")
	waitRecord(t, store, "long-on-a", "completion", complete)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// TestInferenceJobsOnOneHostRunOneAtATime: with one slot per server,
// a second job on the same server waits, visibly host:busy and unsent, until
// the first ends; then it runs. Each job is sent once.
func TestInferenceJobsOnOneHostRunOneAtATime(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	store := job.NewMemoryStore()
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "first", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "second", "replicate")
	waitRecord(t, store, "second", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	backend.mu.Lock()
	submits := backend.submits
	backend.mu.Unlock()
	if submits != 1 {
		t.Fatalf("the queued job was sent: submits=%d", submits)
	}
	backend.open("replicate")
	waitRecord(t, store, "first", "completion", complete)
	waitRecord(t, store, "second", "completion", complete)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	submits = backend.submits
	backend.mu.Unlock()
	backend.gateMu.Lock()
	defer backend.gateMu.Unlock()
	if submits != 2 || backend.maxAtOne["replicate"] != 1 {
		t.Fatalf("submits=%d, at once on one server=%d", submits, backend.maxAtOne["replicate"])
	}
}

// TestInferenceJobBackedOffUncertainWorkHoldsNoSlot: work whose submission
// reply was lost waits out its reconciliation backoff unclaimed, and the next
// job on the same one-slot server runs meanwhile.
func TestInferenceJobBackedOffUncertainWorkHoldsNoSlot(t *testing.T) {
	backend := newHostGatedBackend()
	backend.lost["uncertain"] = true
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	frozen := time.Now()
	fixture.execution.clock = func() time.Time { return frozen }
	store := job.NewMemoryStore()
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "uncertain", "replicate")
	waitRecord(t, store, "uncertain", "upstream:uncertain", func(r *job.Record, c inferenceJobCheckpoint) bool {
		return c.Waiting == "upstream:uncertain" && r.Lease.Owner == ""
	})
	submitDispatchJob(t, fixture, store, "next", "replicate")
	waitRecord(t, store, "next", "completion during the backoff", complete)
	if record, _ := store.Load("uncertain"); record.State.Terminal() || record.Lease.Owner != "" {
		t.Fatalf("the backed-off job: state %s owner %q", record.State, record.Lease.Owner)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 2 || backend.reconciles != 0 {
		t.Fatalf("submits=%d reconciles=%d", backend.submits, backend.reconciles)
	}
}

// TestInferenceJobLimitsDefaultToOnePerLocalServer: a local engine runs one
// job at a time unless configured; hosted services have their own limit.
func TestInferenceJobLimitsDefaultToOnePerLocalServer(t *testing.T) {
	e := &JobExecution{}
	local, hosted := &router.Host{Name: "lemonade"}, &router.Host{Name: "replicate", Hosted: true}
	if e.hostLimit(local) != 1 || e.hostLimit(hosted) != defaultHostedJobsPerHost {
		t.Fatalf("defaults local=%d hosted=%d", e.hostLimit(local), e.hostLimit(hosted))
	}
	e.SetHostConcurrency(2, 0)
	if e.hostLimit(local) != 2 || e.hostLimit(hosted) != defaultHostedJobsPerHost {
		t.Fatalf("configured local=%d hosted=%d", e.hostLimit(local), e.hostLimit(hosted))
	}
}

// TestInferenceJobShutdownReleasesRunningAndQueuedJobs: Serve returns only
// after the running job and the job queued behind it let their leases go;
// the running one keeps its handle, the queued one was never sent. Each
// release takes 200 ms, so a Serve that returned before its jobs let go
// would leave them leased (INF-G9).
func TestInferenceJobShutdownReleasesRunningAndQueuedJobs(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	store := &slowReleaseStore{Store: job.NewMemoryStore()}
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "running", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "queued", "replicate")
	waitRecord(t, store, "queued", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	for id, phase := range map[string]string{"running": "submitted", "queued": ""} {
		record, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint inferenceJobCheckpoint
		if err := record.DecodeCheckpoint(&checkpoint); err != nil {
			t.Fatal(err)
		}
		if record.Lease.Owner != "" || record.State.Terminal() || checkpoint.Phase != phase || checkpoint.Waiting != "service:stopped" {
			t.Fatalf("%s after shutdown: state %s owner %q checkpoint %+v", id, record.State, record.Lease.Owner, checkpoint)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("submits=%d", backend.submits)
	}
}

// TestInferenceJobShutdownDuringAPollDecisionSaysStopped: shutdown that
// cancels a running job's per-poll rights decision lets the job go with
// service:stopped, the shutdown's word, and not rights:unavailable (INF-G9).
// The test above met this window about once in fifteen runs on Windows.
func TestInferenceJobShutdownDuringAPollDecisionSaysStopped(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	deciding := make(chan struct{}, 1)
	fixture.provider.cfg.Standing = func(ctx context.Context, _ Subject, _, _ string) (string, error) {
		select {
		case deciding <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	store := job.NewMemoryStore()
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "running", "replicate")
	select {
	case <-deciding:
	case <-time.After(dispatchFailsafe):
		t.Fatal("the running job never decided its poll")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	record, err := store.Load("running")
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint inferenceJobCheckpoint
	if err := record.DecodeCheckpoint(&checkpoint); err != nil {
		t.Fatal(err)
	}
	if record.Lease.Owner != "" || record.State.Terminal() || checkpoint.Phase != "submitted" || checkpoint.Waiting != "service:stopped" {
		t.Fatalf("after shutdown: state %s owner %q checkpoint %+v", record.State, record.Lease.Owner, checkpoint)
	}
}

// TestInferenceJobCancelWhileQueuedSendsNothing: a job cancelled while it
// waits for its server ends cancelled without reaching the upstream.
func TestInferenceJobCancelWhileQueuedSendsNothing(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	store := job.NewMemoryStore()
	serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "running", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "queued", "replicate")
	waitRecord(t, store, "queued", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	if _, err := store.SetIntent("queued", job.WantCancel, "caller"); err != nil {
		t.Fatal(err)
	}
	waitRecord(t, store, "queued", "cancellation", func(r *job.Record, _ inferenceJobCheckpoint) bool { return r.State == job.StateCancelled })
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("submits=%d", backend.submits)
	}
}

// lostLeaseStore loses every lease once lose closes: renewals and new
// claims fail.
type lostLeaseStore struct {
	job.Store
	lose chan struct{}
}

func (s *lostLeaseStore) lost() bool {
	select {
	case <-s.lose:
		return true
	default:
		return false
	}
}

func (s *lostLeaseStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	if s.lost() {
		return nil, job.ErrLeaseExpiry
	}
	return s.Store.Renew(id, epoch, ttl)
}

func (s *lostLeaseStore) Claim(id, owner string, ttl time.Duration) (*job.Record, error) {
	if s.lost() {
		return nil, job.ErrLeaseHeld
	}
	return s.Store.Claim(id, owner, ttl)
}

// TestInferenceJobLeaseLossStopsItsExecution: an executing job whose lease
// cannot be renewed stops polling, and its server slot passes on.
func TestInferenceJobLeaseLossStopsItsExecution(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.leaseTTL = 150 * time.Millisecond
	store := &lostLeaseStore{Store: job.NewMemoryStore(), lose: make(chan struct{})}
	serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "lease-lost", "replicate")
	waitPolling(t, backend, "replicate")
	close(store.lose)
	// The store refuses every claim from now on: once the execution has
	// ended and given its slot back, nothing polls again.
	waitExecutionEnded(t, fixture.execution, "lease-lost", dispatchFailsafe, "the job kept executing after its lease was lost")
	fixture.execution.slotsMu.Lock()
	slots := len(fixture.execution.hosts)
	fixture.execution.slotsMu.Unlock()
	if slots != 0 {
		t.Fatalf("%d server slots still held after the lease was lost", slots)
	}
}

// waitExecutionEnded waits until this runtime no longer executes id. While
// an execution runs, its id stays in the executing set; a dispatch that only
// tries to claim adds the id after the execution ended, so one observed
// absence proves the end.
func waitExecutionEnded(t *testing.T, e *JobExecution, id string, within time.Duration, failure string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		e.slotsMu.Lock()
		running := e.executing[id]
		e.slotsMu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// slowReleaseStore takes 200 ms to let a lease go.
type slowReleaseStore struct{ job.Store }

func (s *slowReleaseStore) Release(id string, epoch int64) error {
	time.Sleep(200 * time.Millisecond)
	return s.Store.Release(id, epoch)
}

// stallRenewStore lets a lease lapse in the store while its holder still
// executes: once stalled, Renew and Update block until released, and the
// execution that lost its lease stays in its last store write.
type stallRenewStore struct {
	job.Store
	stalled atomic.Bool
	release chan struct{}
	claims  atomic.Int32
	// orphaned counts the sweeps that found the job claimable once stalled.
	orphaned atomic.Int32
}

func (s *stallRenewStore) Orphans() ([]*job.Record, error) {
	records, err := s.Store.Orphans()
	if s.stalled.Load() {
		for _, record := range records {
			if record.ID == "held" {
				s.orphaned.Add(1)
			}
		}
	}
	return records, err
}

func (s *stallRenewStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	if s.stalled.Load() {
		<-s.release
		return nil, job.ErrLeaseExpiry
	}
	return s.Store.Renew(id, epoch, ttl)
}

func (s *stallRenewStore) Update(id string, epoch int64, mutate func(*job.Record) error) (*job.Record, error) {
	if s.stalled.Load() {
		<-s.release
	}
	return s.Store.Update(id, epoch, mutate)
}

func (s *stallRenewStore) Claim(id, owner string, ttl time.Duration) (*job.Record, error) {
	s.claims.Add(1)
	return s.Store.Claim(id, owner, ttl)
}

// TestInferenceJobLapsedLeaseIsNotReclaimedWhileExecuting: a job whose lease
// lapses while this runtime still executes it is not claimed and executed a
// second time here. Two sweeps find it claimable: the first one's dispatch
// has ended when the second one lists the store.
func TestInferenceJobLapsedLeaseIsNotReclaimedWhileExecuting(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.leaseTTL = 150 * time.Millisecond
	store := &stallRenewStore{Store: job.NewMemoryStore(), release: make(chan struct{})}
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "held", "replicate")
	waitPolling(t, backend, "replicate")
	store.stalled.Store(true)
	for deadline := time.Now().Add(dispatchFailsafe); store.orphaned.Load() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			close(store.release)
			t.Fatal("no two sweeps found the lapsed job")
		}
	}
	claims := store.claims.Load()
	close(store.release)
	backend.open("replicate")
	_ = stop()
	if claims != 1 {
		t.Fatalf("the job was claimed %d times while its first execution ran", claims)
	}
}

// TestInferenceJobRefusedAtItsTurnGivesTheSlotBack: a queued job refused when
// it is planned again at its turn gives its slot back; the next job on the
// same one-slot server runs (INF-G9).
func TestInferenceJobRefusedAtItsTurnGivesTheSlotBack(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	var forbid atomic.Bool
	fixture.execution.resolveSubject = func(_ context.Context, scope, credential string) (Subject, ContentOutcome) {
		if forbid.Load() || scope != "caller-scope" || (credential != "replicate" && credential != "replicate-b") {
			return Subject{}, ContentForbidden
		}
		return caller, ContentResolved
	}
	store := job.NewMemoryStore()
	stop := serveDispatch(t, fixture, store)
	submitDispatchJob(t, fixture, store, "first", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "second", "replicate")
	waitRecord(t, store, "second", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	forbid.Store(true)
	backend.open("replicate")
	waitRecord(t, store, "first", "completion", complete)
	waitRecord(t, store, "second", "refusal at its turn", func(r *job.Record, _ inferenceJobCheckpoint) bool { return r.State == job.StateFailed })
	forbid.Store(false)
	submitDispatchJob(t, fixture, store, "third", "replicate")
	waitRecord(t, store, "third", "completion after the refused job", complete)
	_ = stop()
}

// blockRenewStore hangs one job's renewals once blocked, until released.
type blockRenewStore struct {
	job.Store
	id      string
	blocked atomic.Bool
	release chan struct{}
}

func (s *blockRenewStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	if id == s.id && s.blocked.Load() {
		<-s.release
		return nil, job.ErrLeaseExpiry
	}
	return s.Store.Renew(id, epoch, ttl)
}

// Claim refuses the blocked job: the test watches its first execution end,
// and a second one would start polling again.
func (s *blockRenewStore) Claim(id, owner string, ttl time.Duration) (*job.Record, error) {
	if id == s.id && s.blocked.Load() {
		return nil, job.ErrLeaseHeld
	}
	return s.Store.Claim(id, owner, ttl)
}

// leaseDeadlineBound is how long the hung-renewal tests give a job to stop
// after its renewal hangs. Their leases last 300 or 600 ms; the sweep claims
// each job for 30 s (job_dispatch.go). A deadline that is never re-armed
// after a renewal stops the job only when that claim lapses, past this
// bound, and a job stopped by its lease's deadline stays far below it under
// load.
const leaseDeadlineBound = 10 * time.Second

// waitSubmits waits until the backend has seen n submissions, for at most
// within.
func waitSubmits(t *testing.T, backend *hostGatedBackend, n int, within time.Duration, failure string) {
	t.Helper()
	for deadline := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
		backend.mu.Lock()
		submits := backend.submits
		backend.mu.Unlock()
		if submits >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
	}
}

// TestInferenceJobStalledRenewalStopsTheJobAtItsLeaseDeadline: a running job
// whose renewal hangs stops once its lease's duration has passed without a
// renewal, before another runtime could claim it, and its server slot passes
// to the next job (INF-G9).
func TestInferenceJobStalledRenewalStopsTheJobAtItsLeaseDeadline(t *testing.T) {
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	fixture.execution.leaseTTL = 300 * time.Millisecond
	store := &blockRenewStore{Store: job.NewMemoryStore(), id: "stalled", release: make(chan struct{})}
	stop := serveDispatch(t, fixture, store)
	unblock := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(unblock) // before Serve's stop, which waits for the stalled job
	submitDispatchJob(t, fixture, store, "stalled", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "next", "replicate")
	waitRecord(t, store, "next", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	store.blocked.Store(true)
	// Only the lease deadline can end the stalled job: its renewal never
	// returns until the test lets it go.
	hung := time.Now()
	waitSubmits(t, backend, 2, leaseDeadlineBound, "the job whose renewal hangs kept its server past its lease")
	waitExecutionEnded(t, fixture.execution, "stalled", leaseDeadlineBound-time.Since(hung), "the job whose renewal hangs kept executing past its lease")
	backend.open("replicate")
	waitRecord(t, store, "next", "completion", complete)
	unblock()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// hangRenewStore hangs one job's renewals once hang is set, until release;
// entered receives each renewal that starts hanging.
type hangRenewStore struct {
	job.Store
	id      string
	hang    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newHangRenewStore(id string) *hangRenewStore {
	return &hangRenewStore{Store: job.NewMemoryStore(), id: id, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *hangRenewStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	if id == s.id && s.hang.Load() {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
		return nil, job.ErrLeaseExpiry
	}
	return s.Store.Renew(id, epoch, ttl)
}

// TestInferenceJobHungRenewalAtPublishGivesTheSlotBackWithinTheLease: once
// the upstream reports success, the job's renewals hang in the store. The
// publish waits for a renewal no longer than the lease's duration, and the
// next job on the one-slot server is sent while the renewal still hangs:
// only the lease deadline can free the slot (INF-G9).
func TestInferenceJobHungRenewalAtPublishGivesTheSlotBackWithinTheLease(t *testing.T) {
	const ttl = 600 * time.Millisecond
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.SetHostConcurrency(1, 1)
	fixture.execution.leaseTTL = ttl
	store := newHangRenewStore("first")
	stop := serveDispatch(t, fixture, store)
	unblock := sync.OnceFunc(func() { close(store.release) })
	t.Cleanup(unblock) // before Serve's stop
	backend.gateMu.Lock()
	backend.succeeded = func(handle string) {
		if handle == "replicate-first" {
			store.hang.Store(true)
		}
	}
	backend.gateMu.Unlock()
	submitDispatchJob(t, fixture, store, "first", "replicate")
	waitPolling(t, backend, "replicate")
	submitDispatchJob(t, fixture, store, "next", "replicate")
	waitRecord(t, store, "next", "host:busy", func(_ *job.Record, c inferenceJobCheckpoint) bool { return c.Waiting == hostBusyWaiting })
	backend.open("replicate")
	// The renewal starts hanging at the first poll that reports success,
	// after the gate opens.
	waitSubmits(t, backend, 2, leaseDeadlineBound, "the next job was not sent while the first job's renewal hung")
	unblock()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// TestInferenceJobShutdownReturnsWithinTheLeaseWhileARenewalHangs: Serve
// returns while a running job's renewal still hangs in the store, at the
// latest by the lease's deadline; the hung store call may return later
// (INF-G9).
func TestInferenceJobShutdownReturnsWithinTheLeaseWhileARenewalHangs(t *testing.T) {
	const ttl = 600 * time.Millisecond
	backend := newHostGatedBackend("replicate")
	fixture := newDispatchFixture(t, backend)
	fixture.execution.leaseTTL = ttl
	store := newHangRenewStore("running")
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- fixture.execution.Serve(ctx, store) }()
	t.Cleanup(func() {
		close(store.release)
		cancel()
		<-served
	})
	submitDispatchJob(t, fixture, store, "running", "replicate")
	waitPolling(t, backend, "replicate")
	store.hang.Store(true)
	select {
	case <-store.entered:
	case <-time.After(dispatchFailsafe):
		t.Fatal("no renewal reached the store")
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
		served <- nil // for the cleanup
	case <-time.After(dispatchFailsafe):
		t.Fatal("Serve did not return after shutdown while a renewal hung")
	}
}
