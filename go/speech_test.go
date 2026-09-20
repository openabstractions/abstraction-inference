package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type speechStore struct {
	mu             sync.Mutex
	subject        Subject
	digest         string
	data           []byte
	prepareOutcome ContentOutcome
	commitOutcome  ContentOutcome
	commitStarted  chan struct{}
	commitRelease  chan struct{}
}

func (s *speechStore) prepare(_ context.Context, subject Subject, profile, media string, max int64) (ContentCommitter, ContentOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subject = subject
	if profile != "speech" || media == "" || max < 1 {
		return nil, ContentUnavailable
	}
	if s.prepareOutcome != "" && s.prepareOutcome != ContentResolved {
		return nil, s.prepareOutcome
	}
	return func(_ context.Context, digest string, data []byte) ContentOutcome {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.commitStarted != nil {
			close(s.commitStarted)
			s.commitStarted = nil
		}
		if s.commitRelease != nil {
			<-s.commitRelease
		}
		if s.commitOutcome != "" && s.commitOutcome != ContentResolved {
			return s.commitOutcome
		}
		s.digest, s.data = digest, append([]byte(nil), data...)
		return ContentResolved
	}, ContentResolved
}

func TestSpeechCancelWaitsForStartedOutputCommit(t *testing.T) {
	started := make(chan struct{})
	store := &speechStore{commitStarted: started, commitRelease: make(chan struct{})}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("commit did not start")
	}
	cancelled := make(chan wire.Cancellation, 1)
	go func() { cancelled <- f.provider.CancelSpeech(caller, a.Operation) }()
	select {
	case result := <-cancelled:
		t.Fatalf("cancel returned during commit: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.commitRelease)
	select {
	case result := <-cancelled:
		if result.Outcome != wire.CancelOutcomeEnded || result.ReplyOutcome == nil || *result.ReplyOutcome != wire.ReplyOutcomeCompleted {
			t.Fatalf("cancel result %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not return after commit")
	}
	_, _, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Delivery.Digest == "" {
		t.Fatalf("end %+v", end)
	}
}

func speechRequest(text string) wire.SpeechRequest {
	return wire.SpeechRequest{Model: "anthropic/claude-sonnet-5", Voice: "alloy", Text: text, Format: wire.SpeechFormatWav,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "openrouter"}
}

func drainSpeech(t *testing.T, p *Provider, subject Subject, id string) ([]byte, []wire.Delta, wire.SpeechReply) {
	t.Helper()
	var audio []byte
	var deltas []wire.Delta
	for cursor, deadline := int64(0), time.Now().Add(5*time.Second); time.Now().Before(deadline); {
		page := p.ObserveSpeech(context.Background(), subject, id, cursor, 256, 65536, 500)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Sequence != cursor {
				t.Fatalf("sequence %d cursor %d", d.Sequence, cursor)
			}
			cursor++
			deltas = append(deltas, d)
			if d.Audio != nil {
				audio = append(audio, d.Audio.Data...)
			}
			if d.SpeechEnd != nil {
				return audio, deltas, *d.SpeechEnd
			}
		}
	}
	t.Fatal("speech did not end")
	return nil, nil, wire.SpeechReply{}
}

func TestSpeechChunksConcatenateToCommittedDigest(t *testing.T) {
	store := &speechStore{}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	f.up.speech = append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0x5a}, 100000)...)
	text := "Hello, 世界"
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest(text))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if got := f.provider.Observe(context.Background(), caller, a.Operation, 0, 1, 65536, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("cross-profile observe %+v", got)
	}
	if got := f.provider.Cancel(caller, a.Operation); got.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("cross-profile cancel %+v", got)
	}
	audio, deltas, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Characters != 9 || end.Delivery.Location != "local" || end.Delivery.Size != int64(len(audio)) {
		t.Fatalf("end %+v", end)
	}
	for _, d := range deltas {
		if d.Audio != nil && (len(d.Audio.Data) == 0 || len(d.Audio.Data) > 65536) {
			t.Fatalf("chunk %d bytes", len(d.Audio.Data))
		}
	}
	sum := sha256.Sum256(audio)
	wantDigest := fmt.Sprintf("sha256:%x", sum)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.subject != caller || !bytes.Equal(store.data, audio) || store.digest != wantDigest || end.Delivery.Digest != wantDigest {
		t.Fatalf("store subject=%+v digest=%q bytes=%d delivery=%+v", store.subject, store.digest, len(store.data), end.Delivery)
	}
	if posts := f.up.posts(); len(posts) != 1 || posts[0].path != "/api/v1/audio/speech" || !bytes.Contains([]byte(posts[0].body), []byte(`"voice":"alloy"`)) {
		t.Fatalf("posts %+v", posts)
	}
	records := f.waitLogged(t, 1)
	if len(records) != 1 || records[0].Profile != router.ProfileSpeech || records[0].Characters != 9 {
		t.Fatalf("records %+v", records)
	}
}

func TestSpeechCannotObserveOrCancelChatOperation(t *testing.T) {
	store := &speechStore{}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	if got := f.provider.ObserveSpeech(context.Background(), caller, a.Operation, 0, 1, 65536, 0); got.Outcome != wire.PageOutcomeUnknown {
		t.Fatalf("cross-profile observe %+v", got)
	}
	if got := f.provider.CancelSpeech(caller, a.Operation); got.Outcome != wire.CancelOutcomeUnknown {
		t.Fatalf("cross-profile cancel %+v", got)
	}
	_, end, _ := drain(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("chat end %+v", end)
	}
}

func TestSpeechOutputAuthorizationPrecedesCredentialAndUpstream(t *testing.T) {
	store := &speechStore{prepareOutcome: ContentForbidden}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	before := f.applier.count()
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeForbidden || a.Reason != "output:forbidden" {
		t.Fatalf("admission %+v", a)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != before {
		t.Fatalf("spent: posts=%d apply=%d/%d", len(f.up.posts()), f.applier.count(), before)
	}
}

func TestSpeechCapacityRefusalPrecedesCredentialApplication(t *testing.T) {
	store := &speechStore{}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.PrepareContentWrite = store.prepare
		c.MaxOperations = 1
	})
	f.up.hold = true
	chat := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if chat.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("chat admission %+v", chat)
	}
	before := f.applier.count()
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeExhausted || a.Reason != "capacity" {
		t.Fatalf("speech admission %+v", a)
	}
	if f.applier.count() != before {
		t.Fatalf("capacity applied credential: %d/%d", f.applier.count(), before)
	}
	_ = f.provider.Cancel(caller, chat.Operation)
}

func TestSpeechRevokedOutputGrantFailsTerminalCommit(t *testing.T) {
	store := &speechStore{commitOutcome: ContentForbidden}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, _, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeForbidden || end.Reason != "output:forbidden" {
		t.Fatalf("end %+v", end)
	}
}

func TestSpeechUnknownVoiceIsUnsupported(t *testing.T) {
	store := &speechStore{}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
	f.up.speechStatus = http.StatusNotFound
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, _, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeUnsupportedFeature || end.Reason != "voice:alloy" {
		t.Fatalf("end %+v", end)
	}
}

func TestSpeechCountsCharactersAndEnforcesCeiling(t *testing.T) {
	ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"openrouter": {CharactersPerDay: 2}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &speechStore{}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite, c.Ceilings = store.prepare, ceilings })
	a := f.provider.StartSpeech(context.Background(), caller, speechRequest("世界"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("first %+v", a)
	}
	_, _, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted {
		t.Fatalf("end %+v", end)
	}
	a = f.provider.StartSpeech(context.Background(), caller, speechRequest("a"))
	if a.Outcome != wire.StartOutcomeBudgetExceeded || a.Reason != "ceiling:characters:openrouter" {
		t.Fatalf("second %+v", a)
	}
}

func TestPiperRefusesUnsupportedFormatBeforeOutputPreparation(t *testing.T) {
	r := router.New(router.Piper("http://127.0.0.1:1"))
	r.Survey()
	prepared := 0
	p, err := New(Config{Router: r, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		PrepareContentWrite: func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
			prepared++
			return nil, ContentUnavailable
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	req := wire.SpeechRequest{Model: "piper", Voice: "lessac", Text: "hello", Format: wire.SpeechFormatMp3}
	a := p.StartSpeech(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "format:mp3" || prepared != 0 {
		t.Fatalf("admission %+v prepared %d", a, prepared)
	}
}

func TestElevenLabsUsesVoicePathModelAndOutputFormat(t *testing.T) {
	store := &speechStore{}
	f := setup(t, router.WireElevenLabsStream, func(c *Config) { c.PrepareContentWrite = store.prepare })
	f.policy.rules[caller.Program+" "+ActionComplete+" host:elevenlabs"] = "permitted"
	req := wire.SpeechRequest{Model: "eleven_multilingual_v2", Voice: "voice-123", Text: "hello", Format: wire.SpeechFormatMp3,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "elevenlabs"}
	a := f.provider.StartSpeech(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	audio, _, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || string(audio) != "ID3elevenlabs-audio" || end.Delivery.MediaType != "audio/mpeg" {
		t.Fatalf("audio %q end %+v", audio, end)
	}
	posts := f.up.posts()
	if len(posts) != 1 || posts[0].path != "/v1/text-to-speech/voice-123/stream" || posts[0].header.Get("xi-api-key") != secret || !bytes.Contains([]byte(posts[0].body), []byte(`"model_id":"eleven_multilingual_v2"`)) {
		t.Fatalf("posts %+v", posts)
	}
}
