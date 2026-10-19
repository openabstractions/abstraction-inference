package inference

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// Request input profiles (INF-V4). Content an application uploads for a
// request is written and read back under one of these bounded profiles, the
// counterpart of the output profiles in ContentWritePreparer. The resource a
// rule names is InputResource(profile).
const (
	// InputImage is an edit input, an edit mask, a vision image or a video's
	// first frame.
	InputImage = "image"
	// InputAudio is audio to transcribe.
	InputAudio = "audio"
)

// InputProfiles lists every request input profile.
var InputProfiles = []string{InputImage, InputAudio}

// InputResource is the rights resource of an input profile, input:<profile>,
// under both abstraction.storage/content.write and content.read.
func InputResource(profile string) string { return "input:" + profile }

// InputProfileOf names the input profile the leading bytes of a content object
// fall under, or "" when they fit none. head needs at most 512 bytes. An image
// is a PNG, JPEG, GIF or WebP, the types image input and vision accept
// (INF-V1); audio is any type transcription accepts (INF-M5).
func InputProfileOf(head []byte) string {
	switch http.DetectContentType(head) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return InputImage
	}
	for _, media := range []string{"audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac"} {
		if audioMatches(head, media) {
			return InputAudio
		}
	}
	return ""
}

// askInput tells the runtime that a consuming request of subject wants the
// input profile it lacks (INF-V4). Only a request that consumes input calls it.
func (p *Provider) askInput(ctx context.Context, subject Subject, profile string) {
	if p.cfg.AskInput != nil {
		p.cfg.AskInput(ctx, subject, profile)
	}
}

// Output profiles: a video job's clip is written and read back under
// output:video, an image batch's or an image@1 call's images under
// output:image, and a speech@1 call's audio under output:speech (INF-S7).
const (
	OutputImage  = "image"
	OutputVideo  = "video"
	OutputSpeech = "speech"
)

func (p *Provider) askOutput(ctx context.Context, subject Subject, profile string) {
	if p.cfg.AskOutput != nil {
		p.cfg.AskOutput(ctx, subject, profile)
	}
}

// namedInputs drops empty digests and repeats.
func namedInputs(digests ...string) []string {
	var named []string
	for _, digest := range digests {
		if digest != "" && !slices.Contains(named, digest) {
			named = append(named, digest)
		}
	}
	return named
}

// imagePartDigests names every image part a chat request carries.
func imagePartDigests(req wire.Request) []string {
	var digests []string
	for _, message := range req.Messages {
		for _, part := range message.Parts {
			if part.Kind == wire.PartKindImage {
				digests = append(digests, part.Digest)
			}
		}
	}
	return namedInputs(digests...)
}

// NamedInputs is one reading of the job store: every digest an inference job
// that has not reached its terminal state names as input (an edit's input or
// mask, a video's first frame). The job holds them until then (INF-V4). The
// answer is an error before Serve, when the store cannot be read entirely,
// and when a job that has not ended cannot be decoded: its input is unknown,
// and unknown is never unheld.
func (e *JobExecution) NamedInputs() (map[string]bool, error) {
	e.storeMu.Lock()
	store := e.store
	e.storeMu.Unlock()
	if store == nil {
		return nil, errors.New("inference jobs: store not served yet")
	}
	records, err := store.List()
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, record := range records {
		if record.Kind != InferenceJobKind || record.State.Terminal() {
			continue
		}
		work, err := decodeInferenceWork(record)
		if err != nil {
			return nil, fmt.Errorf("inference job %s: input unknown: %w", record.ID, err)
		}
		for _, digest := range namedInputs(work.Request.Image.ImageDigest, work.Request.Image.MaskDigest) {
			named[digest] = true
		}
	}
	return named, nil
}

// NamedInputs is every digest a request that has not reached its terminal
// state names as input. Request input is held while it does (INF-V4).
func (p *Provider) NamedInputs() map[string]bool {
	p.mu.Lock()
	ops := make([]*operation, 0, len(p.ops))
	for _, op := range p.ops {
		ops = append(ops, op)
	}
	p.mu.Unlock()
	named := map[string]bool{}
	for _, op := range ops {
		if len(op.inputs) > 0 && !op.isEnded() {
			for _, digest := range op.inputs {
				named[digest] = true
			}
		}
	}
	return named
}

type inputProfileKey struct{}

// WithInputProfile marks a content read as the input of a request of that
// profile: image input for an image edit, vision or video request, audio for
// transcription. The resolver reads request input back only under the
// profile the request consumes (INF-V4).
func WithInputProfile(ctx context.Context, profile string) context.Context {
	return context.WithValue(ctx, inputProfileKey{}, profile)
}

// InputProfileFrom is the input profile a content read is for, or "".
func InputProfileFrom(ctx context.Context) string {
	profile, _ := ctx.Value(inputProfileKey{}).(string)
	return profile
}
