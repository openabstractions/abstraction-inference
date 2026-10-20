package inference

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

// heldImageEngine reports progress and holds each run until release closes,
// when it returns its image, or until its context ends, which it records.
type heldImageEngine struct {
	release   chan struct{}
	started   chan struct{}
	runs      atomic.Int64
	cancelled atomic.Int64
}

func newHeldImageEngine() *heldImageEngine {
	return &heldImageEngine{release: make(chan struct{}), started: make(chan struct{}, 8)}
}

func (e *heldImageEngine) Unsupported(wire.ImageRequest) string { return "" }

func (e *heldImageEngine) Run(ctx context.Context, _ string, _ wire.ImageRequest, _ map[string]ImageInput, progress func(int64, string)) ImageEngineReply {
	e.runs.Add(1)
	e.started <- struct{}{}
	progress(25, "sampling")
	select {
	case <-e.release:
		return ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: testPNG, MediaType: "image/png"}}}
	case <-ctx.Done():
		e.cancelled.Add(1)
		return ImageEngineReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "caller"}
	}
}

// leafClock is a provider clock a test moves forward.
type leafClock struct {
	start  time.Time
	offset atomic.Int64
}

func (c *leafClock) now() time.Time        { return c.start.Add(time.Duration(c.offset.Load())) }
func (c *leafClock) skip(by time.Duration) { c.offset.Add(int64(by)) }

// imageLeaf is a native image provider built from the provider kit, as
// pytorch-image is: one engine host, its outputs in store.
func imageLeaf(t *testing.T, engine ImageEngine, clock *leafClock, store *storage.ContentStore) *Provider {
	t.Helper()
	host, err := router.NewImageEngine("leaf-engine", []string{testImageModel})
	if err != nil {
		t.Fatal(err)
	}
	routes := router.New(host)
	routes.Survey()
	images := &imageStore{}
	prepare := images.prepare
	if store != nil {
		prepare = func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
			return func(_ context.Context, digest string, data []byte) ContentOutcome {
				ref, err := store.Place(digest, int64(len(data)))
				if err != nil {
					return ContentUnavailable
				}
				if err := os.WriteFile(store.Path(ref), data, 0o600); err != nil {
					return ContentUnavailable
				}
				if err := store.Commit(ref); err != nil {
					return ContentUnavailable
				}
				return ContentResolved
			}, ContentResolved
		}
	}
	p, err := New(Config{Router: routes, ImageEngine: engine, ImageEngineHost: host, Now: clock.now, SurveyAge: time.Hour,
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: prepare})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// designatedRuntimeCall is the admitting context a leaf's service host gives
// its designated runtime's call.
func designatedRuntimeCall() context.Context {
	return WithDesignatedRuntime(WithRoute(context.Background(), RouteNative, "program"))
}

func awaitStopped(t *testing.T, p *Provider, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if op := p.visibleProfile(caller, id, router.ProfileImage); op != nil && op.stopped() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s did not stop %q", id, want)
}

// An image operation a designated runtime starts for a durable job waits
// JobIdle for its next Observe, and stays readable that long after it ends;
// an application's operation, and one whose job mark an application forged,
// idle out after Idle. Past JobIdle the job's operation is cancelled too, so
// an abandoned job holds the engine for a bounded time.
func TestImageJobOperationWaitsJobIdleForItsRuntime(t *testing.T) {
	clock := &leafClock{start: time.Now()}
	engine := newHeldImageEngine()
	p := imageLeaf(t, engine, clock, nil)
	request := func() wire.ImageRequest {
		r := imageEngineRequest()
		r.Model = testImageModel
		r.Extensions = map[string]string{JobExtension: "job-operation-1"}
		return r
	}
	job := p.StartImage(designatedRuntimeCall(), caller, request())
	forged := p.StartImage(WithRoute(context.Background(), RouteNative, "program"), caller, request())
	if job.Outcome != wire.StartOutcomeAccepted || forged.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admissions %+v %+v", job, forged)
	}
	if job.IdleMs != DefaultJobIdle.Milliseconds() || job.RetentionMs != DefaultJobIdle.Milliseconds() || forged.IdleMs != 10000 || forged.RetentionMs != 60000 {
		t.Fatalf("job bounds %d/%d, application bounds %d/%d", job.IdleMs, job.RetentionMs, forged.IdleMs, forged.RetentionMs)
	}
	<-engine.started
	<-engine.started
	clock.skip(15 * time.Second)
	awaitStopped(t, p, forged.Operation, "idle")
	if got := p.visibleProfile(caller, job.Operation, router.ProfileImage).stopped(); got != "" {
		t.Fatalf("job operation stopped %q after 15 s unobserved", got)
	}
	clock.skip(DefaultJobIdle)
	awaitStopped(t, p, job.Operation, "idle")
}

// leafImageService serves a kit provider's image@1 to its designated
// runtime, as the leaf's service host does.
type leafImageService struct{ p *Provider }

func (s leafImageService) Start(request wire.ImageRequest) (wire.Admission, error) {
	return s.p.StartImage(designatedRuntimeCall(), caller, request), nil
}

func (s leafImageService) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return s.p.ObserveImage(context.Background(), caller, operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (s leafImageService) Cancel(operation string) (wire.Cancellation, error) {
	return s.p.CancelImage(caller, operation), nil
}

// A durable picture job whose runtime is gone for 15 s while the provider
// generates still completes when the runtime returns. On 2026-11-10 the
// provider's ten-second idle close cancelled the operation, and the job
// ended cancelled although nobody cancelled it.
func TestNativeImageJobOutlastsARuntimeGoneFifteenSeconds(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("image-leaf", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := &leafClock{start: time.Now()}
	engine := newHeldImageEngine()
	leaf := imageLeaf(t, engine, clock, store)
	allow := func(context.Context, *identity.Peer, string) error { return nil }
	content, err := storageService.NewComponent(store, allow)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := videoEndpoint(t)
	if err := listenFakeProvider(t, &wire.ImageDispatcher{Handler: leafImageService{leaf}}, content, endpoint); err != nil {
		t.Fatal(err)
	}
	v := newNativeJobRuntime(t, "image-provider", endpoint, testImageModel, router.ProfileImage, nil, nil)
	submission, accepted := submitImageBatch(t, v, "image-runtime-gone", 1)
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	<-engine.started
	// The runtime has read the provider's progress before it goes.
	deadline := time.Now().Add(dispatchFailsafe)
	for {
		observed, err := v.jobs.BindOperations("caller-scope").ObserveWork(submission.Identity)
		if err == nil && observed.Snapshot != nil && observed.Snapshot.Progress.Done == 25 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never read the provider's progress: %+v", observed.Snapshot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	v.stop()
	v.mu.Lock()
	done := v.done
	v.mu.Unlock()
	<-done
	clock.skip(15 * time.Second)
	// The leaf's watchdog runs every 100 ms; give it several turns.
	time.Sleep(500 * time.Millisecond)
	if n := engine.cancelled.Load(); n != 0 {
		t.Fatalf("provider cancelled %d runs while the runtime was gone", n)
	}
	close(engine.release)
	v.restart(t)
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	v.mu.Lock()
	committed := v.committed
	v.mu.Unlock()
	if len(committed) != 1 || !bytes.Equal(committed[0], testPNG) || engine.runs.Load() != 1 {
		t.Fatalf("caller store received %d outputs from %d runs", len(committed), engine.runs.Load())
	}
}

// A runtime that lost a Start reply sends the same Start again. The
// provider answers with the operation it already runs for that caller and
// job; another job value or another caller starts new work.
func TestImageJobStartRepeatedReturnsTheRunningOperation(t *testing.T) {
	clock := &leafClock{start: time.Now()}
	engine := newHeldImageEngine()
	p := imageLeaf(t, engine, clock, nil)
	request := func(job string) wire.ImageRequest {
		r := imageEngineRequest()
		r.Model = testImageModel
		r.Extensions = map[string]string{JobExtension: job}
		return r
	}
	first := p.StartImage(designatedRuntimeCall(), caller, request("job-1"))
	<-engine.started
	again := p.StartImage(designatedRuntimeCall(), caller, request("job-1"))
	if first.Outcome != wire.StartOutcomeAccepted || again.Operation != first.Operation || again.IdleMs != first.IdleMs {
		t.Fatalf("admissions %+v %+v", first, again)
	}
	other := caller
	other.Program += "-other"
	for _, a := range []wire.Admission{
		p.StartImage(designatedRuntimeCall(), caller, request("job-2")),
		p.StartImage(designatedRuntimeCall(), other, request("job-1")),
	} {
		if a.Outcome != wire.StartOutcomeAccepted || a.Operation == first.Operation {
			t.Fatalf("separate start %+v", a)
		}
		<-engine.started
	}
	if n := engine.runs.Load(); n != 3 {
		t.Fatalf("engine ran %d times", n)
	}
}
