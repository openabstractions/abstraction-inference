package inference_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type boundaryRefusal struct{ claimChat }

func (boundaryRefusal) Start(wire.Request) (wire.Admission, error) {
	return wire.Admission{Outcome: wire.StartOutcomeNotPermitted, Reason: "rights:denied"}, nil
}

type boundaryReplyConn struct {
	net.Conn
	response []byte
	cancel   context.CancelFunc
	mode     string
}

func (c *boundaryReplyConn) Write(p []byte) (int, error) {
	raw, err := (&wire.ChatDispatcher{Handler: boundaryRefusal{}}).ExchangeFrame(p[4:])
	if err != nil {
		return 0, err
	}
	if c.mode == "malformed" {
		raw = []byte{0}
	}
	c.response = make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(c.response, uint32(len(raw)))
	copy(c.response[4:], raw)
	if c.mode == "incomplete" {
		c.response = c.response[:len(c.response)-1]
	}
	return len(p), nil
}
func (c *boundaryReplyConn) Read(p []byte) (int, error) {
	if len(c.response) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.response)
	c.response = c.response[n:]
	if len(c.response) == 0 && c.cancel != nil {
		c.cancel()
	}
	return n, nil
}
func (*boundaryReplyConn) Close() error                { return nil }
func (*boundaryReplyConn) SetDeadline(time.Time) error { return nil }

// Cancellation on the final read must leave a complete service answer available
// to generated decoding. A partial answer still supplies no authoritative result.
func TestRemoteCompleteReplyCancellationBoundary(t *testing.T) {
	for _, mode := range []string{"control", "complete", "incomplete", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &boundaryReplyConn{mode: mode}
			if mode != "control" {
				conn.cancel = cancel
			}
			transport := listen.FrameClient{Dialer: func(context.Context, string) (net.Conn, error) { return conn, nil }}
			result, err := wire.NewChatClient(transport.WithContext(ctx)).Start(wire.Request{Model: "model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}}})
			switch mode {
			case "control", "complete":
				if err != nil || result.Outcome != wire.StartOutcomeNotPermitted || result.Reason != "rights:denied" {
					t.Fatalf("reply=%+v error=%v", result, err)
				}
			case "incomplete":
				if !errors.Is(err, listen.ErrRequestUncertain) || !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case "malformed":
				if err == nil || errors.Is(err, listen.ErrRequestNotSent) {
					t.Fatalf("reply=%+v error=%v", result, err)
				}
			}
		})
	}
}

func TestRemoteExecutionLostReplyAcrossProfiles(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		for _, cancelAfterSend := range []bool{false, true} {
			for _, profile := range []string{"chat", "image", "speech", "transcription", "live", "embed"} {
				name := profile
				if cancelAfterSend {
					name += "/cancel"
				}
				if malformed {
					name += "/malformed"
				}
				t.Run(name, func(t *testing.T) {
					gate := make(chan struct{})
					defer func() {
						select {
						case <-gate:
						default:
							close(gate)
						}
					}()
					f := newClaimFixtureOutcome(t, true, gate, malformed)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var a wire.Admission
					embedded := make(chan wire.Embeddings, 1)
					switch profile {
					case "chat":
						a = f.provider.Start(ctx, claimCaller, wire.Request{Model: "claim-model", Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hello"}}}}, Guarantees: claimHosted, Credential: "remote-key"})
					case "image":
						a = f.provider.StartImage(ctx, claimCaller, wire.ImageRequest{Model: "claim-model", Mode: wire.ImageModeGenerate, Prompt: "hello", Size: "1024x1024", Count: 1, Guarantees: claimHosted, Credential: "remote-key"})
					case "speech":
						a = f.provider.StartSpeech(ctx, claimCaller, wire.SpeechRequest{Model: "claim-model", Voice: "alloy", Text: "hello", Format: wire.SpeechFormatMp3, Guarantees: claimHosted, Credential: "remote-key"})
					case "transcription":
						a = f.provider.StartTranscription(ctx, claimCaller, wire.TranscriptionRequest{Model: "claim-model", AudioDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", MediaType: "audio/wav", Timestamps: wire.TimestampModeSegment, Guarantees: claimHosted, Credential: "remote-key"})
					case "live":
						a = f.provider.StartLive(ctx, claimCaller, wire.LiveRequest{Model: "claim-model", Voice: "alloy", Format: wire.LiveFormatPcm1624000, Guarantees: claimHosted, Credential: "remote-key"})
					case "embed":
						go func() {
							embedded <- f.provider.Embed(ctx, claimCaller, wire.EmbedRequest{Model: "claim-model", Inputs: []string{"hello"}, Guarantees: claimHosted, Credential: "remote-key"})
						}()
					}
					if profile != "embed" && a.Outcome != wire.StartOutcomeAccepted {
						t.Fatalf("admission=%+v", a)
					}
					select {
					case <-f.seen:
					case <-time.After(5 * time.Second):
						t.Fatal("remote received no request")
					}
					if cancelAfterSend {
						if profile == "embed" {
							cancel()
						} else {
							var stopped wire.Cancellation
							switch profile {
							case "chat":
								stopped = f.provider.Cancel(claimCaller, a.Operation)
							case "image":
								stopped = f.provider.CancelImage(claimCaller, a.Operation)
							case "speech":
								stopped = f.provider.CancelSpeech(claimCaller, a.Operation)
							case "transcription":
								stopped = f.provider.CancelTranscription(claimCaller, a.Operation)
							case "live":
								stopped = f.provider.CancelLive(claimCaller, a.Operation)
							}
							// Cancellation was requested, but the remote result is still
							// unknown. Cancel must not claim confirmed cancellation.
							if stopped.Outcome != wire.CancelOutcomeEnded || stopped.ReplyOutcome == nil || *stopped.ReplyOutcome != wire.ReplyOutcomeUnavailable {
								t.Fatalf("cancel=%+v", stopped)
							}
						}
					}
					close(gate)
					if profile == "embed" {
						select {
						case reply := <-embedded:
							if reply.Outcome != wire.EmbedOutcomeUnavailable || reply.Reason != "remote:uncertain" {
								t.Fatalf("reply=%+v", reply)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("embed did not finish")
						}
					}
					select {
					case record := <-f.records:
						if record.Outcome != "unavailable" || record.Reason != "remote:uncertain" || record.Host != "lab" || record.Operation != a.Operation {
							t.Fatalf("record=%+v admission=%+v", record, a)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("no failure record")
					}
					if len(f.seen) != 0 {
						t.Fatal("remote execution was replayed")
					}
				})
			}
		}
	}
}
