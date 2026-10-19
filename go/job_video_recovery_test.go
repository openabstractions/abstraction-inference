package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func testFrame() ([]byte, string) {
	frame := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), make([]byte, 128)...)
	sum := sha256.Sum256(frame)
	return frame, "sha256:" + hex.EncodeToString(sum[:])
}

func (v *videoRuntime) submitFrame(t *testing.T, key, digest string) (acceptance.Submission, acceptance.AcceptanceResult) {
	t.Helper()
	submission, accepted := v.submit(t, key, jobwire.ImageRequest{Mode: jobwire.ModeEdit, ImageDigest: digest, ImageMediaType: "image/png", Extensions: map[string]string{}})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	return submission, accepted
}

func assertOneOperation(t *testing.T, fake *fakeVideoProvider, starts, created int, requestID string) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != starts || fake.created != created {
		t.Fatalf("starts %d (want %d), operations admitted %d (want %d)", len(fake.starts), starts, fake.created, created)
	}
	for _, start := range fake.starts {
		if start.RequestID != requestID {
			t.Fatalf("request_id %q, want %q", start.RequestID, requestID)
		}
	}
}

// TestNativeVideoJobFrameUploadFailureAfterLostReplyIsReconciled: after a
// lost Start reply the resend's frame upload is refused once by the
// provider's writer. That is a transport outcome after an RPC, so the job
// waits uncertain and the next resend completes it from the one operation.
func TestNativeVideoJobFrameUploadFailureAfterLostReplyIsReconciled(t *testing.T) {
	frame, digest := testFrame()
	fake := &fakeVideoProvider{loseStart: 1, failWriteAfterLost: true}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), frame)
	submission, accepted := v.submitFrame(t, "video-frame-write", digest)
	v.run(t)
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 1, accepted.Receipt.OperationID)
}

// TestNativeVideoJobCancelledContextDuringFrameUploadKeepsTheJob: the service
// stops while a resend uploads the frame. The job stays uncertain, and the
// restarted service completes it from the one operation.
func TestNativeVideoJobCancelledContextDuringFrameUploadKeepsTheJob(t *testing.T) {
	frame, digest := testFrame()
	fake := &fakeVideoProvider{loseStart: 1}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), frame)
	// Each reading of the executor's clock is an hour after the last: every
	// resend is due at the next sweep, and no backoff of the real clock can
	// outlast the wait for the job's end (INF-G7's pacing has its own test).
	start := time.Now()
	var hours atomic.Int64
	v.execution.clock = func() time.Time { return start.Add(time.Duration(hours.Add(1)) * time.Hour) }
	armed := make(chan struct{})
	fake.onLostStart = func() { close(armed) }
	fake.onWrite = func() {
		select {
		case <-armed:
		default:
			return
		}
		fake.mu.Lock()
		fake.onWrite = nil
		fake.mu.Unlock()
		// The service stops while the provider still authorizes the resend's
		// upload: the runtime's context ends before this call answers.
		v.stop()
	}
	submission, accepted := v.submitFrame(t, "video-frame-cancel", digest)
	v.run(t)
	v.restart(t)
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 1, accepted.Receipt.OperationID)
}

// TestNativeVideoJobDeletedFrameAfterLostReplyReconcilesWithoutFrame: the
// caller deletes the initial frame after the provider admitted the work and
// the reply was lost. The job neither fails nor restages: the resend names the
// frame, and the provider that holds the request_id ignores it.
func TestNativeVideoJobDeletedFrameAfterLostReplyReconcilesWithoutFrame(t *testing.T) {
	frame, digest := testFrame()
	fake := &fakeVideoProvider{loseStart: 1}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), frame)
	fake.onLostStart = v.deleteFrame
	submission, accepted := v.submitFrame(t, "video-frame-deleted", digest)
	v.run(t)
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 1, accepted.Receipt.OperationID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.starts[1].ImageDigest != digest {
		t.Fatalf("resend named frame %q", fake.starts[1].ImageDigest)
	}
}

// TestNativeVideoJobRestartedProviderBeforeAnswerAdmitsOnce: the provider
// admitted the work and restarted before its reply arrived. It holds nothing,
// admits the resend as new work, and the job completes from that operation.
func TestNativeVideoJobRestartedProviderBeforeAnswerAdmitsOnce(t *testing.T) {
	fake := &fakeVideoProvider{loseStart: 1, restartOnLost: true}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, accepted := v.submit(t, "video-restarted", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateComplete {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 2, accepted.Receipt.OperationID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.polls["video-2"] == 0 {
		t.Fatalf("completion did not come from the second operation: polls %v", fake.polls)
	}
}

// TestNativeVideoJobCancelWhileUncertainLearnsAndCancelsTheOperation: the
// caller cancels while the Start reply is lost and the provider restarted.
// The executor sends Start again to learn the operation and cancels it.
func TestNativeVideoJobCancelWhileUncertainLearnsAndCancelsTheOperation(t *testing.T) {
	fake := &fakeVideoProvider{loseStart: 1, restartOnLost: true, manual: true, script: []wire.VideoObservation{{Status: wire.VideoStateQueued}}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, accepted := v.submit(t, "video-cancel-uncertain", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	cancelled := make(chan error, 1)
	fake.onLostStart = func() {
		result, err := v.jobs.Bind("caller-scope").CancelWork(submission.Identity)
		if err == nil && result.Outcome != acceptance.CancellationOutcomeRequested {
			err = errors.New("cancel " + result.Outcome.String())
		}
		cancelled <- err
	}
	v.run(t)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateCancelled {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 2, accepted.Receipt.OperationID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.cancelled["video-2"] || len(fake.cancels) != 1 {
		t.Fatalf("provider cancels %v, cancelled %v", fake.cancels, fake.cancelled)
	}
}

// TestNativeVideoJobForbiddenObservationFails: a provider that refuses the
// runtime's Observe as forbidden ends the job as native:forbidden.
func TestNativeVideoJobForbiddenObservationFails(t *testing.T) {
	fake := &fakeVideoProvider{manual: true, script: []wire.VideoObservation{{Status: wire.VideoStateForbidden}}}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
	submission, _ := v.submit(t, "video-forbidden", jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
	v.run(t)
	if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "native:forbidden" {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
}

// TestNativeVideoJobRefusesOversizeAndImageDeliveries: a delivery above the
// output bound and a PNG from a video provider both fail the job before any
// byte reaches the caller's store.
func TestNativeVideoJobRefusesOversizeAndImageDeliveries(t *testing.T) {
	for _, c := range []struct {
		name   string
		result []byte
		media  string
		bound  int64
	}{
		{"oversize", testWebM, "video/webm", int64(len(testWebM)) - 1},
		{"png", testPNG, "image/png", 64 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeVideoProvider{result: c.result, media: c.media}
			v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), nil)
			v.execution.backends[0] = nativeVideoJobBackend{maxInput: 20 << 20, maxOutput: c.bound}
			submission, _ := v.submit(t, "video-"+c.name, jobwire.ImageRequest{Mode: jobwire.ModeGenerate, Extensions: map[string]string{}})
			v.run(t)
			if final := v.waitVideo(t, submission.Identity); final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "native:delivery" {
				t.Fatalf("final %+v failure %+v", final, final.Failure)
			}
			v.mu.Lock()
			defer v.mu.Unlock()
			if len(v.committed) != 0 {
				t.Fatalf("committed %d deliveries", len(v.committed))
			}
		})
	}
}

// TestInferenceJobRecordsVideoUnderTheVideoProfile: the audit record of a
// video job names the video profile; an image batch stays image.
func TestInferenceJobRecordsVideoUnderTheVideoProfile(t *testing.T) {
	for _, c := range []struct {
		profile jobwire.Profile
		output  durableJobOutput
		want    string
	}{
		{jobwire.ProfileVideo, durableJobOutput{Data: testMP4, MediaType: "video/mp4"}, router.ProfileVideo},
		{jobwire.ProfileImageBatch, durableJobOutput{Data: testPNG, MediaType: "image/png"}, "image"},
	} {
		fixture := newDurableFixture(t, &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{c.output}}})
		p, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
		if err != nil {
			t.Fatal(err)
		}
		submission := durableSubmission(t, p, "record-"+c.profile.String(), c.profile)
		if accepted, err := p.Bind("caller-scope").Submit(submission); err != nil || accepted.Receipt == nil {
			t.Fatalf("submit %+v %v", accepted, err)
		}
		cancel := executeUntilCleanup(t, p)
		waitDurable(t, p.BindOperations("caller-scope"), submission.Identity, true)
		select {
		case <-fixture.recorded:
		case <-time.After(3 * time.Second):
			t.Fatal("no audit record")
		}
		cancel()
		fixture.mu.Lock()
		got := fixture.records[0].Profile
		fixture.mu.Unlock()
		if got != c.want {
			t.Fatalf("%s recorded under %q, want %q", c.profile, got, c.want)
		}
	}
}

// TestReplicateStaleWaitingWordClearsOnAnswer: a Poll error leaves the job
// waiting as upstream:unavailable; the next answer clears the word while the
// job still runs.
func TestReplicateStaleWaitingWordClearsOnAnswer(t *testing.T) {
	release := make(chan struct{})
	backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobRunning}, pollErrs: 1, pollHook: func() { <-release }}
	fixture := newDurableFixture(t, backend)
	p, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, p, "stale-waiting", jobwire.ProfileImageBatch)
	if accepted, err := p.Bind("caller-scope").Submit(submission); err != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	// The executor stops, and the temporary folder goes, only after the
	// held poll is let go.
	executeUntilCleanup(t, p)
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letGo)
	operations := p.BindOperations("caller-scope")
	// The answering poll is held until the word from the failed one is seen.
	for _, want := range []string{"upstream:unavailable", ""} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			observed, err := operations.ObserveWork(submission.Identity)
			if err == nil && observed.Snapshot != nil && observed.Snapshot.State == acceptance.WorkStateRunning && observed.Snapshot.Waiting == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting word %q never shown: %+v", want, observed.Snapshot)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if want != "" {
			letGo()
		}
	}
}

// TestNativeVideoJobFrameGoneEverywhereFailsWithoutGenerating: the reply to
// Start was lost, the provider restarted with an empty content store, and the
// caller deleted the frame. The resend names the frame, the provider cannot
// find it and refuses, and the job fails: nothing generates from the prompt
// alone.
func TestNativeVideoJobFrameGoneEverywhereFailsWithoutGenerating(t *testing.T) {
	frame, digest := testFrame()
	fake := &fakeVideoProvider{loseStart: 1, restartOnLost: true, emptyStoreOnRestart: true}
	v := newVideoRuntime(t, serveFakeVideoProvider(t, fake), frame)
	fake.onLostStart = v.deleteFrame
	submission, accepted := v.submitFrame(t, "video-frame-gone", digest)
	v.run(t)
	final := v.waitVideo(t, submission.Identity)
	if final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != "native:invalid:content:unknown" {
		t.Fatalf("final %+v failure %+v", final, final.Failure)
	}
	assertOneOperation(t, fake, 2, 1, accepted.Receipt.OperationID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i, start := range fake.starts {
		if start.ImageDigest != digest {
			t.Fatalf("Start %d named frame %q, want %q", i, start.ImageDigest, digest)
		}
	}
}

// TestUncertainCancelEndsLocallyAfterTheRequestWindow: a cancel of work no
// upstream has answered keeps reconciling inside the 24 h request_id window,
// and ends the job locally as cancelled, upstream:unanswered, once the window
// has passed.
func TestUncertainCancelEndsLocallyAfterTheRequestWindow(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixMilli())
	backend := &fakeDurableBackend{uncertain: true}
	fixture := newDurableFixture(t, backend)
	fixture.execution.clock = func() time.Time { return time.UnixMilli(clock.Load()) }
	p, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
	if err != nil {
		t.Fatal(err)
	}
	submission := durableSubmission(t, p, "uncertain-cancel", jobwire.ProfileVideo)
	if accepted, err := p.Bind("caller-scope").Submit(submission); err != nil || accepted.Receipt == nil {
		t.Fatalf("submit %+v %v", accepted, err)
	}
	stop := executeUntilCleanup(t, p)
	defer stop()
	operations := p.BindOperations("caller-scope")
	waitFor := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				observed, _ := operations.ObserveWork(submission.Identity)
				t.Fatalf("%s: %+v", what, observed.Snapshot)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("first submission left uncertain", func() bool {
		observed, err := operations.ObserveWork(submission.Identity)
		return err == nil && observed.Snapshot != nil && observed.Snapshot.Waiting == "upstream:uncertain"
	})
	if result, err := p.Bind("caller-scope").CancelWork(submission.Identity); err != nil || result.Outcome != acceptance.CancellationOutcomeRequested {
		t.Fatalf("cancel %+v %v", result, err)
	}
	// Inside the window the cancel waits for an answer.
	clock.Add((2 * time.Second).Milliseconds())
	waitFor("reconciled inside the window", func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.reconciles >= 1
	})
	time.Sleep(50 * time.Millisecond)
	if observed, _ := operations.ObserveWork(submission.Identity); observed.Snapshot == nil || observed.Snapshot.State != acceptance.WorkStateRunning && observed.Snapshot.State != acceptance.WorkStatePending {
		t.Fatalf("cancel ended the job inside the window: %+v", observed.Snapshot)
	}
	clock.Add((24 * time.Hour).Milliseconds())
	final := waitDurable(t, operations, submission.Identity, true)
	if final.State != acceptance.WorkStateCancelled {
		t.Fatalf("final %+v", final)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		fixture.mu.Lock()
		var last Record
		if n := len(fixture.records); n > 0 {
			last = fixture.records[n-1]
		}
		fixture.mu.Unlock()
		if last.Outcome == "cancelled" && last.Reason == "upstream:unanswered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("last record %+v", last)
		}
		time.Sleep(5 * time.Millisecond)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.submits != 1 || backend.cancels != 0 {
		t.Fatalf("submits %d cancels %d", backend.submits, backend.cancels)
	}
}
