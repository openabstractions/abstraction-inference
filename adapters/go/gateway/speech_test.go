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

// refusingSpeech is a speech provider whose server cannot honour any option.
type refusingSpeech struct{ fakeSpeech }

func (f *refusingSpeech) StartSpeech(_ context.Context, _ inference.Subject, request wire.SpeechRequest) wire.Admission {
	f.request = request
	for _, key := range []string{inference.SpeechSpeedExtension, inference.SpeechInstructionsExtension} {
		if _, named := request.Extensions[key]; named {
			return wire.Admission{Outcome: wire.StartOutcomeUnsupportedFeature, Reason: "extension:" + key}
		}
	}
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "op"}
}

// speed and instructions travel as speech@1 extensions. A speed out of
// range is the client's invalid request, and a server that cannot honour an
// option is a plain 400 naming it and one log line without its value.
func TestGatewaySpeechCarriesSpeedAndInstructions(t *testing.T) {
	post := func(provider Chat, body string) *httptest.ResponseRecorder {
		w := &Window{cfg: Config{Chat: provider}}
		r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(body))
		rw := httptest.NewRecorder()
		w.openAISpeech(rw, r, fixedBound{inference.Subject{Account: "account", Program: "/apps/reader"}}, Grant{})
		return rw
	}
	provider := &fakeSpeech{}
	rw := post(provider, `{"model":"kokoro","voice":"af_sky","input":"hello","response_format":"wav","speed":1.25,"instructions":"Speak calmly."}`)
	if rw.Code != http.StatusOK || provider.request.Extensions[inference.SpeechSpeedExtension] != "1.25" ||
		provider.request.Extensions[inference.SpeechInstructionsExtension] != "Speak calmly." {
		t.Fatalf("code=%d request %+v", rw.Code, provider.request)
	}
	provider = &fakeSpeech{}
	if rw = post(provider, `{"model":"kokoro","voice":"af_sky","input":"hello","speed":1}`); rw.Code != http.StatusOK || provider.request.Extensions != nil {
		t.Fatalf("default speed: code=%d extensions %v", rw.Code, provider.request.Extensions)
	}
	if rw = post(&fakeSpeech{}, `{"model":"kokoro","voice":"af_sky","input":"hello","speed":5}`); rw.Code != http.StatusBadRequest {
		t.Fatalf("speed 5: code=%d body %s", rw.Code, rw.Body)
	}
	logged := captureLog(t)
	rw = post(&refusingSpeech{}, `{"model":"piper","voice":"lessac","input":"hello","response_format":"wav","instructions":"Secret tone."}`)
	if rw.Code != http.StatusBadRequest || !strings.Contains(rw.Body.String(), `instructions is unsupported by the speech server for model \"piper\"`) {
		t.Fatalf("refused option: code=%d body %s", rw.Code, rw.Body)
	}
	if line := logged.String(); !strings.Contains(line, "extension:openai/instructions") || strings.Contains(line, "Secret tone") {
		t.Fatalf("log %q", line)
	}
}
