package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type fakeTranscriber struct {
	request wire.TranscriptionRequest
	starts  int
}

func (*fakeTranscriber) Start(context.Context, inference.Subject, wire.Request) wire.Admission {
	return wire.Admission{Outcome: wire.StartOutcomeUnavailable}
}
func (*fakeTranscriber) Observe(context.Context, inference.Subject, string, int64, int64, int64, int64) wire.DeltaPage {
	return wire.DeltaPage{Outcome: wire.PageOutcomeUnknown}
}
func (*fakeTranscriber) Cancel(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeUnknown}
}
func (f *fakeTranscriber) StartTranscription(_ context.Context, _ inference.Subject, req wire.TranscriptionRequest) wire.Admission {
	f.request, f.starts = req, f.starts+1
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "op"}
}
func (*fakeTranscriber) ObserveTranscription(_ context.Context, _ inference.Subject, _ string, cursor, _, _, _ int64) wire.DeltaPage {
	if cursor == 0 {
		return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 0, Kind: wire.DeltaKindSegment,
			Segment: &wire.TranscriptSegment{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 0, EndMs: 30000, Text: "hello"}}}, Next: 1}
	}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{{Sequence: 1, Kind: wire.DeltaKindEnd,
		TranscriptionEnd: &wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted, Language: "en", DurationMs: 30000}}}, Next: 2, AtEnd: true}
}
func (*fakeTranscriber) CancelTranscription(inference.Subject, string) wire.Cancellation {
	return wire.Cancellation{Outcome: wire.CancelOutcomeCancelled}
}

type fixedBound struct{ subject inference.Subject }

func (b fixedBound) Subject() inference.Subject { return b.subject }
func (fixedBound) Rung() string                 { return "fixture/bound" }
func (fixedBound) Recheck() error               { return nil }
func (fixedBound) Close() error                 { return nil }

func multipartAudio(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	audio := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 64)...)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "recording.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(audio)
	_ = mw.WriteField("model", "whisper-1")
	_ = mw.WriteField("response_format", "verbose_json")
	_ = mw.WriteField("timestamp_granularities[]", "segment")
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r, audio
}

func TestGatewayTranscriptionStoresForOriginalCallerAndReturnsSegments(t *testing.T) {
	provider := &fakeTranscriber{}
	subject := inference.Subject{Account: "account", Program: "/apps/recorder"}
	stored := false
	w := &Window{cfg: Config{Chat: provider, StoreContent: func(_ context.Context, got inference.Subject, _ string, data []byte) inference.ContentOutcome {
		if got != subject || audioType(data) != "audio/wav" {
			t.Fatalf("stored for %+v", got)
		}
		stored = true
		return inference.ContentResolved
	}}}
	r, _ := multipartAudio(t)
	rw := httptest.NewRecorder()
	w.openAITranscription(rw, r, fixedBound{subject}, Grant{})
	if rw.Code != http.StatusOK || !stored || provider.starts != 1 || provider.request.MediaType != "audio/wav" || provider.request.AudioDigest == "" {
		t.Fatalf("code %d stored %v starts %d request %+v body %s", rw.Code, stored, provider.starts, provider.request, rw.Body.String())
	}
	var out struct {
		Text     string           `json:"text"`
		Segments []map[string]any `json:"segments"`
		Duration float64          `json:"duration"`
	}
	if json.Unmarshal(rw.Body.Bytes(), &out) != nil || out.Text != "hello" || len(out.Segments) != 1 || out.Duration != 30 {
		t.Fatalf("response %s", rw.Body.String())
	}
}

func TestGatewayTranscriptionWriteDenialStartsNoOperation(t *testing.T) {
	provider := &fakeTranscriber{}
	w := &Window{cfg: Config{Chat: provider, StoreContent: func(context.Context, inference.Subject, string, []byte) inference.ContentOutcome {
		return inference.ContentForbidden
	}}}
	r, _ := multipartAudio(t)
	rw := httptest.NewRecorder()
	w.openAITranscription(rw, r, fixedBound{inference.Subject{Account: "account", Program: "/apps/recorder"}}, Grant{})
	if rw.Code != http.StatusForbidden || provider.starts != 0 {
		t.Fatalf("code %d starts %d body %s", rw.Code, provider.starts, rw.Body.String())
	}
}
