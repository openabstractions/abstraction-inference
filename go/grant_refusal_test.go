package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// askedOutputs records the output profiles AskOutput was told of.
type askedOutputs struct {
	mu       sync.Mutex
	profiles []string
}

func (a *askedOutputs) ask(_ context.Context, _ Subject, profile string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.profiles = append(a.profiles, profile)
}

func (a *askedOutputs) got() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.profiles)
}

// INF-V4, as for jobs: a synchronous image@1 call missing complete on its
// host and its output profile is refused once with not_permitted, naming
// both grants in that order, and the output's paired question is asked in
// the same pass. Nothing reaches the upstream. Missing complete alone reads
// as it always has.
func TestAnImageCallNamesEveryMissingGrant(t *testing.T) {
	f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		t.Errorf("unexpected paid request %s", r.URL.Path)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	asked := &askedOutputs{}
	f.provider.cfg.AskOutput = asked.ask
	f.provider.cfg.Decide = func(context.Context, Subject, string, string) (string, error) { return "not_granted", nil }
	a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:not_granted" {
		t.Fatalf("complete alone missing %+v", a)
	}
	f.store.mu.Lock()
	f.store.deny = true
	f.store.mu.Unlock()
	a = f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:not_granted output:forbidden" {
		t.Fatalf("complete and the output missing %+v", a)
	}
	if got := asked.got(); !slices.Equal(got, []string{OutputImage}) {
		t.Fatalf("output questions %v", got)
	}
}

// The same for speech@1: complete and output:speech in one refusal, the
// output's paired question asked in the same pass; the output alone missing
// reads forbidden.
func TestASpeechCallNamesEveryMissingGrant(t *testing.T) {
	asked := &askedOutputs{}
	store := &speechStore{prepareOutcome: ContentForbidden}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite, c.AskOutput = store.prepare, asked.ask })
	if a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello")); a.Outcome != wire.StartOutcomeForbidden || a.Reason != "output:forbidden" {
		t.Fatalf("the output alone missing %+v", a)
	}
	f.policy.mu.Lock()
	clear(f.policy.rules)
	f.policy.mu.Unlock()
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:not_granted output:forbidden" {
		t.Fatalf("complete and the output missing %+v", a)
	}
	if got := asked.got(); !slices.Equal(got, []string{OutputSpeech, OutputSpeech}) {
		t.Fatalf("output questions %v", got)
	}
}

// grantCalls counts what one request asked of the runtime beyond complete:
// input reads, input and output questions, output preparations.
type grantCalls struct {
	mu                               sync.Mutex
	reads, inputs, outputs, prepares int
}

func (g *grantCalls) count(n *int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	*n++
}

func (g *grantCalls) none() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads+g.inputs+g.outputs+g.prepares == 0
}

// wire installs the counters on c: every input read is forbidden and every
// output preparation refused, so a request that went on would ask both.
func (g *grantCalls) wire(c *Config) {
	c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
		g.count(&g.reads)
		return nil, ContentForbidden
	}
	c.PrepareContentWrite = func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
		g.count(&g.prepares)
		return nil, ContentForbidden
	}
	c.AskInput = func(context.Context, Subject, string) { g.count(&g.inputs) }
	c.AskOutput = func(context.Context, Subject, string) { g.count(&g.outputs) }
}

// videoJobAdmission is the outcome and reason of an edit video job's
// admission under c's decisions, for the frame testPNG.
func videoJobAdmission(t *testing.T, adjust func(*Config)) (acceptance.AcceptanceOutcome, string) {
	t.Helper()
	v := newVideoRuntimeWith(t, videoEndpoint(t), testPNG, adjust)
	sum := sha256.Sum256(testPNG)
	image := jobwire.ImageRequest{Mode: jobwire.ModeEdit, Extensions: map[string]string{}, ImageDigest: "sha256:" + hex.EncodeToString(sum[:]),
		ImageMediaType: "image/png", Model: testVideoModel, Prompt: "a lighthouse at dusk", Size: "832x480", Count: 1,
		Guarantees: []jobwire.RequestGuarantee{jobwire.RequestGuaranteeLocalOnly}}
	document := &jobwire.Document{Request: &jobwire.Request{Profile: jobwire.ProfileVideo, DurationMs: 2000, Image: image}}
	return v.execution.CheckAdmission("caller-scope", InferenceJobKind, jobwire.Encode(document), []string{RecoverableUpstreamGuarantee})
}

// INF-V4: a denied complete, or an unknown action, is final. The request is
// refused with that word alone and asks nothing more: no input is read, no
// input or output question is admitted and no output is prepared, for an
// image edit, a speech call and a video job alike. Only not_granted gathers
// the other grants, since only then can answers let the request run.
func TestAFinalCompleteRefusalAsksNothingElse(t *testing.T) {
	for _, word := range []string{"denied", "unknown_action"} {
		decide := func(context.Context, Subject, string, string) (string, error) { return word, nil }
		t.Run(word+"/image", func(t *testing.T) {
			f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Errorf("unexpected paid request %s", r.URL.Path)
			})
			calls := &grantCalls{}
			calls.wire(&f.provider.cfg)
			f.provider.cfg.Decide = decide
			req := imageRequestFixture(wire.ImageModeEdit, 1)
			req.ImageDigest, req.ImageMediaType = "sha256:"+strings.Repeat("1", 64), "image/png"
			if a := f.provider.StartImage(context.Background(), caller, req); a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:"+word {
				t.Fatalf("admission %+v", a)
			}
			if !calls.none() {
				t.Fatalf("a final refusal asked more: %+v", calls)
			}
		})
		t.Run(word+"/speech", func(t *testing.T) {
			calls := &grantCalls{}
			f := setup(t, router.WireOpenAICompatible, func(c *Config) { calls.wire(c); c.Decide = decide })
			if a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello")); a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:"+word {
				t.Fatalf("admission %+v", a)
			}
			if !calls.none() {
				t.Fatalf("a final refusal asked more: %+v", calls)
			}
		})
		t.Run(word+"/job", func(t *testing.T) {
			calls := &grantCalls{}
			outcome, reason := videoJobAdmission(t, func(c *Config) { calls.wire(c); c.Decide = decide })
			if outcome != acceptance.AcceptanceOutcomeForbidden || reason != "rights:"+word {
				t.Fatalf("admission %s %q", outcome, reason)
			}
			if !calls.none() {
				t.Fatalf("a final refusal asked more: %+v", calls)
			}
		})
	}
}

// INF-V4: a decision word other than permitted, denied, not_granted or
// unknown_action could not be obtained, so the request is unavailable and
// asks nothing, for a call and a job alike. Reading every word other than
// permitted as a refusal fails this.
func TestOtherDecisionWordsReadUnavailable(t *testing.T) {
	for _, word := range []string{"unavailable", ""} {
		decide := func(context.Context, Subject, string, string) (string, error) { return word, nil }
		t.Run("image", func(t *testing.T) {
			f := newImageFixture(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				t.Errorf("unexpected paid request %s", r.URL.Path)
			})
			calls := &grantCalls{}
			calls.wire(&f.provider.cfg)
			f.provider.cfg.Decide = decide
			if a := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1)); a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "rights:unavailable" {
				t.Fatalf("word %q: admission %+v", word, a)
			}
			if !calls.none() {
				t.Fatalf("word %q asked: %+v", word, calls)
			}
		})
		t.Run("speech", func(t *testing.T) {
			calls := &grantCalls{}
			f := setup(t, router.WireOpenAICompatible, func(c *Config) { calls.wire(c); c.Decide = decide })
			if a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello")); a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "rights:unavailable" {
				t.Fatalf("word %q: admission %+v", word, a)
			}
		})
		t.Run("job", func(t *testing.T) {
			calls := &grantCalls{}
			outcome, reason := videoJobAdmission(t, func(c *Config) { calls.wire(c); c.Decide = decide })
			if outcome != acceptance.AcceptanceOutcomeUnavailable || reason != "rights:unavailable" {
				t.Fatalf("word %q: admission %s %q", word, outcome, reason)
			}
			if !calls.none() {
				t.Fatalf("word %q asked: %+v", word, calls)
			}
		})
	}
}
