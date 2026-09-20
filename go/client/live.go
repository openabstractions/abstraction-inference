package client

import (
	"context"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type Live struct{ transport listen.FrameClient }

func NewLive(endpoint string) *Live {
	return NewLiveWithTransport(listen.FrameClient{Endpoint: endpoint})
}
func NewLiveWithTransport(t listen.FrameClient) *Live {
	return &Live{transport: t.WithDefaults(callMargin, 1<<20)}
}
func (c *Live) Start(ctx context.Context, q wire.LiveRequest) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	return checkedAdmission(wire.NewLiveClient(c.transport.WithContext(ctx)).Start(q))
}
func (c *Live) Append(ctx context.Context, id string, sequence int64, data []byte) (wire.LiveInputResult, error) {
	if err := ctx.Err(); err != nil {
		return wire.LiveInputResult{}, err
	}
	return wire.NewLiveClient(c.transport.WithContext(ctx)).Append(id, sequence, data)
}
func (c *Live) Commit(ctx context.Context, id string) (wire.LiveInputResult, error) {
	if err := ctx.Err(); err != nil {
		return wire.LiveInputResult{}, err
	}
	return wire.NewLiveClient(c.transport.WithContext(ctx)).Commit(id)
}
func (c *Live) Observe(ctx context.Context, id string, cursor, n, size, wait int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, n, size, wait, func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.LiveEnd != nil }, func(t listen.FrameClient) (DeltaPage, error) {
		return wire.NewLiveClient(t.WithContext(ctx)).Observe(id, cursor, n, size, wait)
	})
}
func (c *Live) Cancel(ctx context.Context, id string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewLiveClient(c.transport.WithContext(ctx)).Cancel(id)
}
