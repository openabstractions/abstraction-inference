package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func wavFixture() []byte {
	// The provider treats content as opaque after validating its declared media
	// type. The fake upstream describes this fixture as thirty seconds long.
	return append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 64)...)
}

func audioDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func transcriptionRequest(data []byte) wire.TranscriptionRequest {
	return wire.TranscriptionRequest{Model: "anthropic/claude-sonnet-5", AudioDigest: audioDigest(data), MediaType: "audio/wav",
		Timestamps: wire.TimestampModeSegment, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "openrouter"}
}

func transcriptionFixture(t *testing.T, tune func(*Config)) (*fixture, []byte) {
	t.Helper()
	audio := wavFixture()
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.ResolveContent = func(_ context.Context, subject Subject, digest string, limit int64) ([]byte, ContentOutcome) {
			if subject != caller {
				t.Fatalf("resolver subject %+v, want %+v", subject, caller)
			}
			if digest != audioDigest(audio) {
				return nil, ContentUnknown
			}
			if int64(len(audio)) > limit {
				return nil, ContentTooLarge
			}
			return append([]byte(nil), audio...), ContentResolved
		}
		if tune != nil {
			tune(c)
		}
	})
	return f, audio
}

func drainTranscription(t *testing.T, p *Provider, subject Subject, id string, cursor int64) ([]wire.Delta, wire.TranscriptionReply) {
	t.Helper()
	var all []wire.Delta
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page := p.ObserveTranscription(context.Background(), subject, id, cursor, 1, 65536, 500)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Sequence != cursor {
				t.Fatalf("sequence %d, cursor %d", d.Sequence, cursor)
			}
			cursor++
			all = append(all, d)
			if d.Kind == wire.DeltaKindEnd {
				if d.TranscriptionEnd == nil {
					t.Fatal("transcription end missing")
				}
				return all, *d.TranscriptionEnd
			}
		}
	}
	t.Fatal("transcription did not end")
	return nil, wire.TranscriptionReply{}
}

func TestTranscriptionOrdersThirtySecondSegmentsAndResumesCursor(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	// Chat cannot observe or cancel a transcription operation.
	if got := f.provider.Observe(context.Background(), caller, a.Operation, 0, 1, 65536, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("cross-profile observe %+v", got)
	}
	if got := f.provider.Cancel(caller, a.Operation); got.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("cross-profile cancel %+v", got)
	}

	first := f.provider.ObserveTranscription(context.Background(), caller, a.Operation, 0, 1, 65536, 1000)
	if len(first.Deltas) != 1 || first.Next != 1 || first.Deltas[0].Segment == nil || first.Deltas[0].Segment.Text != "one" {
		t.Fatalf("first page %+v", first)
	}
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, first.Next)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.DurationMs != 30000 {
		t.Fatalf("end %+v", end)
	}
	texts := []string{"one"}
	var starts = []int64{0}
	for _, d := range deltas {
		if d.Segment != nil {
			texts, starts = append(texts, d.Segment.Text), append(starts, d.Segment.StartMs)
		}
	}
	if strings.Join(texts, "|") != "one| two| three" || len(starts) != 3 || starts[1] != 10000 || starts[2] != 20000 {
		t.Fatalf("segments %q at %v", texts, starts)
	}
	posts := f.up.posts()
	if len(posts) != 1 || posts[0].path != "/api/v1/audio/transcriptions" || !strings.Contains(posts[0].body, "verbose_json") {
		t.Fatalf("posts %+v", posts)
	}
	records := f.waitLogged(t, 1)
	if len(records) != 1 || records[0].Profile != router.ProfileTranscription || records[0].AudioSeconds != 30 {
		t.Fatalf("records %+v", records)
	}
}

func TestTranscriptionContentRefusalsSpendNothing(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	req := transcriptionRequest(audio)
	req.AudioDigest = "sha256:" + strings.Repeat("0", 64)
	a := f.provider.StartTranscription(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:unknown" {
		t.Fatalf("unknown %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != 1 { // one apply surveyed /models
		t.Fatalf("posts %d applies %d", len(f.up.posts()), f.applier.count())
	}

	before := f.applier.count()
	f.policy.rules[caller.Program+" "+ActionComplete+" host:openrouter"] = "denied"
	a = f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeNotPermitted || a.Reason != "rights:denied" {
		t.Fatalf("denied %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != before {
		t.Fatalf("denied spent: posts %d applies %d/%d", len(f.up.posts()), f.applier.count(), before)
	}
}

func TestTranscriptionInputBoundIsCheckedBeforeCredentialOrUpstream(t *testing.T) {
	f, audio := transcriptionFixture(t, func(c *Config) { c.MaxAudioBytes = 32 })
	before := f.applier.count()
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:too-large" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != before {
		t.Fatalf("bounded input spent: posts %d applies %d/%d", len(f.up.posts()), f.applier.count(), before)
	}
}

func TestTranscriptionOtherContentScopeIsForbiddenWithoutSpend(t *testing.T) {
	f, audio := transcriptionFixture(t, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			return nil, ContentForbidden
		}
	})
	before := f.applier.count()
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeForbidden || a.Reason != "content:read:forbidden" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != before {
		t.Fatalf("forbidden content spent: posts %d applies %d/%d", len(f.up.posts()), f.applier.count(), before)
	}
}

// TestTranscriptionWriteGrantDoesNotNameReadAsWrite models the real gateway
// sequence on one digest: the window stores the uploaded audio under
// content.write (gateway/transcription.go storeGatewayAudio) before the
// provider ever runs, then the provider resolves that same digest under
// content.read here (resolveAudio). A caller granted only content.write for
// the digest is refused on read, and the refusal must name the operation
// that was actually refused rather than repeat the write grant's word.
func TestTranscriptionWriteGrantDoesNotNameReadAsWrite(t *testing.T) {
	// readGranted models the caller's content.read grants only. The digest
	// below is written (uploaded and stored by the window under a separate
	// content.write grant this fixture never models) but never added here,
	// exactly as serve/runtime_content_test.go proves at the rights layer
	// (TestRuntimeContentConfiguresScopedReadAndWrite: "write implicitly
	// granted read"). content.read for it must still be refused.
	readGranted := map[string]bool{}
	f, audio := transcriptionFixture(t, func(c *Config) {
		c.ResolveContent = func(_ context.Context, _ Subject, digest string, limit int64) ([]byte, ContentOutcome) {
			if readGranted[digest] {
				t.Fatal("this test never grants content.read")
			}
			return nil, ContentForbidden
		}
	})
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeForbidden {
		t.Fatalf("admission %+v", a)
	}
	if a.Reason != "content:read:forbidden" {
		t.Fatalf("reason %q does not name the read refusal", a.Reason)
	}
	if strings.Contains(a.Reason, "write") {
		t.Fatalf("read refusal named write: %q", a.Reason)
	}
}

func TestTranscriptionContentInfrastructureFailureIsUnavailable(t *testing.T) {
	f, audio := transcriptionFixture(t, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			return nil, ContentUnavailable
		}
	})
	before := f.applier.count()
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "content:unavailable" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != before {
		t.Fatalf("unavailable content spent: posts %d applies %d/%d", len(f.up.posts()), f.applier.count(), before)
	}
}

func TestTranscriptionCannotObserveOrCancelChatOperation(t *testing.T) {
	f, _ := transcriptionFixture(t, nil)
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if got := f.provider.ObserveTranscription(context.Background(), caller, a.Operation, 0, 1, 65536, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("cross-profile observe %+v", got)
	}
	if got := f.provider.CancelTranscription(caller, a.Operation); got.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("cross-profile cancel %+v", got)
	}
	_, end, _ := drain(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("chat end %+v", end)
	}
}

func TestTranscriptionIdleCancelsUpstream(t *testing.T) {
	f, audio := transcriptionFixture(t, func(c *Config) { c.Idle = 80 * time.Millisecond })
	f.up.transcriptionHold = true
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	select {
	case <-f.up.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not cancelled after idle timeout")
	}
	_, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCancelled || end.Reason != "idle" {
		t.Fatalf("end %+v", end)
	}
}

func TestTranscriptionAudioSecondsCeilingRefusesNextRequest(t *testing.T) {
	ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"openrouter": {AudioSecondsPerDay: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, audio := transcriptionFixture(t, func(c *Config) { c.Ceilings = ceilings })
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("first %+v", a)
	}
	_, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
	a = f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeBudgetExceeded || a.Reason != "ceiling:audio_seconds:openrouter" {
		t.Fatalf("second %+v", a)
	}
}

func TestWhisperCPPRefusesNonWAVBeforeContent(t *testing.T) {
	h := router.WhisperCPP("http://127.0.0.1:1")
	r := router.New(h)
	r.Survey()
	contentCalls := 0
	p, err := New(Config{Router: r, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		ResolveContent: func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			contentCalls++
			return nil, ContentUnknown
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	req := wire.TranscriptionRequest{Model: "whisper", AudioDigest: "sha256:" + strings.Repeat("0", 64), MediaType: "audio/mpeg", Timestamps: wire.TimestampModeSegment}
	a := p.StartTranscription(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "format:audio/mpeg" || contentCalls != 0 {
		t.Fatalf("admission %+v, content calls %d", a, contentCalls)
	}
}

func TestOversizeUpstreamTranscriptIsUnavailable(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	f.up.transcript = `{"language":"en","duration":30,"segments":[{"start":0,"end":30,"text":"` + strings.Repeat("x", maxTranscriptTextBytes+1) + `"}]}`
	a := f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "upstream:segments" {
		t.Fatalf("end %+v", end)
	}
	for _, d := range deltas {
		if d.Segment != nil {
			t.Fatalf("oversize segment leaked: %+v", d.Segment)
		}
	}
}

func TestTranscriptionWithoutTimestampsStillReturnsText(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	f.up.transcript = `{"language":"en","duration":30,"text":"plain transcript"}`
	req := transcriptionRequest(audio)
	req.Timestamps = wire.TimestampModeNone
	a := f.provider.StartTranscription(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCompleted || len(deltas) != 2 || deltas[0].Segment == nil || deltas[0].Segment.Text != "plain transcript" || deltas[0].Segment.EndMs != 30000 {
		t.Fatalf("deltas %+v end %+v", deltas, end)
	}
}

func TestMalformedUpstreamTimesAndLanguageCannotOverflowUsage(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		timestamps wire.TimestampMode
	}{
		{"negative duration", `{"language":"en","duration":-1,"text":"bad"}`, wire.TimestampModeNone},
		{"duration overflow", `{"language":"en","duration":1e100,"text":"bad"}`, wire.TimestampModeNone},
		{"segment overflow", `{"language":"en","duration":30,"segments":[{"start":0,"end":1e100,"text":"bad"}]}`, wire.TimestampModeSegment},
		{"language bound", `{"language":"` + strings.Repeat("a", 36) + `","duration":30,"text":"bad"}`, wire.TimestampModeNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, audio := transcriptionFixture(t, nil)
			f.up.transcript = tt.response
			req := transcriptionRequest(audio)
			req.Timestamps = tt.timestamps
			a := f.provider.StartTranscription(context.Background(), caller, req)
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			_, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
			if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "upstream:segments" {
				t.Fatalf("end %+v", end)
			}
			records := f.waitLogged(t, 1)
			if len(records) != 1 || records[0].AudioSeconds != 0 {
				t.Fatalf("records %+v", records)
			}
		})
	}
}

func TestDeepgramPrerecordedSendsRawAudioAndReturnsTimestampUnits(t *testing.T) {
	audio := wavFixture()
	f := setup(t, router.WireDeepgramPrerecorded, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			return append([]byte(nil), audio...), ContentResolved
		}
	})
	f.policy.rules[caller.Program+" "+ActionComplete+" host:deepgram"] = "permitted"
	req := wire.TranscriptionRequest{Model: "nova-3", AudioDigest: audioDigest(audio), MediaType: "audio/wav", Timestamps: wire.TimestampModeSegmentAndWord,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "deepgram"}
	a := f.provider.StartTranscription(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Language != "en" || end.DurationMs != 30000 {
		t.Fatalf("end %+v", end)
	}
	var segments, words int
	for _, d := range deltas {
		if d.Segment != nil && d.Segment.Kind == wire.TranscriptUnitKindSegment {
			segments++
		}
		if d.Segment != nil && d.Segment.Kind == wire.TranscriptUnitKindWord {
			words++
		}
	}
	posts := f.up.posts()
	if segments != 1 || words != 2 || len(posts) != 1 || posts[0].path != "/v1/listen" || posts[0].header.Get("Content-Type") != "audio/wav" || posts[0].body != string(audio) {
		t.Fatalf("segments %d words %d posts %+v", segments, words, posts)
	}
}
