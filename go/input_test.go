package inference

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	job "github.com/openabstractions/abstraction-job/go"
)

// INF-V4: request input is classified into its profile by its own bytes.
func TestInputProfileOfClassifiesRequestInput(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	for _, c := range []struct {
		name string
		head []byte
		want string
	}{
		{"png", png, InputImage},
		{"jpeg", jpeg, InputImage},
		{"wav", wavFixture(), InputAudio},
		{"flac", []byte("fLaC\x00\x00\x00\x22"), InputAudio},
		{"text", []byte("plain text is no request input"), ""},
		{"empty", nil, ""},
	} {
		if got := InputProfileOf(c.head); got != c.want {
			t.Errorf("%s: profile %q, want %q", c.name, got, c.want)
		}
	}
	if InputResource(InputAudio) != "input:audio" || InputResource(InputImage) != "input:image" {
		t.Fatal("input resources must be input:<profile>")
	}
}

// INF-V4: an inference job names its edit input and mask until its terminal
// state; before its store is served the answer is unknown, never "unheld".
func TestInferenceJobNamesItsInputUntilTerminal(t *testing.T) {
	execution := &JobExecution{}
	input, mask := audioDigest([]byte("edit input")), audioDigest([]byte("edit mask"))
	if _, err := execution.NamedInputs(); err == nil {
		t.Fatal("an unserved job store answered")
	}
	work := inferenceJobWork{Scope: "caller-scope", Request: jobwire.Request{Profile: jobwire.ProfileImageBatch, Image: jobwire.ImageRequest{
		Model: "black-forest-labs/flux-kontext", Mode: jobwire.ModeEdit, Prompt: "make it night", Size: "1024x1024", Count: 1,
		ImageDigest: input, ImageMediaType: "image/png", MaskDigest: mask, MaskMediaType: "image/png",
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}, Extensions: map[string]string{},
	}}}
	if err := validateInferenceJob(work.Request); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	store := job.NewMemoryStore()
	id, err := store.Submit(job.Record{ID: "edit", Kind: InferenceJobKind, State: job.StatePending, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	execution.storeMu.Lock()
	execution.store = store
	execution.storeMu.Unlock()
	named, err := execution.NamedInputs()
	if err != nil || len(named) != 2 || !named[input] || !named[mask] {
		t.Fatalf("pending job names %v %v", named, err)
	}
	held, err := store.Claim(id, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, held.Lease.Epoch, func(record *job.Record) error {
		record.State = job.StateFailed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if named, err := execution.NamedInputs(); err != nil || len(named) != 0 {
		t.Fatalf("terminal job still names its input: %v %v", named, err)
	}
	// A job that has not ended and cannot be decoded names unknown input: the
	// reading is an error, which keeps every object.
	if _, err := store.Submit(job.Record{ID: "garbled", Kind: InferenceJobKind, State: job.StatePending, Spec: []byte(`{"scope":""}`)}); err != nil {
		t.Fatal(err)
	}
	if named, err := execution.NamedInputs(); err == nil {
		t.Fatalf("an undecodable live job read as naming %v", named)
	}
}

// INF-V4: a consuming request refused for want of a grant tells the runtime
// which input profile it consumes, once at a not_granted complete decision and
// once at a refused input read; a denied decision asks nothing.
func TestConsumingRequestAsksForItsInputProfile(t *testing.T) {
	for _, c := range []struct {
		name     string
		decide   string
		resolved ContentOutcome
		asked    []string
	}{
		{"complete not granted", "not_granted", ContentResolved, []string{InputAudio}},
		{"complete denied", "denied", ContentResolved, nil},
		{"input read refused", "permitted", ContentForbidden, []string{InputAudio}},
		{"admitted", "permitted", ContentResolved, nil},
	} {
		var asked []string
		f, audio := transcriptionFixture(t, func(cfg *Config) {
			cfg.Decide = func(context.Context, Subject, string, string) (string, error) { return c.decide, nil }
			resolve := cfg.ResolveContent
			cfg.ResolveContent = func(ctx context.Context, s Subject, digest string, limit int64) ([]byte, ContentOutcome) {
				if c.resolved != ContentResolved {
					return nil, c.resolved
				}
				return resolve(ctx, s, digest, limit)
			}
			cfg.AskInput = func(_ context.Context, s Subject, profile string) {
				if s != caller {
					t.Fatalf("asked for %+v", s)
				}
				asked = append(asked, profile)
			}
		})
		f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
		if len(asked) != len(c.asked) || (len(asked) == 1 && asked[0] != c.asked[0]) {
			t.Fatalf("%s: asked %v, want %v", c.name, asked, c.asked)
		}
	}
}

// INF-V4: a request names its input until its terminal state, and no longer.
func TestRequestNamesItsInputUntilItEnds(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	f.up.transcriptionHold = true
	digest := audioDigest(audio)
	if f.provider.NamedInputs()[digest] {
		t.Fatal("input named before any request")
	}
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if named := f.provider.NamedInputs(); len(named) != 1 || !named[digest] {
		t.Fatalf("a live request names %v", named)
	}
	if c := f.provider.CancelTranscription(caller, a.Operation); c.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel %+v", c)
	}
	_, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCancelled {
		t.Fatalf("end %+v", end)
	}
	if f.provider.NamedInputs()[digest] {
		t.Fatal("an ended request still names its input")
	}
}

// INF-V4: each request read names the input profile its request consumes.
func TestInputReadsNameTheConsumingProfile(t *testing.T) {
	var profiles []string
	f, audio := transcriptionFixture(t, func(cfg *Config) {
		resolve := cfg.ResolveContent
		cfg.ResolveContent = func(ctx context.Context, s Subject, digest string, limit int64) ([]byte, ContentOutcome) {
			profiles = append(profiles, InputProfileFrom(ctx))
			return resolve(ctx, s, digest, limit)
		}
	})
	if a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio)); a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if len(profiles) != 1 || profiles[0] != InputAudio {
		t.Fatalf("transcription read under %v", profiles)
	}
	if InputProfileFrom(context.Background()) != "" {
		t.Fatal("a plain context names a profile")
	}
}
