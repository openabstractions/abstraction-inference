package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

const testImageModel = "sdxl-turbo"

// fakeImageProvider is a native image provider such as pytorch-image: it
// serves image@1 and its content reader on one authenticated endpoint. Every
// operation reports progress and then, unless hold is set, its images and
// its end; a cancelled operation ends cancelled.
type fakeImageProvider struct {
	mu        sync.Mutex
	store     *storage.ContentStore
	images    [][]byte
	hold      bool
	starts    []wire.ImageRequest
	cancels   []string
	observed  map[string]int
	counts    map[string]int64
	cancelled map[string]bool
}

func (f *fakeImageProvider) Start(request wire.ImageRequest) (wire.Admission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, request)
	operation := fmt.Sprintf("image-%d", len(f.starts))
	f.counts[operation] = request.Count
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: operation, RetentionMs: 60000}, nil
}

func (f *fakeImageProvider) Observe(operation string, cursor, maxDeltas, _, _ int64) (wire.DeltaPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count, known := f.counts[operation]
	if !known {
		return wire.DeltaPage{Outcome: wire.PageOutcomeUnknown, Deltas: []wire.Delta{}, Next: cursor}, nil
	}
	f.observed[operation]++
	deltas := []wire.Delta{{Kind: wire.DeltaKindImageProgress, ImageProgress: &wire.ImageProgress{Percent: 40, Message: "sampling"}}}
	end := func(reply wire.ImageReply) {
		reply.Host, reply.Model = "image-provider", testImageModel
		deltas = append(deltas, wire.Delta{Kind: wire.DeltaKindEnd, ImageEnd: &reply})
	}
	switch {
	case f.cancelled[operation]:
		end(wire.ImageReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "cancel", Deliveries: []wire.Delivery{}})
	case !f.hold:
		var deliveries []wire.Delivery
		for i := int64(0); i < count; i++ {
			data := f.images[i]
			sum := sha256.Sum256(data)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			if _, stored := f.store.Find(digest); !stored {
				ref, err := f.store.Place(digest, int64(len(data)))
				if err != nil {
					return wire.DeltaPage{}, err
				}
				if err := os.WriteFile(f.store.Path(ref), data, 0o600); err != nil {
					return wire.DeltaPage{}, err
				}
				if err := f.store.Commit(ref); err != nil {
					return wire.DeltaPage{}, err
				}
			}
			delivery := wire.Delivery{Digest: digest, MediaType: "image/png", Size: int64(len(data)), Location: "local"}
			deliveries = append(deliveries, delivery)
			deltas = append(deltas, wire.Delta{Kind: wire.DeltaKindImageResult, ImageResult: &wire.ImageResult{Index: i, Delivery: delivery}})
		}
		end(wire.ImageReply{Outcome: wire.ReplyOutcomeCompleted, Deliveries: deliveries, Images: count})
	}
	page := wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
	for i := cursor; i < int64(len(deltas)) && int64(len(page.Deltas)) < maxDeltas; i++ {
		delta := deltas[i]
		delta.Sequence = i
		page.Deltas = append(page.Deltas, delta)
		page.Next = i + 1
		page.AtEnd = delta.Kind == wire.DeltaKindEnd
	}
	return page, nil
}

func (f *fakeImageProvider) Cancel(operation string) (wire.Cancellation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, operation)
	if _, known := f.counts[operation]; !known {
		return wire.Cancellation{Outcome: wire.CancelOutcomeUnknown}, nil
	}
	f.cancelled[operation] = true
	cancelled := wire.ReplyOutcomeCancelled
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled, ReplyOutcome: &cancelled}, nil
}

// serveFakeImageProvider listens on a native endpoint with Program proof and
// serves image@1 beside the provider's content reader.
func serveFakeImageProvider(t *testing.T, f *fakeImageProvider) string {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	store, err := storage.NewContentStore("image-provider", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.observed, f.counts, f.cancelled = store, map[string]int{}, map[string]int64{}, map[string]bool{}
	allow := func(context.Context, *identity.Peer, string) error { return nil }
	content, err := storageService.NewComponent(store, allow)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := videoEndpoint(t)
	if err := listenFakeProvider(t, &wire.ImageDispatcher{Handler: f}, content, endpoint); err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func submitImageBatch(t *testing.T, v *videoRuntime, key string, count int64) (acceptance.Submission, acceptance.AcceptanceResult) {
	t.Helper()
	return v.submitJob(t, key, jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: testImageModel, Mode: jobwire.ModeGenerate, Prompt: "a lighthouse at dusk", Size: "512x512", Count: count,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}, Extensions: map[string]string{}}})
}

// A durable local image batch routed to a native image provider (pytorch,
// lemonade, thenoise) runs through its image@1 service: the provider
// receives the job's model, prompt, size and count, and the job completes
// with every image copied into the caller's store. On 2026-11-10 OpenCode's
// oa_inference_job_submit was refused host:unsupported, because durable
// jobs reached only native video providers and Replicate.
func TestNativeImageJobCompletesThroughProviderImageService(t *testing.T) {
	fake := &fakeImageProvider{images: [][]byte{testPNG, []byte("\x89PNG\r\n\x1a\nsecond")}}
	v := newNativeJobRuntime(t, "image-provider", serveFakeImageProvider(t, fake), testImageModel, router.ProfileImage, nil, nil)
	submission, accepted := submitImageBatch(t, v, "image-complete", 2)
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
	if err != nil || document.Result == nil || len(document.Result.Deliveries) != 2 || document.Result.Host != "image-provider" {
		t.Fatalf("result document %+v %v", document, err)
	}
	for i, delivery := range document.Result.Deliveries {
		sum := sha256.Sum256(fake.images[i])
		if delivery.Digest != "sha256:"+hex.EncodeToString(sum[:]) || delivery.MediaType != "image/png" || delivery.Location != "local" || delivery.Size != int64(len(fake.images[i])) {
			t.Fatalf("delivery %d %+v", i, delivery)
		}
	}
	v.mu.Lock()
	committed := v.committed
	v.mu.Unlock()
	if len(committed) != 2 || !bytes.Equal(committed[0], fake.images[0]) || !bytes.Equal(committed[1], fake.images[1]) {
		t.Fatalf("caller store received %d outputs", len(committed))
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != 1 {
		t.Fatalf("provider saw %d starts", len(fake.starts))
	}
	sent := fake.starts[0]
	if sent.Model != testImageModel || sent.Prompt != "a lighthouse at dusk" || sent.Size != "512x512" || sent.Count != 2 || sent.Mode != wire.ImageModeGenerate || sent.Credential != "" {
		t.Fatalf("provider received %+v", sent)
	}
}

// Cancelling a running image job cancels the provider's operation once and
// ends the job cancelled.
func TestNativeImageJobCancelReachesProvider(t *testing.T) {
	fake := &fakeImageProvider{hold: true}
	v := newNativeJobRuntime(t, "image-provider", serveFakeImageProvider(t, fake), testImageModel, router.ProfileImage, nil, nil)
	submission, accepted := submitImageBatch(t, v, "image-cancel", 1)
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	v.run(t)
	operations := v.jobs.BindOperations("caller-scope")
	deadline := time.Now().Add(5 * time.Second)
	for {
		observed, err := operations.ObserveWork(submission.Identity)
		if err == nil && observed.Snapshot != nil && observed.Snapshot.Progress.Done == 40 && observed.Snapshot.Progress.Total == 100 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the provider's progress never reached the snapshot: %+v", observed.Snapshot)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result, err := v.jobs.Bind("caller-scope").CancelWork(submission.Identity); err != nil || result.Outcome != acceptance.CancellationOutcomeRequested {
		t.Fatalf("cancel %+v %v", result, err)
	}
	final := waitDurable(t, operations, submission.Identity, true)
	if final.State != acceptance.WorkStateCancelled {
		t.Fatalf("final %+v", final)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.cancels) != 1 || fake.cancels[0] != "image-1" {
		t.Fatalf("provider cancels %v", fake.cancels)
	}
}

// A job no backend serves on the picked host is still refused: a video job
// for a model a native image provider lists finds no video host, and the
// image host the router picks serves image batches only.
func TestNativeImageProviderRefusesAVideoJob(t *testing.T) {
	fake := &fakeImageProvider{}
	v := newNativeJobRuntime(t, "image-provider", serveFakeImageProvider(t, fake), testImageModel, router.ProfileImage, nil, nil)
	_, refused := v.submitJob(t, "image-video", jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 2000, Image: jobwire.ImageRequest{
		Model: testImageModel, Mode: jobwire.ModeGenerate, Prompt: "a lighthouse at dusk", Size: "512x512", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}, Extensions: map[string]string{}}})
	if refused.Outcome != acceptance.AcceptanceOutcomeInvalid || refused.Reason != "host:unsupported" || refused.Receipt != nil {
		t.Fatalf("refused %+v, want invalid host:unsupported", refused)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.starts) != 0 {
		t.Fatalf("provider saw %d starts", len(fake.starts))
	}
}
