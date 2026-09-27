package client

import (
	"context"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// Image calls abstraction.inference/image@1.
type Image struct{ transport listen.FrameClient }

func NewImage(endpoint string) *Image {
	return NewImageWithTransport(listen.FrameClient{Endpoint: endpoint})
}
func NewImageWithTransport(transport listen.FrameClient) *Image {
	return &Image{transport: transport.WithDefaults(callMargin, 1<<20)}
}
func (c *Image) Start(ctx context.Context, request ImageRequest) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	return checkedAdmission(wire.NewImageClient(c.transport.WithContext(ctx)).Start(request))
}
func (c *Image) Observe(ctx context.Context, operation string, cursor, maxDeltas, maxBytes, waitMS int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, maxDeltas, maxBytes, waitMS,
		func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.ImageEnd != nil },
		func(transport listen.FrameClient) (DeltaPage, error) {
			return wire.NewImageClient(transport.WithContext(ctx)).Observe(operation, cursor, maxDeltas, maxBytes, waitMS)
		})
}
func (c *Image) Cancel(ctx context.Context, operation string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewImageClient(c.transport.WithContext(ctx)).Cancel(operation)
}

// Generate drains one image operation and returns its ordered typed results.
func (c *Image) Generate(ctx context.Context, request ImageRequest) ([]ImageResult, ImageReply, error) {
	a, err := c.Start(ctx, request)
	if err != nil {
		return nil, ImageReply{}, err
	}
	if a.Outcome != wire.StartOutcomeAccepted {
		return nil, ImageReply{Outcome: replyFromAdmission(a.Outcome), Reason: a.Reason}, nil
	}
	ended := false
	defer func() {
		if !ended {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callMargin)
			//unchecked: best-effort cancel after Generate already has its result or error; the caller has nothing left to receive a second failure
			_, _ = c.Cancel(cancelCtx, a.Operation)
			cancel()
		}
	}()
	var results []ImageResult
	for cursor := int64(0); ; {
		page, err := c.Observe(ctx, a.Operation, cursor, PageDeltas, PageBytes, PageWait.Milliseconds())
		if err != nil {
			return nil, ImageReply{}, err
		}
		if page.Outcome == wire.PageOutcomeGap {
			return nil, ImageReply{}, ErrGap
		}
		if page.Outcome != wire.PageOutcomePage {
			return nil, ImageReply{}, &PageRefusal{Outcome: page.Outcome}
		}
		for _, delta := range page.Deltas {
			if delta.Kind == wire.DeltaKindImageResult && delta.ImageResult != nil {
				if delta.ImageResult.Index != int64(len(results)) {
					return nil, ImageReply{}, ErrInconsistent
				}
				results = append(results, *delta.ImageResult)
			}
			if delta.Kind == wire.DeltaKindEnd {
				ended = true
				if delta.ImageEnd == nil {
					return nil, ImageReply{}, ErrInconsistent
				}
				reply := *delta.ImageEnd
				if reply.Outcome == wire.ReplyOutcomeCompleted {
					if len(reply.Deliveries) != len(results) {
						return nil, ImageReply{}, ErrInconsistent
					}
					for i := range results {
						if results[i].Delivery != reply.Deliveries[i] {
							return nil, ImageReply{}, ErrInconsistent
						}
					}
				}
				return results, reply, nil
			}
		}
		if page.AtEnd {
			return nil, ImageReply{}, ErrInconsistent
		}
		cursor = page.Next
	}
}
