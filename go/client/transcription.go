package client

import (
	"context"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// Transcriptions calls abstraction.inference/transcription@1.
type Transcriptions struct{ transport listen.FrameClient }

func NewTranscriptions(endpoint string) *Transcriptions {
	return NewTranscriptionsWithTransport(listen.FrameClient{Endpoint: endpoint})
}

func NewTranscriptionsWithTransport(transport listen.FrameClient) *Transcriptions {
	return &Transcriptions{transport: transport.WithDefaults(callMargin, 1<<20)}
}

func (c *Transcriptions) Start(ctx context.Context, request TranscriptionRequest) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	return checkedAdmission(wire.NewTranscriptionClient(c.transport.WithContext(ctx)).Start(request))
}

func (c *Transcriptions) Observe(ctx context.Context, operation string, cursor, maxDeltas, maxBytes, waitMS int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, maxDeltas, maxBytes, waitMS,
		func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.TranscriptionEnd != nil },
		func(transport listen.FrameClient) (DeltaPage, error) {
			return wire.NewTranscriptionClient(transport.WithContext(ctx)).Observe(operation, cursor, maxDeltas, maxBytes, waitMS)
		})
}

func (c *Transcriptions) Cancel(ctx context.Context, operation string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewTranscriptionClient(c.transport.WithContext(ctx)).Cancel(operation)
}

// Transcribe drains one operation into ordered timestamp units. A caller
// cancellation is followed by a bounded best-effort Cancel.
func (c *Transcriptions) Transcribe(ctx context.Context, request TranscriptionRequest) ([]TranscriptSegment, TranscriptionReply, error) {
	a, err := c.Start(ctx, request)
	if err != nil {
		return nil, TranscriptionReply{}, err
	}
	if a.Outcome != wire.StartOutcomeAccepted {
		return nil, TranscriptionReply{Outcome: replyFromAdmission(a.Outcome), Reason: a.Reason}, nil
	}
	ended := false
	defer func() {
		if !ended {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callMargin)
			//unchecked: best-effort cancel after Transcribe already has its result or error; the caller has nothing left to receive a second failure
			_, _ = c.Cancel(cancelCtx, a.Operation)
			cancel()
		}
	}()
	var segments []TranscriptSegment
	for cursor := int64(0); ; {
		page, err := c.Observe(ctx, a.Operation, cursor, PageDeltas, PageBytes, PageWait.Milliseconds())
		if err != nil {
			return nil, TranscriptionReply{}, err
		}
		if page.Outcome == wire.PageOutcomeGap {
			return nil, TranscriptionReply{}, ErrGap
		}
		if page.Outcome != wire.PageOutcomePage {
			return nil, TranscriptionReply{}, &PageRefusal{Outcome: page.Outcome}
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindSegment && d.Segment != nil {
				segments = append(segments, *d.Segment)
			}
			if d.Kind == wire.DeltaKindEnd {
				ended = true
				return segments, *d.TranscriptionEnd, nil
			}
		}
		if page.AtEnd {
			return nil, TranscriptionReply{}, ErrInconsistent
		}
		cursor = page.Next
	}
}
