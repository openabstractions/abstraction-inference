package inference

import (
	"errors"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

type chat3NoEffectTransport struct{ calls int }

func (f *chat3NoEffectTransport) ExchangeFrame([]byte) ([]byte, error) {
	f.calls++
	return nil, errors.New("unexpected transport effect")
}

func chat3RequestFixture() wire.RequestV3 {
	return wire.RequestV3{
		Identity: acceptance.RequestIdentity{Key: "key", HistoryEpoch: "epoch"},
		Target:   wire.ChatTarget{Model: "model"},
		Content: wire.ChatContent{Messages: []wire.ChatMessage{{Role: wire.RoleUser,
			Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "prompt"}}}},
			Guarantees: []wire.ChatGuarantee{}},
	}
}

func chat3RefusalWord(err error) string {
	var refusal *wire.Refusal
	if errors.As(err, &refusal) {
		return refusal.Word
	}
	return ""
}

func TestChat3RequestOverCapRefusesBeforeTransport(t *testing.T) {
	for _, tc := range []struct {
		size int
		word string
	}{
		{chat3FrameBytes + 1, ""},
		{chat3FrameBytes, "encoded_size"},
	} {
		request := chat3RequestFixture()
		request.Content.Messages[0].Parts[0].Text = strings.Repeat("x", tc.size)
		transport := &chat3NoEffectTransport{}
		_, err := wire.NewChatV3Client(transport).Start(request)
		word := chat3RefusalWord(err)
		if word == "" || tc.word != "" && word != tc.word || transport.calls != 0 {
			t.Fatalf("size %d: word %q transport calls %d", tc.size, chat3RefusalWord(err), transport.calls)
		}
	}
}

func TestChat3OverFramePageReturnsNoPartialEncoding(t *testing.T) {
	page := wire.ChatPage{Outcome: wire.PageOutcomePage, Next: 2}
	for sequence := int64(0); sequence < 2; sequence++ {
		page.Events = append(page.Events, wire.ChatEvent{Sequence: sequence, Kind: wire.ChatEventKindPart,
			Part: &wire.ChatPart{Block: sequence, Kind: wire.ChatPartKindText, Text: strings.Repeat("x", 600000)}})
	}
	encoded, err := wire.ToJSON(page)
	if chat3RefusalWord(err) != "encoded_size" || len(encoded) != 0 {
		t.Fatalf("word %q exposed %d partial bytes", chat3RefusalWord(err), len(encoded))
	}
}

func TestChat3ActualUsageKeepsAbsentZeroAndNullDistinct(t *testing.T) {
	var absent, zero wire.ChatUsage
	if err := wire.FromJSON([]byte(`{}`), &absent); err != nil {
		t.Fatal(err)
	}
	if err := wire.FromJSON([]byte(`{"output":0}`), &zero); err != nil {
		t.Fatal(err)
	}
	if absent.Output != nil || zero.Output == nil || *zero.Output != 0 {
		t.Fatal("absent and reported zero collapsed")
	}
	if err := wire.FromJSON([]byte(`{"output":null}`), &zero); chat3RefusalWord(err) != "wrong_type" {
		t.Fatalf("null: %v", err)
	}
}

func TestChat3CapacityHintWireAbsenceAndPositiveRange(t *testing.T) {
	for _, value := range []int64{0, 1, 86400000, 86400001} {
		admission := wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnavailable, RetryAfterMs: &value}
		terminal := wire.ChatResult{Outcome: wire.ChatResultOutcomeFailed, RetryAfterMs: &value}
		for _, record := range []any{admission, terminal} {
			_, err := wire.ToJSON(record)
			if (value < 1 || value > 86400000) != (err != nil) {
				t.Fatalf("hint %d: %v", value, err)
			}
		}
	}
	var absent wire.ChatResult
	if err := wire.FromJSON([]byte(`{"outcome":"failed","cause":"unavailable","usage":{}}`), &absent); err != nil || absent.RetryAfterMs != nil {
		t.Fatalf("absent hint became a value: %v", err)
	}
}

func TestChat3TimingWirePreservesAbsenceZeroAndOpenDisplayWords(t *testing.T) {
	var absent wire.ChatResult
	if err := wire.FromJSON([]byte(`{"outcome":"failed","cause":"unavailable","usage":{}}`), &absent); err != nil || absent.Timing != nil {
		t.Fatal("absent timing became present")
	}
	var timing wire.ChatTiming
	if err := wire.FromJSON([]byte(`{"queue_ms":0,"load":"future_load","load_source":"future_source"}`), &timing); err != nil {
		t.Fatal(err)
	}
	if timing.QueueMs == nil || *timing.QueueMs != 0 || timing.RestoreMs != nil || timing.Load.Known() || timing.LoadSource.Known() {
		t.Fatalf("presence/open words: %+v", timing)
	}
	raw, err := wire.ToJSON(timing)
	if err != nil || !strings.Contains(string(raw), `"future_load"`) {
		t.Fatalf("display words lost: %s %v", raw, err)
	}
	for _, raw := range []string{`{"load_ms":null}`, `{"decode_ms":-1}`, `{"reused_tokens":-1}`, `{"load":null}`} {
		if err := wire.FromJSON([]byte(raw), &timing); err == nil {
			t.Fatalf("invalid timing accepted: %s", raw)
		}
	}
}

type chat3HistoryResultFixture struct{}

func (chat3HistoryResultFixture) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeAvailable}, nil
}
func (chat3HistoryResultFixture) Start(wire.RequestV3) (wire.ChatAdmission, error) {
	return wire.ChatAdmission{}, nil
}
func (chat3HistoryResultFixture) Observe(string, int64, int64, int64, int64) (wire.ChatPage, error) {
	return wire.ChatPage{}, nil
}
func (chat3HistoryResultFixture) Cancel(string) (wire.ChatCancellation, error) {
	return wire.ChatCancellation{}, nil
}

func TestChat3HistoryReadSuccessRequiresOwnedWindow(t *testing.T) {
	dispatcher := &wire.ChatV3Dispatcher{Handler: chat3HistoryResultFixture{}}
	_, err := wire.NewChatV3Client(dispatcher).GetHistoryWindow()
	var service *wire.ServiceError
	if !errors.As(err, &service) || service.Code != wire.ServiceErrorCodeInvalidResult {
		t.Fatalf("missing history window reached caller: %v", err)
	}
}
