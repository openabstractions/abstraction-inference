package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type fakeSpeech struct{ request wire.SpeechRequest }

func (*fakeSpeech) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{}
}
func (*fakeSpeech) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{}
}
func (*fakeSpeech) Cancel(inference.Subject, string) wire.Cancellation { return wire.Cancellation{} }
func (f *fakeSpeech) StartSpeech(_ context.Context, _ inference.Subject, request wire.SpeechRequest) wire.Admission {
	f.request = request
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "op"}
}
func (*fakeSpeech) ObserveSpeech(_ context.Context, _ inference.Subject, _ string, cursor, _, _, _ int64) wire.DeltaPage {
	if cursor == 0 {
		return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 0, Kind: wire.DeltaKindAudio, Audio: &wire.AudioChunk{Data: []byte("abc")}}}, Next: 1}
	}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 1, Kind: wire.DeltaKindEnd, SpeechEnd: &wire.SpeechReply{Outcome: wire.ReplyOutcomeCompleted}}}, Next: 2, AtEnd: true}
}
func (*fakeSpeech) CancelSpeech(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

func TestGatewaySpeechStreamsProviderBytes(t *testing.T) {
	provider := &fakeSpeech{}
	w := &Window{cfg: Config{Chat: provider}}
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-1","voice":"alloy","input":"hello","response_format":"wav"}`))
	rw := httptest.NewRecorder()
	w.openAISpeech(rw, r, fixedBound{inference.Subject{Account: "account", Program: "/apps/reader"}}, Grant{Credential: "openai"})
	if rw.Code != http.StatusOK || rw.Body.String() != "abc" || rw.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("code=%d type=%q body=%q", rw.Code, rw.Header().Get("Content-Type"), rw.Body.String())
	}
	if provider.request.Model != "tts-1" || provider.request.Voice != "alloy" || provider.request.Text != "hello" || provider.request.Format != wire.SpeechFormatWav || provider.request.Credential != "openai" {
		t.Fatalf("request %+v", provider.request)
	}
}
