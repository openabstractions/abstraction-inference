package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// Speech calls abstraction.inference/speech@1.
type Speech struct{ transport listen.FrameClient }

func NewSpeech(endpoint string) *Speech {
	return NewSpeechWithTransport(listen.FrameClient{Endpoint: endpoint})
}
func NewSpeechWithTransport(transport listen.FrameClient) *Speech {
	return &Speech{transport: transport.WithDefaults(callMargin, 1<<20)}
}
func (c *Speech) Start(ctx context.Context, request SpeechRequest) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	return checkedAdmission(wire.NewSpeechClient(c.transport.WithContext(ctx)).Start(request))
}
func (c *Speech) Observe(ctx context.Context, operation string, cursor, maxDeltas, maxBytes, waitMS int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, maxDeltas, maxBytes, waitMS,
		func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.SpeechEnd != nil },
		func(transport listen.FrameClient) (DeltaPage, error) {
			return wire.NewSpeechClient(transport.WithContext(ctx)).Observe(operation, cursor, maxDeltas, maxBytes, waitMS)
		})
}
func (c *Speech) Cancel(ctx context.Context, operation string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewSpeechClient(c.transport.WithContext(ctx)).Cancel(operation)
}

// Synthesize drains audio chunks and verifies the terminal digest describes
// exactly the delivered bytes.
func (c *Speech) Synthesize(ctx context.Context, request SpeechRequest) ([]byte, SpeechReply, error) {
	a, err := c.Start(ctx, request)
	if err != nil {
		return nil, SpeechReply{}, err
	}
	if a.Outcome != wire.StartOutcomeAccepted {
		return nil, SpeechReply{Outcome: replyFromAdmission(a.Outcome), Reason: a.Reason}, nil
	}
	ended := false
	defer func() {
		if !ended {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callMargin)
			_, _ = c.Cancel(cancelCtx, a.Operation)
			cancel()
		}
	}()
	var audio []byte
	for cursor := int64(0); ; {
		page, err := c.Observe(ctx, a.Operation, cursor, PageDeltas, PageBytes, PageWait.Milliseconds())
		if err != nil {
			return nil, SpeechReply{}, err
		}
		if page.Outcome == wire.PageOutcomeGap {
			return nil, SpeechReply{}, ErrGap
		}
		if page.Outcome != wire.PageOutcomePage {
			return nil, SpeechReply{}, &PageRefusal{Outcome: page.Outcome}
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindAudio && d.Audio != nil {
				audio = append(audio, d.Audio.Data...)
			}
			if d.Kind == wire.DeltaKindEnd {
				ended = true
				reply := *d.SpeechEnd
				if reply.Outcome == wire.ReplyOutcomeCompleted {
					sum := sha256.Sum256(audio)
					if reply.Delivery.Size != int64(len(audio)) || reply.Delivery.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
						return nil, SpeechReply{}, ErrInconsistent
					}
				}
				return audio, reply, nil
			}
		}
		if page.AtEnd {
			return nil, SpeechReply{}, ErrInconsistent
		}
		cursor = page.Next
	}
}
