package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	casapi "github.com/openabstractions/abstraction-cas/go/api"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

const testVideoModel = "wan2.2-ti2v-5b"

// fakeVideoProvider is a native video provider: it serves video@1 and its
// content store on one authenticated endpoint, and returns a small WebM. Its
// operations live in memory, as an engine-backed provider's do; forget is a
// provider restart.
type fakeVideoProvider struct {
	mu          sync.Mutex
	store       *storage.ContentStore
	result      []byte
	media       string
	script      []wire.VideoObservation
	forgetAfter int
	refuse      *wire.VideoAdmission
	starts      []wire.VideoRequest
	cancels     []string
	polls       map[string]int
	cancelled   map[string]bool
	inputSeen   bool
	// manual serves script[stage] on every Observe until the test moves
	// stage past the script, instead of one entry per Observe.
	manual bool
	stage  int
	// loseStart is how many accepted Starts lose their reply: the provider
	// admits the work and the runtime sees a failed call.
	loseStart int
	// byRequest maps each held request_id to its operation; created counts
	// operations actually admitted.
	byRequest  map[string]string
	created    int
	startTimes []time.Time
	// restartOnLost forgets every operation right after a lost reply, as a
	// provider that restarted before answering; with emptyStoreOnRestart its
	// content store comes back empty too.
	restartOnLost       bool
	emptyStoreOnRestart bool
	emptyStore          *storage.ContentStore
	// onLostStart runs, in its own goroutine, after a reply is lost.
	onLostStart func()
	// failWrite makes the next content-writer authorization refuse once,
	// armed by a lost reply when failWriteAfterLost is set; onWrite runs on
	// every content-writer authorization.
	failWriteAfterLost bool
	failWrite          bool
	onWrite            func()
}

func (f *fakeVideoProvider) Start(request wire.VideoRequest) (wire.VideoAdmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, request)
	f.startTimes = append(f.startTimes, time.Now())
	if f.refuse != nil {
		return *f.refuse, nil
	}
	// A held request_id names its operation; the frame is not read again.
	operation, held := f.byRequest[request.RequestID]
	if !held {
		if request.ImageDigest != "" {
			if _, f.inputSeen = f.store.Find(request.ImageDigest); !f.inputSeen {
				return wire.VideoAdmission{Outcome: wire.StartOutcomeInvalid, Reason: "content:unknown"}, nil
			}
		}
		f.created++
		operation = fmt.Sprintf("video-%d", f.created)
		f.polls[operation] = 0
		if request.RequestID != "" {
			f.byRequest[request.RequestID] = operation
		}
	}
	if f.loseStart > 0 {
		f.loseStart--
		if f.restartOnLost {
			f.polls, f.byRequest = map[string]int{}, map[string]string{}
			if f.emptyStore != nil {
				f.store = f.emptyStore
			}
		}
		f.failWrite = f.failWriteAfterLost
		if f.onLostStart != nil {
			go f.onLostStart()
		}
		return wire.VideoAdmission{}, errors.New("reply lost")
	}
	return wire.VideoAdmission{Outcome: wire.StartOutcomeAccepted, Operation: operation, RetentionMs: 60000}, nil
}

// authorizeWrite is the provider content writer's policy.
func (f *fakeVideoProvider) authorizeWrite(context.Context, *identity.Peer, string) error {
	f.mu.Lock()
	refuse, hook := f.failWrite, f.onWrite
	f.failWrite = false
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if refuse {
		return errors.New("writer refused once")
	}
	return nil
}

func (f *fakeVideoProvider) Observe(operation string) (wire.VideoObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, known := f.polls[operation]
	if !known {
		return wire.VideoObservation{Status: wire.VideoStateUnknown}, nil
	}
	f.polls[operation] = n + 1
	if f.forgetAfter > 0 && n+1 >= f.forgetAfter {
		f.polls, f.byRequest = map[string]int{}, map[string]string{}
	}
	if f.cancelled[operation] {
		return wire.VideoObservation{Status: wire.VideoStateCancelled}, nil
	}
	if f.manual {
		n = f.stage
	}
	if n < len(f.script) {
		return f.script[n], nil
	}
	sum := sha256.Sum256(f.result)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if _, stored := f.store.Find(digest); !stored {
		ref, err := f.store.Place(digest, int64(len(f.result)))
		if err != nil {
			return wire.VideoObservation{Status: wire.VideoStateUnavailable}, nil
		}
		if err := os.WriteFile(f.store.Path(ref), f.result, 0o600); err != nil {
			return wire.VideoObservation{Status: wire.VideoStateUnavailable}, nil
		}
		if err := f.store.Commit(ref); err != nil {
			return wire.VideoObservation{Status: wire.VideoStateUnavailable}, nil
		}
	}
	return wire.VideoObservation{Status: wire.VideoStateCompleted, Delivery: &wire.Delivery{Digest: digest, MediaType: f.media, Size: int64(len(f.result)), Location: "local"}}, nil
}

func (f *fakeVideoProvider) Cancel(operation string) (wire.Cancellation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, operation)
	if _, known := f.polls[operation]; !known {
		return wire.Cancellation{Outcome: wire.CancelOutcomeUnknown}, nil
	}
	f.cancelled[operation] = true
	cancelled := wire.ReplyOutcomeCancelled
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled, ReplyOutcome: &cancelled}, nil
}

func (f *fakeVideoProvider) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts), len(f.cancels)
}

func videoEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\inference-video-test-` + fixtureSuffix()
	}
	dir, err := os.MkdirTemp("/tmp", "inference-video-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// serveFakeVideoProvider listens on a native endpoint with Program proof and
// serves video@1 beside the provider's content reader and writer.
func serveFakeVideoProvider(t *testing.T, f *fakeVideoProvider) string {
	t.Helper()
	endpoint := videoEndpoint(t)
	if err := fakeVideoProviderStarter(t, f, endpoint)(); err != nil {
		t.Fatal(err)
	}
	return endpoint
}

// fakeVideoProviderStarter prepares f's stores and returns the function that
// starts listening on endpoint, which a test may call later and from any
// goroutine, as a runtime launches an on-demand provider.
func fakeVideoProviderStarter(t *testing.T, f *fakeVideoProvider, endpoint string) func() error {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("video-provider", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.polls, f.cancelled, f.byRequest = store, map[string]int{}, map[string]bool{}, map[string]string{}
	if f.emptyStoreOnRestart {
		if f.emptyStore, err = storage.NewContentStore("video-provider-restarted", t.TempDir()); err != nil {
			t.Fatal(err)
		}
	}
	if f.result == nil {
		f.result, f.media = testWebM, "video/webm"
	}
	allow := func(context.Context, *identity.Peer, string) error { return nil }
	content, err := storageService.NewComponent(store, allow)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.EnableWriter(f.authorizeWrite, 20<<20, casapi.BoundedFileStore{MaxBytes: storageService.MaxRecordFileBytes}, filepath.Join(t.TempDir(), "uploads.json")); err != nil {
		t.Fatal(err)
	}
	return func() error { return listenFakeVideoProvider(t, f, content, endpoint) }
}

func listenFakeVideoProvider(t *testing.T, f *fakeVideoProvider, content *storageService.Component, endpoint string) error {
	l, err := listen.ListenFramed(endpoint, listen.Program)
	if err != nil {
		return err
	}
	sessions := listen.Sessions(l, listen.SessionOptions{MaxSessions: 16})
	ctx, cancel := context.WithCancel(context.Background())
	if err := content.Start(ctx); err != nil {
		cancel()
		_ = sessions.Close()
		return err
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := sessions.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				callCtx, done := context.WithTimeout(ctx, 30*time.Second)
				defer done()
				call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, 1<<20)
				if err != nil {
					return
				}
				defer call.Close()
				services := []wire.ServedService{&wire.VideoDispatcher{Handler: f}}
				for _, service := range content.Services(call, callCtx) {
					services = append(services, service)
				}
				if reply, err := wire.ServeEndpoint(call.Frame, "openabstractions", "", services...); err == nil {
					_ = call.Reply(reply)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = sessions.Close()
		workers.Wait()
		_ = content.Close()
	})
	return nil
}

type videoRuntime struct {
	execution *JobExecution
	jobs      *acceptanceprovider.Provider
	root      string
	mu        sync.Mutex
	committed [][]byte
	frame     []byte
	cancel    context.CancelFunc
	done      chan struct{}
}

// deleteFrame removes the caller's initial frame from the caller's store.
func (v *videoRuntime) deleteFrame() {
	v.mu.Lock()
	v.frame = nil
	v.mu.Unlock()
}

// newVideoRuntime is the runtime side: one native host declaring the video
// profile, the executor NewJobExecution builds, and a caller store.
func newVideoRuntime(t *testing.T, endpoint string, input []byte) *videoRuntime {
	t.Helper()
	return newVideoRuntimeWith(t, endpoint, input, nil)
}

// newVideoRuntimeWith is newVideoRuntime with adjust applied to the
// provider's configuration before it is built.
func newVideoRuntimeWith(t *testing.T, endpoint string, input []byte, adjust func(*Config)) *videoRuntime {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("video-provider", endpoint, listen.ServerExpectation{Principal: principal, Program: identity.CanonicalProgramPath(filepath.Clean(exe))},
		[]string{testVideoModel}, []string{router.ProfileVideo})
	if err != nil {
		t.Fatal(err)
	}
	v := &videoRuntime{frame: input, root: t.TempDir()}
	cfg := Config{Router: router.New(native),
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveContent: func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
			v.mu.Lock()
			frame := v.frame
			v.mu.Unlock()
			sum := sha256.Sum256(frame)
			if frame == nil || digest != "sha256:"+hex.EncodeToString(sum[:]) {
				return nil, ContentUnknown
			}
			return bytes.Clone(frame), ContentResolved
		},
		PrepareContentWrite: func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
			return func(_ context.Context, _ string, data []byte) ContentOutcome {
				v.mu.Lock()
				v.committed = append(v.committed, bytes.Clone(data))
				v.mu.Unlock()
				return ContentResolved
			}, ContentResolved
		},
	}
	if adjust != nil {
		adjust(&cfg)
	}
	provider, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	v.execution = NewJobExecution(provider, func(_ context.Context, scope, _ string) (Subject, ContentOutcome) {
		if scope != "caller-scope" {
			return Subject{}, ContentForbidden
		}
		return caller, ContentResolved
	}, nil)
	v.execution.pollInterval = 5 * time.Millisecond
	v.jobs, err = acceptanceprovider.OpenWithExecutor(v.root, "owner", v.execution)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (v *videoRuntime) submit(t *testing.T, key string, image jobwire.ImageRequest) (acceptance.Submission, acceptance.AcceptanceResult) {
	t.Helper()
	history, err := v.jobs.Bind("caller-scope").GetHistoryWindow()
	if err != nil {
		t.Fatal(err)
	}
	image.Model, image.Prompt, image.Size, image.Count = testVideoModel, "a lighthouse at dusk", "832x480", 1
	image.Guarantees = []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 2000, Image: image}}
	submission := acceptance.Submission{Identity: acceptance.RequestIdentity{Key: key, HistoryEpoch: history.HistoryEpoch}, Kind: InferenceJobKind,
		Spec: jobwire.Encode(document), RequiredGuarantees: []string{RecoverableUpstreamGuarantee}}
	result, err := v.jobs.Bind("caller-scope").Submit(submission)
	if err != nil {
		t.Fatal(err)
	}
	return submission, result
}

func (v *videoRuntime) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	v.mu.Lock()
	v.cancel, v.done = cancel, done
	v.mu.Unlock()
	go func() { _ = v.jobs.Execute(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// stop cancels the running service without waiting for it to return.
func (v *videoRuntime) stop() {
	v.mu.Lock()
	cancel := v.cancel
	v.mu.Unlock()
	cancel()
}

// restart waits for the stopped service and starts it again on the same
// job store, as a runtime restart does.
func (v *videoRuntime) restart(t *testing.T) {
	t.Helper()
	v.mu.Lock()
	done := v.done
	v.mu.Unlock()
	<-done
	jobs, err := acceptanceprovider.OpenWithExecutor(v.root, "owner", v.execution)
	if err != nil {
		t.Fatal(err)
	}
	v.jobs = jobs
	v.run(t)
}

// waitVideo waits for the job's terminal snapshot, up to dispatchFailsafe;
// a resend after a lost reply waits for its backoff.
func (v *videoRuntime) waitVideo(t *testing.T, id acceptance.RequestIdentity) acceptance.OperationSnapshot {
	t.Helper()
	deadline := time.Now().Add(dispatchFailsafe)
	for time.Now().Before(deadline) {
		observed, err := v.jobs.BindOperations("caller-scope").ObserveWork(id)
		if err == nil && observed.Snapshot != nil && (observed.Snapshot.State == acceptance.WorkStateComplete || observed.Snapshot.State == acceptance.WorkStateFailed || observed.Snapshot.State == acceptance.WorkStateCancelled) {
			return *observed.Snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	observed, _ := v.jobs.BindOperations("caller-scope").ObserveWork(id)
	t.Fatalf("operation did not end: %+v", observed.Snapshot)
	return acceptance.OperationSnapshot{}
}

// TestNativeVideoJobCompletesThroughProviderVideoService: a video job goes to
// the native provider that declares video@1, never to an image host; the
// provider receives the clip request with no preset field and the caller's
// claimed program, and its WebM is copied into the caller's store.
func TestNativeVideoJobCompletesThroughProviderVideoService(t *testing.T) {
	fake := &fakeVideoProvider{script: []wire.VideoObservation{
		{Status: wire.VideoStateQueued, QueuePosition: 2},
		{Status: wire.VideoStateGenerating, Step: 1, Steps: 3},
	}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, accepted := v.submit(t, "video-complete", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	final := waitDurable(t, v.jobs.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	read, err := v.jobs.BindOperations("caller-scope").ReadResult(submission.Identity, 0, 65536)
	if err != nil || read.Chunk == nil {
		t.Fatalf("result %+v %v", read, err)
	}
	document, err := jobwire.Decode(read.Chunk.Data)
	if err != nil || document.Result == nil || len(document.Result.Deliveries) != 1 || document.Result.Deliveries[0].MediaType != "video/webm" || document.Result.Host != "video-provider" {
		t.Fatalf("document %+v %v", document, err)
	}
	v.mu.Lock()
	committed := v.committed
	v.mu.Unlock()
	if len(committed) != 1 || !bytes.Equal(committed[0], testWebM) {
		t.Fatalf("committed %q", committed)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != 1 {
		t.Fatalf("starts %d", len(fake.starts))
	}
	got := fake.starts[0]
	if got.Model != testVideoModel || got.Prompt != "a lighthouse at dusk" || got.Size != "832x480" || got.DurationMs != 2000 || got.Fps != 0 || got.Seed != 0 || got.ImageDigest != "" ||
		got.Extensions[ClaimExtension] != caller.Program {
		t.Fatalf("forwarded %+v", got)
	}
}

// TestNativeVideoJobLostProviderOperationFailsVisibly: a provider restart
// forgets the operation; the job fails as provider:lost and the work is never
// submitted again.
func TestNativeVideoJobLostProviderOperationFailsVisibly(t *testing.T) {
	fake := &fakeVideoProvider{forgetAfter: 1, script: []wire.VideoObservation{{Status: wire.VideoStateQueued}, {Status: wire.VideoStateQueued}}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, _ := v.submit(t, "video-lost", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	final := waitDurable(t, v.jobs.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "provider:lost" {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	time.Sleep(50 * time.Millisecond)
	if starts, _ := fake.counts(); starts != 1 {
		t.Fatalf("lost work submitted %d times", starts)
	}
}

// TestNativeVideoJobLostStartReplyIsReconciledByRequestID: the provider
// admitted the work and its reply was lost. The executor sends Start again
// with the same request_id, the provider answers with the operation it holds,
// and the job completes from one operation.
func TestNativeVideoJobLostStartReplyIsReconciledByRequestID(t *testing.T) {
	fake := &fakeVideoProvider{loseStart: 1}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, accepted := v.submit(t, "video-lost-reply", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != 2 || fake.created != 1 {
		t.Fatalf("starts %d, operations admitted %d", len(fake.starts), fake.created)
	}
	// The resend waits out the first backoff step.
	if gap := fake.startTimes[1].Sub(fake.startTimes[0]); gap < 900*time.Millisecond {
		t.Fatalf("Start resent after %s, before the 1 s backoff", gap)
	}
	for _, start := range fake.starts {
		if start.RequestID != accepted.Receipt.OperationID {
			t.Fatalf("request_id %q, want the job operation %q", start.RequestID, accepted.Receipt.OperationID)
		}
	}
}

// TestNativeVideoJobRefusedStartFailsWithoutRetry: a provider that refuses
// Start ran nothing; the job fails with its outcome and reason at once.
func TestNativeVideoJobRefusedStartFailsWithoutRetry(t *testing.T) {
	fake := &fakeVideoProvider{refuse: &wire.VideoAdmission{Outcome: wire.StartOutcomeUnsupportedFeature, Reason: "size:832x480"}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, _ := v.submit(t, "video-refused", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	final := waitDurable(t, v.jobs.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "native:unsupported_feature:size:832x480" {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	if starts, _ := fake.counts(); starts != 1 {
		t.Fatalf("starts %d", starts)
	}
}

// TestNativeVideoJobStagesTheInitialFrame: edit mode names the clip's first
// frame; the runtime copies the caller's authorized image into the provider's
// store before Start, and a mask is refused before acceptance.
func TestNativeVideoJobStagesTheInitialFrame(t *testing.T) {
	frame := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 128)...)
	sum := sha256.Sum256(frame)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	fake := &fakeVideoProvider{}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), frame)
	masked, refused := v.submit(t, "video-mask", jobwire.ImageRequest{Mode: jobwire.ModeEdit, ImageDigest: digest, ImageMediaType: "image/png",
		MaskDigest: digest, MaskMediaType: "image/png", Extensions: map[string]string{}})
	if refused.Receipt != nil || refused.Outcome != acceptance.AcceptanceOutcomeInvalid {
		t.Fatalf("masked video %+v (%s)", refused, masked.Identity.Key)
	}
	submission, accepted := v.submit(t, "video-frame", jobwire.ImageRequest{Mode: jobwire.ModeEdit, ImageDigest: digest, ImageMediaType: "image/png", Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	final := waitDurable(t, v.jobs.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != 1 || fake.starts[0].ImageDigest != digest || fake.starts[0].ImageMediaType != "image/png" || !fake.inputSeen {
		t.Fatalf("initial frame not staged: %+v seen=%v", fake.starts, fake.inputSeen)
	}
}

// TestNativeVideoJobCancelReachesProvider: cancelling the job cancels the
// provider's operation once.
func TestNativeVideoJobCancelReachesProvider(t *testing.T) {
	script := make([]wire.VideoObservation, 10000)
	for i := range script {
		script[i] = wire.VideoObservation{Status: wire.VideoStateGenerating, Step: 1, Steps: 3}
	}
	fake := &fakeVideoProvider{script: script}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, _ := v.submit(t, "video-cancel", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		polled := fake.polls["video-1"]
		fake.mu.Unlock()
		if polled > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider operation was never observed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result, err := v.jobs.Bind("caller-scope").CancelWork(submission.Identity); err != nil || result.Outcome != acceptance.CancellationOutcomeRequested {
		t.Fatalf("cancel %+v %v", result, err)
	}
	final := waitDurable(t, v.jobs.BindOperations("caller-scope"), submission.Identity, true)
	if final.State != acceptance.WorkStateCancelled {
		t.Fatalf("final %+v", final)
	}
	if _, cancels := fake.counts(); cancels != 1 {
		t.Fatalf("provider cancels %d", cancels)
	}
}

// TestNativeVideoJobSnapshotCarriesProgressAndQueuePosition: the job snapshot
// a caller observes carries the provider's step of steps as progress and its
// queue position as the waiting word queue:<n>, and drops the word once the
// engine starts generating.
func TestNativeVideoJobSnapshotCarriesProgressAndQueuePosition(t *testing.T) {
	fake := &fakeVideoProvider{manual: true, script: []wire.VideoObservation{
		{Status: wire.VideoStateQueued, QueuePosition: 2},
		{Status: wire.VideoStateGenerating, Step: 1, Steps: 3},
		{Status: wire.VideoStateGenerating, Step: 3, Steps: 3},
	}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, _ := v.submit(t, "video-progress", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	operations := v.jobs.BindOperations("caller-scope")
	for stage, want := range []struct {
		done, total int64
		waiting     string
	}{{0, 0, "queue:2"}, {1, 3, ""}, {3, 3, ""}} {
		fake.mu.Lock()
		fake.stage = stage
		fake.mu.Unlock()
		deadline := time.Now().Add(5 * time.Second)
		for {
			observed, err := operations.ObserveWork(submission.Identity)
			if err == nil && observed.Snapshot != nil && observed.Snapshot.State == acceptance.WorkStateRunning &&
				observed.Snapshot.Progress.Done == want.done && observed.Snapshot.Progress.Total == want.total && observed.Snapshot.Waiting == want.waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("stage %d: snapshot %+v (%v), want progress %d/%d waiting %q", stage, observed.Snapshot, err, want.done, want.total, want.waiting)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	fake.mu.Lock()
	fake.stage = len(fake.script)
	fake.mu.Unlock()
	final := waitDurable(t, operations, submission.Identity, true)
	if final.State != acceptance.WorkStateComplete || final.Progress.Done != 1 || final.Progress.Total != 1 || final.Waiting != "" {
		t.Fatalf("final %+v", final)
	}
}
