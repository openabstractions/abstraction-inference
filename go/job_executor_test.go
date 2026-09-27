package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
	router "github.com/openabstractions/abstraction-router/go"
)

type fakeDurableBackend struct {
	mu             sync.Mutex
	submits, polls int
	reconciles     int
	cancels        int
	uncertain      bool
	blockSubmit    bool
	submitStarted  chan struct{}
	submitCanceled chan struct{}
	poll           durableJobStatus
	pollHook       func()
	polled         chan struct{}
}

func (*fakeDurableBackend) Supports(*router.Host, jobwire.Request) bool { return true }
func (b *fakeDurableBackend) Reconcile(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error) {
	b.mu.Lock()
	b.reconciles++
	b.mu.Unlock()
	return durableJobStatus{State: durableJobUnknown}, nil
}
func (b *fakeDurableBackend) Submit(ctx context.Context, _ *router.Host, _ string, _ string, _ jobwire.Request, _ map[string]string) (durableJobStatus, error) {
	b.mu.Lock()
	b.submits++
	block, started, canceled := b.blockSubmit, b.submitStarted, b.submitCanceled
	b.mu.Unlock()
	if block {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		if canceled != nil {
			close(canceled)
		}
		return durableJobStatus{}, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.uncertain {
		return durableJobStatus{}, errors.New("reply lost")
	}
	return durableJobStatus{State: durableJobRunning, Handle: "remote-1"}, nil
}

type failingRenewStore struct {
	job.Store
	fail    <-chan struct{}
	renewed chan<- struct{}
	failErr error
}

type failingCheckpointStore struct {
	job.Store
	failAfter int
}

func (s *failingCheckpointStore) Update(id string, epoch int64, mutate func(*job.Record) error) (*job.Record, error) {
	if s.failAfter > 0 {
		s.failAfter--
		if s.failAfter == 0 {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return s.Store.Update(id, epoch, mutate)
}

func (s *failingRenewStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	select {
	case <-s.fail:
		return nil, s.failErr
	default:
	}
	record, err := s.Store.Renew(id, epoch, ttl)
	if err == nil && s.renewed != nil {
		select {
		case s.renewed <- struct{}{}:
		default:
		}
	}
	return record, err
}
func (b *fakeDurableBackend) Poll(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error) {
	b.mu.Lock()
	b.polls++
	result := b.poll
	hook := b.pollHook
	b.pollHook = nil
	if b.polled != nil {
		select {
		case b.polled <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return result, nil
}
func (b *fakeDurableBackend) Cancel(context.Context, *router.Host, string, map[string]string) error {
	b.mu.Lock()
	b.cancels++
	b.mu.Unlock()
	return nil
}

type durableFixture struct {
	execution         *JobExecution
	backend           *fakeDurableBackend
	provider          *Provider
	committed         [][]byte
	records           []Record
	recorded          chan struct{}
	permitted         bool
	policyUnavailable bool
	consumer          string
	mu                sync.Mutex
}

func newDurableFixture(t *testing.T, backend *fakeDurableBackend) *durableFixture {
	t.Helper()
	host := router.NewHosted("replicate", "https://api.replicate.com", router.WireReplicatePredictions, "replicate")
	routes := router.New(host)
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		return map[string]string{"Authorization": "Bearer listing"}, nil
	})
	routes.Survey()
	fixture := &durableFixture{backend: backend, permitted: true, recorded: make(chan struct{}, 16)}
	provider, err := New(Config{Router: routes,
		Decide: func(context.Context, Subject, string, string) (string, error) {
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.policyUnavailable {
				return "", errors.New("rights backend unavailable")
			}
			if fixture.permitted {
				return "permitted", nil
			}
			return "denied", nil
		},
		Apply: func(_ context.Context, _ Subject, consumer, _ string, _ string) (map[string]string, string) {
			fixture.mu.Lock()
			fixture.consumer = consumer
			fixture.mu.Unlock()
			return map[string]string{"Authorization": "Bearer execution"}, "applied"
		},
		PrepareContentWrite: func(_ context.Context, subject Subject, profile, media string, _ int64) (ContentCommitter, ContentOutcome) {
			if subject != caller || profile != "image" || (media != "image/*" && media != "video/*") {
				return nil, ContentForbidden
			}
			return func(_ context.Context, _ string, data []byte) ContentOutcome {
				fixture.mu.Lock()
				fixture.committed = append(fixture.committed, bytes.Clone(data))
				fixture.mu.Unlock()
				return ContentResolved
			}, ContentResolved
		},
		Record: func(record Record) {
			fixture.mu.Lock()
			fixture.records = append(fixture.records, record)
			fixture.mu.Unlock()
			select {
			case fixture.recorded <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider = provider
	t.Cleanup(func() { _ = provider.Close() })
	execution := NewJobExecution(provider, func(_ context.Context, scope, credential string) (Subject, ContentOutcome) {
		if scope != "caller-scope" || credential != "replicate" {
			return Subject{}, ContentForbidden
		}
		return caller, ContentResolved
	}, nil)
	execution.backend, execution.pollInterval = backend, time.Millisecond
	fixture.execution = execution
	return fixture
}

type boundInferenceExecutor struct{ *JobExecution }

func (e boundInferenceExecutor) CheckSubjectAdmission(binding acceptanceprovider.Binding, kind string, spec []byte, required []string) (acceptance.AcceptanceOutcome, string) {
	return e.CheckAdmissionForSubject(binding.Scope, kind, spec, required, Subject{Account: binding.Subject.Account, Program: binding.Subject.Program})
}

func TestInferenceBoundAdmissionRefusesBeforeJournalAndDuplicateKeepsReceipt(t *testing.T) {
	fixture := newDurableFixture(t, &fakeDurableBackend{})
	root := t.TempDir()
	p, err := acceptanceprovider.OpenWithExecutor(root, "owner", boundInferenceExecutor{fixture.execution})
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "caller")
	subject := acceptanceprovider.AuthenticatedSubject{AccountKind: "posix", Account: caller.Account, Executable: executable, Program: caller.Program}
	scope, err := acceptanceprovider.LocalSubjectScope(subject.AccountKind, subject.Account, executable)
	if err != nil {
		t.Fatal(err)
	}
	binding := acceptanceprovider.Binding{Scope: scope, Subject: &subject, Origin: "local"}
	submission := durableSubmission(t, p, "bound-admission", jobwire.ProfileVideo)
	fixture.mu.Lock()
	fixture.permitted = false
	fixture.mu.Unlock()
	result, err := p.BindBinding(binding).Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeInvalid || result.Reason != "rights:denied" || result.Receipt != nil {
		t.Fatalf("permanent refusal %+v %v", result, err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "acceptance", "requests"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("refusal journaled: %v %v", entries, err)
	}
	fixture.mu.Lock()
	fixture.policyUnavailable = true
	fixture.mu.Unlock()
	result, err = p.BindBinding(binding).Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeUnavailable || result.Receipt != nil {
		t.Fatalf("policy outage %+v %v", result, err)
	}
	fixture.mu.Lock()
	fixture.policyUnavailable = false
	fixture.permitted = true
	fixture.mu.Unlock()
	result, err = p.BindBinding(binding).Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeAccepted || result.Receipt == nil {
		t.Fatalf("repaired grant %+v %v", result, err)
	}
	operationID := result.Receipt.OperationID
	fixture.mu.Lock()
	fixture.permitted = false
	fixture.mu.Unlock()
	result, err = p.BindBinding(binding).Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeAccepted || result.Receipt == nil || result.Receipt.OperationID != operationID {
		t.Fatalf("duplicate after revocation %+v %v", result, err)
	}
}

func durableSubmission(t *testing.T, provider *acceptanceprovider.Provider, key string, profile jobwire.Profile) acceptance.Submission {
	t.Helper()
	history, err := provider.Bind("caller-scope").GetHistoryWindow()
	if err != nil {
		t.Fatal(err)
	}
	duration := int64(0)
	if profile == jobwire.ProfileVideo {
		duration = 8000
	}
	document := &jobwire.Document{Request: &jobwire.Request{Profile: profile, DurationMs: duration, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "a lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
	}}}
	return acceptance.Submission{Identity: acceptance.RequestIdentity{Key: key, HistoryEpoch: history.HistoryEpoch}, Kind: InferenceJobKind,
		Spec: jobwire.Encode(document), RequiredGuarantees: []string{RecoverableUpstreamGuarantee}}
}

func waitDurable(t *testing.T, operations acceptance.OperationControl, identity acceptance.RequestIdentity, terminal bool) acceptance.OperationSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		observed, err := operations.ObserveWork(identity)
		if err == nil && observed.Snapshot != nil && (!terminal || observed.Snapshot.State == acceptance.WorkStateComplete || observed.Snapshot.State == acceptance.WorkStateFailed || observed.Snapshot.State == acceptance.WorkStateCancelled) {
			return *observed.Snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not reach requested state")
	return acceptance.OperationSnapshot{}
}

func TestInferenceJobSurvivesCallerExitAndDuplicateReturnsReceipt(t *testing.T) {
	video := []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: video, MediaType: "video/mp4"}}}}
	fixture := newDurableFixture(t, backend)
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "video-once", jobwire.ProfileVideo)
	first, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || first.Receipt == nil {
		t.Fatalf("submit %+v %v", first, err)
	}
	duplicate, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || duplicate.Receipt == nil || duplicate.Receipt.OperationID != first.Receipt.OperationID {
		t.Fatalf("duplicate %+v %v", duplicate, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	read, err := provider.BindOperations("caller-scope").ReadResult(submission.Identity, 0, 65536)
	if err != nil || read.Chunk == nil {
		t.Fatalf("result %+v %v", read, err)
	}
	document, err := jobwire.Decode(read.Chunk.Data)
	if err != nil || document.Result == nil || len(document.Result.Deliveries) != 1 || document.Result.Deliveries[0].MediaType != "video/mp4" {
		t.Fatalf("document %+v %v", document, err)
	}
	backend.mu.Lock()
	submits := backend.submits
	backend.mu.Unlock()
	if submits != 1 {
		t.Fatalf("upstream submits=%d", submits)
	}
	select {
	case <-fixture.recorded:
	case <-time.After(time.Second):
		t.Fatal("audit record was not attempted after terminal state")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.committed) != 1 || !bytes.Equal(fixture.committed[0], video) {
		t.Fatalf("committed %q", fixture.committed)
	}
	if len(fixture.records) != 1 || fixture.records[0].Outcome != "completed" || fixture.records[0].Operation == "" || fixture.records[0].Account != caller.Account {
		t.Fatalf("audit records %+v", fixture.records)
	}
	if fixture.consumer != ImageContract {
		t.Fatalf("credential consumer %q", fixture.consumer)
	}
}

func TestInferenceJobResumesRecordedHandleAfterServiceRestart(t *testing.T) {
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobRunning}, polled: make(chan struct{}, 1)}
	fixture := newDurableFixture(t, backend)
	ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"replicate": {RequestsPerDay: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.provider.cfg.Ceilings = ceilings
	root := t.TempDir()
	first, err := acceptanceprovider.OpenWithExecutor(root, "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, first, "restart-handle", jobwire.ProfileVideo)
	if accepted, submitErr := first.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, submitErr)
	}
	firstCtx, stopFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Execute(firstCtx) }()
	select {
	case <-backend.polled:
	case <-time.After(3 * time.Second):
		t.Fatal("first service did not poll recorded handle")
	}
	stopFirst()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first service did not stop")
	}

	backend.mu.Lock()
	backend.poll = durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), MediaType: "video/mp4"}}}
	backend.mu.Unlock()
	second, err := acceptanceprovider.OpenWithExecutor(root, "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	secondCtx, stopSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Execute(secondCtx) }()
	final := waitDurable(t, second.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	stopSecond()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second service did not stop")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("recorded handle was resubmitted %d times", backend.submits)
	}
	_, _, _, requests, _, _, _ := ceilings.SpendUnits("replicate")
	if requests != 1 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestInferenceJobRejectsOversizedOutputBeforeCommit(t *testing.T) {
	output := []byte("\x89PNG\r\n\x1a\n1234")
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: output, MediaType: "image/png"}}}}
	fixture := newDurableFixture(t, backend)
	fixture.provider.cfg.MaxGeneratedImageBytes = 8
	fixture.provider.cfg.MaxImageOutputBytes = 10
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "oversized-output", jobwire.ProfileImageBatch)
	if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, submitErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateFailed {
		t.Fatalf("final %+v", final)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.committed) != 0 {
		t.Fatalf("oversized output was committed")
	}
}

func TestInferenceJobRetriesResultAccountingBeforeTerminal(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ceilings.json")
	ceilings, err := OpenCeilings(state, map[string]Ceiling{"replicate": {RequestsPerDay: 10, ImagesPerDay: 10}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: []byte("\x89PNG\r\n\x1a\n1234"), MediaType: "image/png"}}}}
	backend.pollHook = func() {
		// cas.Change stages its replacement under a randomized name it
		// invents itself, so nothing here can collide with that; its lock
		// file is the one fixed, predictable path, and failing to open it
		// (because it is already a directory) fails the persist the same
		// way the write used to fail when blocked. An earlier successful
		// persist (at submit time) already left this file behind, closed
		// but not removed, so it is replaced rather than freshly created.
		if err := os.RemoveAll(state + ".lock"); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(state+".lock", 0o700); err != nil {
			t.Error(err)
		}
		close(blocked)
	}
	fixture := newDurableFixture(t, backend)
	fixture.provider.cfg.Ceilings = ceilings
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "accounting-retry", jobwire.ProfileImageBatch)
	if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, submitErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("result accounting was not reached")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		observed := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, false)
		if observed.Waiting == "ceiling:unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not wait for accounting: %+v", observed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.Remove(state + ".lock"); err != nil {
		t.Fatal(err)
	}
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	_, _, _, requests, images, _, _ := ceilings.SpendUnits("replicate")
	if requests != 1 || images != 1 {
		t.Fatalf("requests=%d images=%d", requests, images)
	}
}

func TestInferenceJobRetriesAttemptAccountingBeforeFirstSubmit(t *testing.T) {
	state := filepath.Join(t.TempDir(), "ceilings.json")
	ceilings, err := OpenCeilings(state, map[string]Ceiling{"replicate": {RequestsPerDay: 10, ImagesPerDay: 10}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// cas.Change stages its replacement under a randomized name it invents
	// itself, so nothing here can collide with that; its lock file is the
	// one fixed, predictable path, and failing to open it (because it is
	// already a directory) fails the persist the same way the write used
	// to fail when blocked.
	if err := os.Mkdir(state+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: []byte("\x89PNG\r\n\x1a\n1234"), MediaType: "image/png"}}}}
	fixture := newDurableFixture(t, backend)
	fixture.provider.cfg.Ceilings = ceilings
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "attempt-accounting-retry", jobwire.ProfileImageBatch)
	if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, submitErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		observed := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, false)
		if observed.Waiting == "ceiling:unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not wait for attempt accounting: %+v", observed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	backend.mu.Lock()
	if backend.submits != 0 {
		backend.mu.Unlock()
		t.Fatal("upstream submit happened before accounting")
	}
	backend.mu.Unlock()
	if err := os.Remove(state + ".lock"); err != nil {
		t.Fatal(err)
	}
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v", final)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("upstream submits=%d", backend.submits)
	}
}

func TestInferenceJobUncertainSubmissionIsNeverReplayed(t *testing.T) {
	backend := &fakeDurableBackend{uncertain: true}
	fixture := newDurableFixture(t, backend)
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "uncertain", jobwire.ProfileImageBatch)
	if accepted, err := provider.Bind("caller-scope").Submit(submission); err != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = provider.Execute(ctx) }()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		observed := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, false)
		if observed.Waiting == "upstream:uncertain" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 {
		t.Fatalf("uncertain request replayed %d times", backend.submits)
	}
}

func TestInferenceJobPersistedSubmittingMarkerNeverReplaysSubmit(t *testing.T) {
	backend := &fakeDurableBackend{}
	fixture := newDurableFixture(t, backend)
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
	}}}
	prepared, _, err := fixture.execution.PrepareScoped("caller-scope", "crash", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
	if err != nil {
		t.Fatal(err)
	}
	store := job.NewMemoryStore()
	id, err := store.Submit(job.Record{ID: "crash", Kind: InferenceJobKind, State: job.StatePending, Spec: prepared})
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Claim(id, "successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	held, err = store.Update(id, held.Lease.Epoch, func(record *job.Record) error {
		return record.SetCheckpoint(inferenceJobCheckpoint{Phase: "submitting", Host: "replicate", Binding: inferenceHostBinding(fixture.provider.cfg.Router.Hosts()[0]), Model: "owner/model", Adapter: "replicate"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.execution.execute(context.Background(), store, held); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 0 || backend.reconciles != 1 {
		t.Fatalf("submits=%d reconciles=%d", backend.submits, backend.reconciles)
	}
}

func TestInferenceJobCheckpointFailureAfterSubmitReconcilesWithoutReplay(t *testing.T) {
	backend := &fakeDurableBackend{}
	fixture := newDurableFixture(t, backend)
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
	}}}
	prepared, _, err := fixture.execution.PrepareScoped("caller-scope", "checkpoint-failure", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
	if err != nil {
		t.Fatal(err)
	}
	base := job.NewMemoryStore()
	store := &failingCheckpointStore{Store: base}
	id, err := store.Submit(job.Record{ID: "checkpoint-failure", Kind: InferenceJobKind, State: job.StatePending, Spec: prepared})
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Claim(id, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// The first Update writes the submitting marker. The next Update is the
	// required persistence of the returned upstream handle and is forced to
	// fail after the upstream has accepted the request.
	store.failAfter = 2
	if err := fixture.execution.execute(context.Background(), store, held); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("execute error %v", err)
	}
	record, err := base.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint inferenceJobCheckpoint
	if err := record.DecodeCheckpoint(&checkpoint); err != nil || checkpoint.Phase != "submitting" || checkpoint.Handle != "" {
		t.Fatalf("checkpoint %+v err=%v", checkpoint, err)
	}
	backend.mu.Lock()
	submits := backend.submits
	backend.mu.Unlock()
	if submits != 1 {
		t.Fatalf("upstream submits=%d", submits)
	}
	// A successor sees the persisted submitting marker and calls Reconcile;
	// it never calls Submit again, even though the handle was not persisted.
	if err := base.Release(id, held.Lease.Epoch); err != nil {
		t.Fatal(err)
	}
	next, err := base.Claim(id, "successor", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.execution.execute(context.Background(), base, next); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 || backend.reconciles != 1 {
		t.Fatalf("submits=%d reconciles=%d", backend.submits, backend.reconciles)
	}
}

func TestInferenceJobRenewalIOFailureCancelsInFlightUpstream(t *testing.T) {
	backend := &fakeDurableBackend{blockSubmit: true, submitStarted: make(chan struct{}), submitCanceled: make(chan struct{})}
	fixture := newDurableFixture(t, backend)
	fixture.execution.leaseTTL = 300 * time.Millisecond
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-schnell", Mode: jobwire.ModeGenerate, Prompt: "lighthouse", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeHostedAllowed}, Credential: "replicate", Extensions: map[string]string{},
	}}}
	prepared, _, err := fixture.execution.PrepareScoped("caller-scope", "lease-loss", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
	if err != nil {
		t.Fatal(err)
	}
	memory := job.NewMemoryStore()
	failRenewals := make(chan struct{})
	renewed := make(chan struct{}, 1)
	store := &failingRenewStore{Store: memory, fail: failRenewals, renewed: renewed, failErr: io.ErrUnexpectedEOF}
	id, err := store.Submit(job.Record{ID: "lease-loss", Kind: InferenceJobKind, State: job.StatePending, Spec: prepared})
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Claim(id, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fixture.execution.execute(context.Background(), store, held) }()
	select {
	case <-backend.submitStarted:
	case err := <-done:
		record, _ := store.Load(id)
		t.Fatalf("executor returned before upstream submission: %v record=%+v", err, record)
	case <-time.After(time.Second):
		t.Fatal("upstream submission did not start")
	}
	select {
	case <-renewed:
	default:
		t.Fatal("executor submitted upstream before its pre-submit lease renewal")
	}
	// Only now make renewal unavailable. Pre-submit renewal remains valid even
	// when the race detector lets the background keeper run first; the next
	// keeper tick must cancel the already-blocked upstream submission.
	close(failRenewals)
	select {
	case <-backend.submitCanceled:
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel upstream submission")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("executor did not return after lease loss")
	}
}

func TestInferenceJobCancellationStopsRecordedUpstream(t *testing.T) {
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobRunning}, polled: make(chan struct{}, 1)}
	fixture := newDurableFixture(t, backend)
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "cancel", jobwire.ProfileVideo)
	if accepted, err := provider.Bind("caller-scope").Submit(submission); err != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	select {
	case <-backend.polled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream was not polled")
	}
	if result, err := provider.Bind("caller-scope").CancelWork(submission.Identity); err != nil || result.Outcome != acceptance.CancellationOutcomeRequested {
		t.Fatalf("cancel %+v %v", result, err)
	}
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateCancelled {
		t.Fatalf("final %+v", final)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.cancels != 1 {
		t.Fatalf("upstream cancels=%d", backend.cancels)
	}
}

func TestInferenceJobRequiresRecoverableGuaranteeBeforeAcceptance(t *testing.T) {
	fixture := newDurableFixture(t, &fakeDurableBackend{})
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "missing-guarantee", jobwire.ProfileVideo)
	submission.RequiredGuarantees = nil
	result, err := provider.Bind("caller-scope").Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeInvalid || result.Receipt != nil {
		t.Fatalf("result %+v %v", result, err)
	}
}

func TestInferenceJobAdmissionUsesReceiverSubjectWithoutScopeInversion(t *testing.T) {
	fixture := newDurableFixture(t, &fakeDurableBackend{})
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "subject-admission", jobwire.ProfileVideo)
	fixture.execution.resolveSubject = func(context.Context, string, string) (Subject, ContentOutcome) {
		return Subject{}, ContentForbidden
	}
	if outcome, _ := fixture.execution.CheckAdmission("unmapped-scope", submission.Kind, submission.Spec, []string{RecoverableUpstreamGuarantee}); outcome != acceptance.AcceptanceOutcomeForbidden {
		t.Fatalf("legacy scope inversion outcome %s", outcome)
	}
	if outcome, reason := fixture.execution.CheckAdmissionForSubject("unmapped-scope", submission.Kind, submission.Spec, []string{RecoverableUpstreamGuarantee}, caller); outcome != acceptance.AcceptanceOutcomeAccepted {
		t.Fatalf("receiver subject admission %s %s", outcome, reason)
	}
	fixture.mu.Lock()
	fixture.permitted = false
	fixture.mu.Unlock()
	if outcome, reason := fixture.execution.CheckAdmissionForSubject("unmapped-scope", submission.Kind, submission.Spec, []string{RecoverableUpstreamGuarantee}, caller); outcome != acceptance.AcceptanceOutcomeInvalid || reason != "rights:denied" {
		t.Fatalf("permanent refusal was not reported before acceptance: %s %s", outcome, reason)
	}
}

func TestDurableRequestGuaranteesMapByExactWireWord(t *testing.T) {
	future := jobwire.RequestGuarantee("example.runtime/private-routing@1")
	got := requestGuaranteesFromJob([]jobwire.RequestGuarantee{
		jobwire.RequestGuaranteeLocalOnly,
		jobwire.RequestGuaranteeHostedAllowed,
		future,
	})
	want := []wire.RequestGuarantee{
		wire.RequestGuaranteeLocalOnly,
		wire.RequestGuaranteeHostedAllowed,
		wire.RequestGuarantee(future),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("mapped guarantees %v, want %v", got, want)
	}
	request := jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 8000, Image: jobwire.ImageRequest{
		Model: "model", Mode: jobwire.ModeGenerate, Prompt: "prompt", Size: "1024x1024", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly, jobwire.RequestGuaranteeHostedAllowed}, Extensions: map[string]string{},
	}}
	if err := validateInferenceJob(request); err == nil || !strings.Contains(err.Error(), "guarantees") {
		t.Fatalf("contradictory durable guarantees: %v", err)
	}
	request.Image.Guarantees = []jobwire.RequestGuarantee{future}
	roundTrip, err := jobwire.Decode(jobwire.Encode(&jobwire.Document{Request: &request}))
	if err != nil || roundTrip == nil || roundTrip.Request == nil || len(roundTrip.Request.Image.Guarantees) != 1 || roundTrip.Request.Image.Guarantees[0] != future {
		t.Fatalf("future durable guarantee codec round trip: %+v %v", roundTrip, err)
	}
	if err := validateInferenceJob(*roundTrip.Request); err == nil || !strings.Contains(err.Error(), "guarantees") {
		t.Fatalf("future durable guarantee: %v", err)
	}
}

func TestInferenceJobRevokedRightsFailWithoutUpstreamSpend(t *testing.T) {
	backend := &fakeDurableBackend{}
	fixture := newDurableFixture(t, backend)
	provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, provider, "revoked", jobwire.ProfileVideo)
	if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, submitErr)
	}
	fixture.mu.Lock()
	fixture.permitted = false
	fixture.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = provider.Execute(ctx) }()
	final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil {
		t.Fatalf("final %+v", final)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 0 {
		t.Fatalf("revoked job spent upstream %d times", backend.submits)
	}
}

func TestInferenceJobResultReadClampsOverflowingPage(t *testing.T) {
	execution := NewJobExecution(nil, nil, nil)
	result := jobwire.JobResult{Profile: jobwire.ProfileImageBatch, Deliveries: []jobwire.Delivery{}, Host: "host", Model: "model"}
	record := &job.Record{Kind: InferenceJobKind, State: job.StateComplete}
	if err := record.SetCheckpoint(inferenceJobCheckpoint{Phase: "complete", Result: &result}); err != nil {
		t.Fatal(err)
	}
	data, total, err := execution.ReadOperationResult("", record, 1, math.MaxInt64)
	if err != nil || int64(len(data)) != total-1 {
		t.Fatalf("len=%d total=%d err=%v", len(data), total, err)
	}
}

func TestReplicateDurableBackendUsesAsyncHandle(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/models/owner/model/predictions":
			_, _ = fmt.Fprint(w, `{"id":"prediction-1","status":"starting","output":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/predictions/prediction-1/cancel":
			_, _ = fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	backend := replicateJobBackend{client: server.Client(), maxEach: 1 << 20, maxTotal: 1 << 20}
	host := router.NewHosted("replicate", server.URL, router.WireReplicatePredictions, "replicate")
	request := jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 8000, Image: jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Prompt: "test", Size: "1024x1024", Count: 1, Extensions: map[string]string{}}}
	status, err := backend.Submit(context.Background(), host, "operation", "owner/model", request, map[string]string{"Authorization": "Bearer secret"})
	if err != nil || status.Handle != "prediction-1" || status.State != durableJobRunning {
		t.Fatalf("status %+v %v", status, err)
	}
	if err := backend.Cancel(context.Background(), host, status.Handle, map[string]string{"Authorization": "Bearer secret"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(paths) != "[POST /v1/models/owner/model/predictions POST /v1/predictions/prediction-1/cancel]" {
		t.Fatalf("paths %v", paths)
	}
}

func TestReplicateDurableOutputIsBoundedAndReceivesNoCredential(t *testing.T) {
	mp4 := "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"
	client := &http.Client{Transport: imageRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://replicate.delivery/result.mp4" || request.Header.Get("Authorization") != "" {
			t.Fatalf("output request %s headers %v", request.URL, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(mp4)), Header: make(http.Header)}, nil
	})}
	request := jobwire.Request{Profile: jobwire.ProfileVideo, Image: jobwire.ImageRequest{Count: 1}}
	backend := replicateJobBackend{client: client, maxEach: 1, maxTotal: int64(len(mp4))}
	status, err := backend.status(context.Background(), replicatePrediction{ID: "prediction", Status: "succeeded", Output: []string{"https://replicate.delivery/result.mp4"}}, request)
	if err != nil || status.State != durableJobSucceeded || len(status.Outputs) != 1 || status.Outputs[0].MediaType != "video/mp4" {
		t.Fatalf("status %+v error %v", status, err)
	}
	backend.maxTotal--
	if _, err := backend.status(context.Background(), replicatePrediction{ID: "prediction", Status: "succeeded", Output: []string{"https://replicate.delivery/result.mp4"}}, request); err == nil {
		t.Fatal("oversized durable output accepted")
	}
}

func TestReplicateDurableImageUsesPerOutputBound(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n1234"
	client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(png)), Header: make(http.Header)}, nil
	})}
	backend := replicateJobBackend{client: client, maxEach: int64(len(png) - 1), maxTotal: 1 << 20}
	request := jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{Count: 1}}
	if _, err := backend.status(context.Background(), replicatePrediction{ID: "prediction", Status: "succeeded", Output: []string{"https://replicate.delivery/result.png"}}, request); err == nil {
		t.Fatal("image larger than the per-output bound was accepted")
	}
}
