package inference

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// A managed engine reads whisper-server's verbose_json answer with the same
// decoder and bounds as the HTTP adapter.
func TestDecodeTranscript(t *testing.T) {
	answer := []byte(`{"text":"one two","language":"en","duration":1.5,"segments":[{"start":0,"end":0.7,"text":"one"},{"start":0.7,"end":1.5,"text":"two"}]}`)
	var got []wire.TranscriptSegment
	end := DecodeTranscript(answer, wire.TimestampModeSegment, func(s wire.TranscriptSegment) { got = append(got, s) })
	if end.Outcome != wire.ReplyOutcomeCompleted || end.DurationMs != 1500 || end.Language != "en" || len(got) != 2 || got[1].StartMs != 700 || got[1].Text != "two" {
		t.Fatalf("end %+v segments %+v", end, got)
	}
	got = nil
	end = DecodeTranscript(answer, wire.TimestampModeNone, func(s wire.TranscriptSegment) { got = append(got, s) })
	if len(got) != 1 || got[0].Text != "one two" || got[0].EndMs != 1500 {
		t.Fatalf("text-only %+v", got)
	}
	for _, bad := range []string{`not json`, `{"duration":-1}`, `{"segments":[{"start":2,"end":1,"text":"x"}]}`} {
		if end := DecodeTranscript([]byte(bad), wire.TimestampModeSegment, func(wire.TranscriptSegment) {}); end.Outcome != wire.ReplyOutcomeUnavailable {
			t.Fatalf("%s: %+v", bad, end)
		}
	}
}

type fakeTranscriber struct {
	unsupported string
	runs        atomic.Int64
	segments    []wire.TranscriptSegment
	end         wire.TranscriptionReply
	model       string
	audio       []byte
}

func (f *fakeTranscriber) Unsupported(req wire.TranscriptionRequest) string {
	if req.Credential != "" {
		return "credential:forwarded"
	}
	return f.unsupported
}

func (f *fakeTranscriber) Run(_ context.Context, model string, _ wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	f.runs.Add(1)
	f.model, f.audio = model, audio
	for _, s := range f.segments {
		emit(s)
	}
	return f.end
}

type fakeSpeaker struct {
	unsupported string
	runs        atomic.Int64
	reply       SpeechEngineReply
}

func (f *fakeSpeaker) Unsupported(wire.SpeechRequest) string { return f.unsupported }
func (f *fakeSpeaker) Run(context.Context, string, wire.SpeechRequest) SpeechEngineReply {
	f.runs.Add(1)
	return f.reply
}

type refuseHTTP struct{ t *testing.T }

func (r refuseHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("an engine host's request reached the HTTP adapter")
	return nil, errors.New("no HTTP")
}

type engineFixture struct {
	provider *Provider
	resolves atomic.Int64
	store    *speechStore
	audio    []byte
}

func newEngineFixture(t *testing.T, transcriber TranscriptionEngine, speaker SpeechEngine) *engineFixture {
	t.Helper()
	f := &engineFixture{store: &speechStore{}, audio: wavFixture()}
	host := router.OpenAICompatible("lemonade", "http://127.0.0.1:9", "/api/v1", []string{"Whisper-Large-v3-Turbo", "kokoro-v1"})
	host.Profiles = []string{router.ProfileTranscription, router.ProfileSpeech}
	r := router.New(host)
	r.Survey()
	pol := &policy{rules: map[string]string{caller.Program + " " + ActionComplete + " host:lemonade": "permitted"}}
	p, err := New(Config{Router: r, Decide: pol.decide, Apply: (&applier{}).apply, SurveyAge: time.Hour, Idle: 2 * time.Second,
		HTTP:                &http.Client{Transport: refuseHTTP{t}},
		TranscriptionEngine: transcriber, TranscriptionEngineHost: host, SpeechEngine: speaker, SpeechEngineHost: host,
		PrepareContentWrite: f.store.prepare,
		ResolveContent: func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
			f.resolves.Add(1)
			if digest != audioDigest(f.audio) {
				return nil, ContentUnknown
			}
			return f.audio, ContentResolved
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	f.provider = p
	return f
}

func localTranscription(audio []byte) wire.TranscriptionRequest {
	return wire.TranscriptionRequest{Model: "Whisper-Large-v3-Turbo", AudioDigest: audioDigest(audio), MediaType: "audio/wav",
		Timestamps: wire.TimestampModeNone, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
}

func localSpeech(format wire.SpeechFormat) wire.SpeechRequest {
	return wire.SpeechRequest{Model: "kokoro-v1", Voice: "af_sky", Text: "hello", Format: format, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
}

// An engine's refusal of a request reads unsupported_feature with its reason
// before any content is resolved or the engine runs.
func TestTranscriptionEngineRefusesBeforeContent(t *testing.T) {
	engine := &fakeTranscriber{unsupported: "format:audio/mpeg"}
	f := newEngineFixture(t, engine, &fakeSpeaker{})
	a := f.provider.StartTranscription(context.Background(), caller, localTranscription(f.audio))
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "format:audio/mpeg" || f.resolves.Load() != 0 || engine.runs.Load() != 0 {
		t.Fatalf("admission %+v, resolves %d, runs %d", a, f.resolves.Load(), engine.runs.Load())
	}
}

// The engine receives the selected model and the verified audio; its
// segments reach the caller and the HTTP adapter is never used.
func TestTranscriptionEngineRunsInPlaceOfHTTP(t *testing.T) {
	engine := &fakeTranscriber{segments: []wire.TranscriptSegment{{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 0, EndMs: 900, Text: "hello"}},
		end: wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted, Language: "en", DurationMs: 900}}
	f := newEngineFixture(t, engine, &fakeSpeaker{})
	a := f.provider.StartTranscription(context.Background(), caller, localTranscription(f.audio))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeCompleted || len(deltas) != 2 || deltas[0].Segment == nil || deltas[0].Segment.Text != "hello" {
		t.Fatalf("end %+v deltas %+v", end, deltas)
	}
	if engine.model != "Whisper-Large-v3-Turbo" || !bytes.Equal(engine.audio, f.audio) {
		t.Fatalf("engine saw %q and %d bytes", engine.model, len(engine.audio))
	}
}

// A segment outside INF-M6's bounds ends the request unavailable and is
// never relayed.
func TestTranscriptionEngineInvalidSegment(t *testing.T) {
	engine := &fakeTranscriber{segments: []wire.TranscriptSegment{{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 900, EndMs: 100, Text: "backwards"}},
		end: wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted}}
	f := newEngineFixture(t, engine, &fakeSpeaker{})
	a := f.provider.StartTranscription(context.Background(), caller, localTranscription(f.audio))
	deltas, end := drainTranscription(t, f.provider, caller, a.Operation, 0)
	if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "engine:invalid_reply" || len(deltas) != 1 {
		t.Fatalf("end %+v deltas %+v", end, deltas)
	}
}

// A speech engine's refusal comes before the output write is prepared.
func TestSpeechEngineRefusesBeforeOutputAuthorization(t *testing.T) {
	engine := &fakeSpeaker{unsupported: "format:mp3"}
	f := newEngineFixture(t, &fakeTranscriber{}, engine)
	a := f.provider.StartSpeech(context.Background(), caller, localSpeech(wire.SpeechFormatMp3))
	f.store.mu.Lock()
	prepared := f.store.subject
	f.store.mu.Unlock()
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || a.Reason != "format:mp3" || engine.runs.Load() != 0 || prepared != (Subject{}) {
		t.Fatalf("admission %+v, runs %d, prepared %+v", a, engine.runs.Load(), prepared)
	}
}

// The engine's complete audio is streamed as bounded audio deltas and
// published under the caller's output authorization.
func TestSpeechEngineDeliversCheckedAudio(t *testing.T) {
	audio := append([]byte("RIFF\x00\x00\x00\x00WAVE"), bytes.Repeat([]byte{7}, 100000)...)
	f := newEngineFixture(t, &fakeTranscriber{}, &fakeSpeaker{reply: SpeechEngineReply{Outcome: wire.ReplyOutcomeCompleted, Audio: audio}})
	a := f.provider.StartSpeech(context.Background(), caller, localSpeech(wire.SpeechFormatWav))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	got, deltas, end := drainSpeech(t, f.provider, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || !bytes.Equal(got, audio) || end.Delivery.Digest != audioDigest(audio) || end.Delivery.MediaType != "audio/wav" || len(deltas) != 4 {
		t.Fatalf("end %+v, %d bytes, %d deltas", end, len(got), len(deltas))
	}
	if !bytes.Equal(f.store.data, audio) || f.store.subject != caller {
		t.Fatal("audio not published for the caller")
	}
}

// Audio that is not the requested format, or a failed reply that carries
// audio, ends the request unavailable and publishes nothing.
func TestSpeechEngineInvalidReplies(t *testing.T) {
	for name, reply := range map[string]SpeechEngineReply{
		"mp3 as wav":       {Outcome: wire.ReplyOutcomeCompleted, Audio: []byte("ID3\x04 not a wav")},
		"empty":            {Outcome: wire.ReplyOutcomeCompleted},
		"failed with data": {Outcome: wire.ReplyOutcomeUnavailable, Audio: []byte("RIFF\x00\x00\x00\x00WAVE")},
	} {
		f := newEngineFixture(t, &fakeTranscriber{}, &fakeSpeaker{reply: reply})
		a := f.provider.StartSpeech(context.Background(), caller, localSpeech(wire.SpeechFormatWav))
		_, _, end := drainSpeech(t, f.provider, caller, a.Operation)
		if end.Outcome != wire.ReplyOutcomeUnavailable || f.store.data != nil {
			t.Fatalf("%s: end %+v", name, end)
		}
	}
	f := newEngineFixture(t, &fakeTranscriber{}, &fakeSpeaker{reply: SpeechEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "engine:crashed"}})
	a := f.provider.StartSpeech(context.Background(), caller, localSpeech(wire.SpeechFormatWav))
	if _, _, end := drainSpeech(t, f.provider, caller, a.Operation); end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "engine:crashed" {
		t.Fatalf("engine failure %+v", end)
	}
}
