package providerhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type leafTranscriber struct {
	mu    sync.Mutex
	audio []byte
}

func (*leafTranscriber) Unsupported(req wire.TranscriptionRequest) string {
	if req.MediaType != "audio/wav" {
		return "format:" + req.MediaType
	}
	return ""
}

func (l *leafTranscriber) Run(_ context.Context, _ string, _ wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	l.mu.Lock()
	l.audio = audio
	l.mu.Unlock()
	emit(wire.TranscriptSegment{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 0, EndMs: 500, Text: "harbour lighthouse"})
	return wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted, Language: "en", DurationMs: 500}
}

var leafWAV = append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{3, 0}, 60000)...)

type leafSpeaker struct{}

func (leafSpeaker) Unsupported(req wire.SpeechRequest) string {
	if req.Format != wire.SpeechFormatWav {
		return "format:" + req.Format.String()
	}
	return ""
}
func (leafSpeaker) Run(context.Context, string, wire.SpeechRequest) inference.SpeechEngineReply {
	return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeCompleted, Audio: leafWAV}
}

func engineHost(profiles ...string) *router.Host {
	h := router.OpenAICompatible("managed", "http://127.0.0.1:9", "/api/v1", []string{"Whisper-Large-v3-Turbo", "kokoro-v1"})
	h.Profiles = profiles
	return h
}

func testEndpoint(t *testing.T, name string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\providerhost-%s-%d-%d`, name, os.Getpid(), time.Now().UnixNano())
	}
	return filepath.Join(t.TempDir(), name+".sock")
}

// A managed engine's host declares exactly its profiles, and each audio
// profile comes with its explicit engine.
func TestNewEngineChecksProfiles(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := EngineConfig{Endpoint: testEndpoint(t, "profiles"), RuntimePrograms: []string{exe}, StoreRoot: t.TempDir(), RecordPath: filepath.Join(t.TempDir(), "uploads.json")}
	for name, tune := range map[string]func(*EngineConfig){
		"no profiles": func(c *EngineConfig) { c.Host = engineHost(router.ProfileTranscription) },
		"host declares more": func(c *EngineConfig) {
			c.Host, c.Profiles, c.TranscriptionEngine = engineHost(router.ProfileTranscription, router.ProfileChat), []string{router.ProfileTranscription}, &leafTranscriber{}
		},
		"chat profile": func(c *EngineConfig) {
			c.Host, c.Profiles = engineHost(router.ProfileChat), []string{router.ProfileChat}
		},
		"repeated profile": func(c *EngineConfig) {
			c.Host, c.Profiles, c.SpeechEngine = engineHost(router.ProfileSpeech), []string{router.ProfileSpeech, router.ProfileSpeech}, leafSpeaker{}
		},
		"speech, no engine": func(c *EngineConfig) {
			c.Host, c.Profiles = engineHost(router.ProfileSpeech), []string{router.ProfileSpeech}
		},
		"engine, no profile": func(c *EngineConfig) {
			c.Host, c.Profiles, c.SpeechEngine, c.TranscriptionEngine = engineHost(router.ProfileSpeech), []string{router.ProfileSpeech}, leafSpeaker{}, &leafTranscriber{}
		},
		"image without HTTP": func(c *EngineConfig) {
			c.Host, c.Profiles = engineHost(router.ProfileImage), []string{router.ProfileImage}
		},
	} {
		cfg := base
		tune(&cfg)
		if h, err := NewEngine(cfg); err == nil {
			h.Close()
			t.Errorf("%s accepted", name)
		}
	}
}

// The designated runtime reaches a managed engine's transcription@1 and
// speech@1 over the native transport: the caller's audio is staged at the
// provider by digest, segments come back in order, and speech output is
// read back, checked and published under the runtime's own authorization.
func TestNativeRelayReachesManagedEngineProfiles(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := testEndpoint(t, "engine")
	transcriber := &leafTranscriber{}
	leaf, err := NewEngine(EngineConfig{Endpoint: endpoint, RuntimePrograms: []string{exe}, Host: engineHost(router.ProfileTranscription, router.ProfileSpeech),
		Profiles: []string{router.ProfileTranscription, router.ProfileSpeech}, TranscriptionEngine: transcriber, SpeechEngine: leafSpeaker{},
		StoreRoot: t.TempDir(), RecordPath: filepath.Join(t.TempDir(), "uploads.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- leaf.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("managed", endpoint, listen.ServerExpectation{Principal: principal, Program: identity.CanonicalProgramPath(exe)},
		[]string{"transcription:Whisper-Large-v3-Turbo", "speech:kokoro-v1"}, []string{router.ProfileTranscription, router.ProfileSpeech})
	if err != nil {
		t.Fatal(err)
	}
	routes := router.New(native)
	routes.Survey()
	caller := inference.Subject{Account: "S-1-5-21-fixture", Program: `C:\apps\opencode.exe`}
	audio := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{5, 0}, 40000)...)
	sum := sha256.Sum256(audio)
	audioDigest := "sha256:" + hex.EncodeToString(sum[:])
	var published []byte
	var mu sync.Mutex
	runtimeProvider, err := inference.New(inference.Config{Router: routes, SurveyAge: time.Hour,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ResolveContent: func(_ context.Context, _ inference.Subject, digest string, _ int64) ([]byte, inference.ContentOutcome) {
			if digest != audioDigest {
				return nil, inference.ContentUnknown
			}
			return audio, inference.ContentResolved
		},
		PrepareContentWrite: func(_ context.Context, _ inference.Subject, profile, media string, _ int64) (inference.ContentCommitter, inference.ContentOutcome) {
			if profile != "speech" || media != "audio/wav" && media != "audio/mpeg" {
				return nil, inference.ContentForbidden
			}
			return func(_ context.Context, _ string, data []byte) inference.ContentOutcome {
				mu.Lock()
				published = append([]byte(nil), data...)
				mu.Unlock()
				return inference.ContentResolved
			}, inference.ContentResolved
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeProvider.Close()

	local := []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	// The runtime's router answers the native host's audio addresses and
	// serves each model only for the profile its declaration names
	// (ROUTE-D9): speech naming the transcription model reads no_host.
	if crossed := runtimeProvider.StartSpeech(context.Background(), caller, wire.SpeechRequest{Model: "Whisper-Large-v3-Turbo", Voice: "af_sky", Text: "Done.", Format: wire.SpeechFormatWav, Guarantees: local}); crossed.Outcome != wire.StartOutcomeNoHost || crossed.Reason != "router:"+router.NoHost {
		t.Fatalf("speech naming the transcription model: %+v", crossed)
	}
	a := runtimeProvider.StartTranscription(context.Background(), caller, wire.TranscriptionRequest{Model: "Whisper-Large-v3-Turbo", AudioDigest: audioDigest, MediaType: "audio/wav", Timestamps: wire.TimestampModeNone, Guarantees: local})
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("transcription admission %+v", a)
	}
	var text string
	var tEnd *wire.TranscriptionReply
	for cursor, deadline := int64(0), time.Now().Add(20*time.Second); tEnd == nil; {
		if time.Now().After(deadline) {
			t.Fatal("transcription did not end")
		}
		page := runtimeProvider.ObserveTranscription(context.Background(), caller, a.Operation, cursor, 256, 65536, 1000)
		for _, d := range page.Deltas {
			if d.Segment != nil {
				text += d.Segment.Text
			}
			if d.TranscriptionEnd != nil {
				tEnd = d.TranscriptionEnd
			}
		}
		cursor = page.Next
	}
	transcriber.mu.Lock()
	staged := transcriber.audio
	transcriber.mu.Unlock()
	if tEnd.Outcome != wire.ReplyOutcomeCompleted || text != "harbour lighthouse" || !bytes.Equal(staged, audio) {
		t.Fatalf("transcription end %+v, text %q, staged %d bytes", tEnd, text, len(staged))
	}

	s := runtimeProvider.StartSpeech(context.Background(), caller, wire.SpeechRequest{Model: "kokoro-v1", Voice: "af_sky", Text: "Done.", Format: wire.SpeechFormatWav, Guarantees: local})
	if s.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("speech admission %+v", s)
	}
	var streamed []byte
	var sEnd *wire.SpeechReply
	for cursor, deadline := int64(0), time.Now().Add(20*time.Second); sEnd == nil; {
		if time.Now().After(deadline) {
			t.Fatal("speech did not end")
		}
		page := runtimeProvider.ObserveSpeech(context.Background(), caller, s.Operation, cursor, 256, 65536, 1000)
		for _, d := range page.Deltas {
			if d.Audio != nil {
				streamed = append(streamed, d.Audio.Data...)
			}
			if d.SpeechEnd != nil {
				sEnd = d.SpeechEnd
			}
		}
		cursor = page.Next
	}
	mu.Lock()
	defer mu.Unlock()
	if sEnd.Outcome != wire.ReplyOutcomeCompleted || !bytes.Equal(streamed, leafWAV) || !bytes.Equal(published, leafWAV) || sEnd.Delivery.Location != "local" {
		t.Fatalf("speech end %+v, streamed %d, published %d", sEnd, len(streamed), len(published))
	}

	// The leaf decides its formats: mp3 reads unsupported_feature through
	// the relay.
	m := runtimeProvider.StartSpeech(context.Background(), caller, wire.SpeechRequest{Model: "kokoro-v1", Voice: "af_sky", Text: "Done.", Format: wire.SpeechFormatMp3, Guarantees: local})
	if m.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("mp3 admission %+v", m)
	}
	for cursor, deadline := int64(0), time.Now().Add(20*time.Second); ; {
		if time.Now().After(deadline) {
			t.Fatal("mp3 speech did not end")
		}
		page := runtimeProvider.ObserveSpeech(context.Background(), caller, m.Operation, cursor, 256, 65536, 1000)
		for _, d := range page.Deltas {
			if d.SpeechEnd != nil {
				if d.SpeechEnd.Outcome != wire.ReplyOutcomeUnsupportedFeature || d.SpeechEnd.Reason != "native:format:mp3" {
					t.Fatalf("mp3 end %+v", d.SpeechEnd)
				}
				return
			}
		}
		cursor = page.Next
	}
}
