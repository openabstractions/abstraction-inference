package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// lyingSpeech is a native leaf's speech@1 that relays one audio chunk and
// ends with the delivery it is given, true or not.
type lyingSpeech struct {
	audio    []byte
	delivery wire.Delivery
}

func (l lyingSpeech) Start(wire.SpeechRequest) (wire.Admission, error) {
	return wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: "leaf-op", Host: "leaf", Model: "fixture-voice"}, nil
}

func (l lyingSpeech) Observe(_ string, cursor, _, _, _ int64) (wire.DeltaPage, error) {
	deltas := []wire.Delta{
		{Sequence: 0, Kind: wire.DeltaKindAudio, Audio: &wire.AudioChunk{Index: 0, Data: l.audio}},
		{Sequence: 1, Kind: wire.DeltaKindEnd, SpeechEnd: &wire.SpeechReply{Outcome: wire.ReplyOutcomeCompleted, Delivery: l.delivery}},
	}
	return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: deltas[cursor:], Next: 2}, nil
}

func (lyingSpeech) Cancel(string) (wire.Cancellation, error) {
	return wire.Cancellation{Outcome: wire.CancelOutcomeEnded}, nil
}

// nativeSpeechFixture serves speech@1 from handler on the identity-bound
// native transport, as nativeEmbedFixture serves embed@1.
func nativeSpeechFixture(t *testing.T, handler wire.Speech) *router.Host {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listen.Endpoint("inf-speech-" + fixtureSuffix())
	l, err := listen.ListenFramed(endpoint, listen.Program)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); l.Close() })
	dispatcher := &wire.SpeechDispatcher{Handler: handler}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				callCtx, done := context.WithTimeout(ctx, 5*time.Second)
				defer done()
				call, err := listen.ReceiveFramed(callCtx, conn, listen.Program, 1<<20)
				if err != nil {
					return
				}
				defer call.Close()
				if reply, err := dispatcher.ExchangeFrame(call.Frame); err == nil {
					call.Reply(reply)
				}
			}()
		}
	}()
	host, err := router.NewNative("leaf", endpoint, listen.ServerExpectation{Principal: principalOfThisProcess(t), Program: exe}, []string{"fixture-voice"}, []string{router.ProfileSpeech})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// A native leaf whose terminal delivery differs from the audio it relayed in
// digest, size, media type or location ends native:delivery, and nothing is
// committed (INF-S7). The caller's own output authorization is prepared for
// the original caller, never for the runtime or the leaf.
func TestNativeSpeechRefusesADeliveryThatIsNotTheRelayedAudio(t *testing.T) {
	audio := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 64)...)
	sum := sha256.Sum256(audio)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	other := sha256.Sum256([]byte("other"))
	for name, delivery := range map[string]wire.Delivery{
		"digest":   {Digest: "sha256:" + hex.EncodeToString(other[:]), MediaType: "audio/wav", Size: int64(len(audio)), Location: "local"},
		"size":     {Digest: digest, MediaType: "audio/wav", Size: int64(len(audio)) + 1, Location: "local"},
		"media":    {Digest: digest, MediaType: "audio/mpeg", Size: int64(len(audio)), Location: "local"},
		"location": {Digest: digest, MediaType: "audio/wav", Size: int64(len(audio)), Location: "remote.example"},
	} {
		t.Run(name, func(t *testing.T) {
			store := &speechStore{}
			p, err := New(Config{Router: router.New(nativeSpeechFixture(t, lyingSpeech{audio: audio, delivery: delivery})),
				Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
				Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
					return nil, "not_permitted"
				},
				PrepareContentWrite: store.prepare})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			a := p.StartSpeech(context.Background(), caller, wire.SpeechRequest{Model: "fixture-voice", Voice: "af_sky", Text: "Done.",
				Format: wire.SpeechFormatWav, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}})
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			_, _, end := drainSpeech(t, p, caller, a.Operation)
			if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "native:delivery" {
				t.Fatalf("end %+v", end)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.data != nil || store.digest != "" {
				t.Fatal("a delivery that is not the relayed audio was committed")
			}
			if store.subject != caller {
				t.Fatalf("output prepared for %+v, not the original caller", store.subject)
			}
		})
	}
}
