package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sync"
	"testing"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// videoGrants is a video runtime whose three grants a test switches: complete
// on the host, the first frame's input profile and the output profile. It
// records every decision, question and output write.
type videoGrants struct {
	mu                               sync.Mutex
	complete, input, output          bool
	decided, inputs, outputs, writes []string
}

func newVideoGrants(t *testing.T, frame []byte) (*videoRuntime, *videoGrants) {
	g := &videoGrants{}
	v := newVideoRuntimeWith(t, videoEndpoint(t), frame, func(c *Config) {
		c.Decide = func(_ context.Context, _ Subject, action, resource string) (string, error) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.decided = append(g.decided, action+" "+resource)
			if g.complete {
				return "permitted", nil
			}
			return "not_granted", nil
		}
		resolve := c.ResolveContent
		c.ResolveContent = func(ctx context.Context, s Subject, digest string, max int64) ([]byte, ContentOutcome) {
			g.mu.Lock()
			allowed := g.input
			g.mu.Unlock()
			if !allowed {
				return nil, ContentForbidden
			}
			return resolve(ctx, s, digest, max)
		}
		prepare := c.PrepareContentWrite
		c.PrepareContentWrite = func(ctx context.Context, s Subject, profile, media string, max int64) (ContentCommitter, ContentOutcome) {
			g.mu.Lock()
			g.writes = append(g.writes, profile+" "+media)
			allowed := g.output
			g.mu.Unlock()
			if !allowed {
				return nil, ContentForbidden
			}
			return prepare(ctx, s, profile, media, max)
		}
		c.AskInput = func(_ context.Context, _ Subject, profile string) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.inputs = append(g.inputs, profile)
		}
		c.AskOutput = func(_ context.Context, _ Subject, profile string) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.outputs = append(g.outputs, profile)
		}
	})
	return v, g
}

func (g *videoGrants) set(complete, input, output bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.complete, g.input, g.output = complete, input, output
}

// INF-V4, INF-S7, job JOB-A16: a video job decides every grant it needs before
// it answers: complete on the host, its first frame's input profile and its
// output profile, video. Missing all three, its submission is refused once,
// forbidden, naming each, and the first-use question of each is admitted in
// the same pass. The job service seals nothing, so the same key is accepted
// once the person has answered. On 2026-10-02 an application's video job was
// refused one grant per submission with its output decided as image's
// (research/live-runs/2026-10-02.md, phase 2 finding 4), and a refusal for
// output:video read invalid (research/live-runs/2026-10-02-walkthrough.md,
// D11).
func TestAVideoJobIsRefusedOnceNamingEveryMissingGrant(t *testing.T) {
	frame := testPNG
	v, g := newVideoGrants(t, frame)
	sum := sha256.Sum256(frame)
	request := jobwire.ImageRequest{Mode: jobwire.ModeEdit, Extensions: map[string]string{},
		ImageDigest: "sha256:" + hex.EncodeToString(sum[:]), ImageMediaType: "image/png"}
	submission, refused := v.submit(t, "video-ungranted", request)
	if want := "rights:not_granted content:read:forbidden output:forbidden"; refused.Outcome != acceptance.AcceptanceOutcomeForbidden ||
		refused.Reason != want || refused.Receipt != nil {
		t.Fatalf("refused %+v, want forbidden %q with no receipt", refused, want)
	}
	g.mu.Lock()
	complete := ActionComplete + " " + ResourceHost("video-provider")
	if len(g.decided) == 0 || slices.ContainsFunc(g.decided, func(d string) bool { return d != complete }) {
		t.Fatalf("decided %v", g.decided)
	}
	if len(g.inputs) == 0 || slices.ContainsFunc(g.inputs, func(p string) bool { return p != InputImage }) {
		t.Fatalf("input questions %v", g.inputs)
	}
	// Each planning pass asks for the output once, as video, beside the others.
	if len(g.outputs) != len(g.decided) || slices.ContainsFunc(g.outputs, func(p string) bool { return p != OutputVideo }) ||
		slices.ContainsFunc(g.writes, func(w string) bool { return w != OutputVideo+" video/*" }) {
		t.Fatalf("output questions %v, output writes %v: a video job's clip is output:video", g.outputs, g.writes)
	}
	g.mu.Unlock()

	// The person allows complete and the input: the same key is still
	// refused, now for its output alone.
	g.set(true, true, false)
	again := v.jobs.Bind("caller-scope")
	result, err := again.Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeForbidden || result.Reason != "output:forbidden" || result.Receipt != nil {
		t.Fatalf("refused for output:video alone %+v %v", result, err)
	}
	// Allowed the output too, the same key is accepted.
	g.set(true, true, true)
	result, err = again.Submit(submission)
	if err != nil || result.Outcome != acceptance.AcceptanceOutcomeAccepted || result.Receipt == nil {
		t.Fatalf("the same key after Allow: %+v %v", result, err)
	}
}

// INF-V4: a job admitted with every grant and then refused one before it runs
// fails once, naming each grant missing at execution.
func TestAVideoJobLosingItsGrantsFailsNamingEach(t *testing.T) {
	frame := testPNG
	v, g := newVideoGrants(t, frame)
	g.set(true, true, true)
	sum := sha256.Sum256(frame)
	submission, accepted := v.submit(t, "video-revoked", jobwire.ImageRequest{Mode: jobwire.ModeEdit, Extensions: map[string]string{},
		ImageDigest: "sha256:" + hex.EncodeToString(sum[:]), ImageMediaType: "image/png"})
	if accepted.Receipt == nil {
		t.Fatalf("submit %+v", accepted)
	}
	g.set(false, false, false)
	v.run(t)
	final := v.waitVideo(t, submission.Identity)
	if want := "rights:not_granted content:read:forbidden output:forbidden"; final.State != acceptance.WorkStateFailed || final.Failure == nil || final.Failure.Message != want {
		t.Fatalf("refused %+v (%+v), want one failure %q", final, final.Failure, want)
	}
}
