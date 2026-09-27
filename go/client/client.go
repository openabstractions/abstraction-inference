// Package client calls an explicitly selected abstraction.inference/chat@1
// service over shared framed IPC. Complete and Stream hide Start, Observe and
// Cancel. Start is never retried.
package client

import (
	"context"
	"errors"
	"iter"
	"sort"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// ErrInconsistent is a reply whose outcome and fields contradict the contract.
var ErrInconsistent = errors.New("inference client: inconsistent reply")

// Page bounds Stream uses for each Observe.
const (
	PageDeltas = 256
	PageBytes  = 65536
	PageWait   = 25 * time.Second
)

// callMargin is the budget beyond an Observe's wait for connecting and replying.
const callMargin = 5 * time.Second

// Chat calls abstraction.inference/chat@1.
type Chat struct{ transport listen.FrameClient }

// New binds an explicit endpoint.
func New(endpoint string) *Chat { return NewWithTransport(listen.FrameClient{Endpoint: endpoint}) }

// NewWithTransport retains the caller's endpoint, server trust and limits.
func NewWithTransport(transport listen.FrameClient) *Chat {
	return &Chat{transport: transport.WithDefaults(callMargin, 1<<20)}
}

func check(ok bool) error {
	if ok {
		return nil
	}
	return ErrInconsistent
}

// Start admits one request. The reply's operation is observed with Observe.
func (c *Chat) Start(ctx context.Context, request Request) (Admission, error) {
	if err := ctx.Err(); err != nil {
		return Admission{}, err
	}
	return checkedAdmission(wire.NewChatClient(c.transport.WithContext(ctx)).Start(request))
}

// Observe reads one bounded page from cursor, waiting up to waitMS. The call's
// budget is the transport's plus the wait.
func (c *Chat) Observe(ctx context.Context, operation string, cursor, maxDeltas, maxBytes, waitMS int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, maxDeltas, maxBytes, waitMS,
		func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.End != nil },
		func(transport listen.FrameClient) (DeltaPage, error) {
			return wire.NewChatClient(transport.WithContext(ctx)).Observe(operation, cursor, maxDeltas, maxBytes, waitMS)
		})
}

// Cancel stops an operation this program started.
func (c *Chat) Cancel(ctx context.Context, operation string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewChatClient(c.transport.WithContext(ctx)).Cancel(operation)
}

// Stream starts request and yields its deltas in sequence until the end delta,
// which carries the reply. A Start refusal yields one end delta carrying a
// reply with that outcome. Leaving the loop early, or a failure, cancels the
// operation. A gap yields ErrGap and stops.
func (c *Chat) Stream(ctx context.Context, request Request) iter.Seq2[Delta, error] {
	return func(yield func(Delta, error) bool) {
		a, err := c.Start(ctx, request)
		if err != nil {
			yield(Delta{}, err)
			return
		}
		if a.Outcome != wire.StartOutcomeAccepted {
			end := refusedReply(a)
			yield(Delta{Kind: wire.DeltaKindEnd, End: &end}, nil)
			return
		}
		ended := false
		defer func() {
			if !ended {
				cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callMargin)
				//unchecked: best-effort cancel after Stream already has its result or error; the caller has nothing left to receive a second failure
				c.Cancel(cancelCtx, a.Operation)
				cancel()
			}
		}()
		cursor := int64(0)
		for {
			page, err := c.Observe(ctx, a.Operation, cursor, PageDeltas, PageBytes, PageWait.Milliseconds())
			if err != nil {
				yield(Delta{}, err)
				return
			}
			switch page.Outcome {
			case wire.PageOutcomePage:
			case wire.PageOutcomeGap:
				yield(Delta{}, ErrGap)
				return
			default:
				yield(Delta{}, &PageRefusal{Outcome: page.Outcome})
				return
			}
			for _, d := range page.Deltas {
				if d.Kind == wire.DeltaKindEnd {
					ended = true
				}
				if !yield(d, nil) {
					return
				}
			}
			cursor = page.Next
			if page.AtEnd {
				ended = true
				return
			}
		}
	}
}

// Complete starts request, observes it to its end and folds the deltas into
// the reply. A Start refusal is a reply with that outcome.
func (c *Chat) Complete(ctx context.Context, request Request) (Reply, error) {
	var fold Fold
	for d, err := range c.Stream(ctx, request) {
		if err != nil {
			return Reply{}, err
		}
		fold.Add(d)
	}
	reply, ok := fold.Reply()
	if !ok {
		return Reply{}, ErrInconsistent
	}
	return reply, nil
}

// ErrGap is an observation that fell behind the service's retained deltas.
var ErrGap = errors.New("inference client: deltas older than the retained window")

// PageRefusal is an Observe refusal: unknown, invalid, forbidden or unavailable.
type PageRefusal struct{ Outcome PageOutcome }

func (e *PageRefusal) Error() string { return "inference client: observe " + e.Outcome.String() }

func refusedReply(a Admission) Reply {
	return Reply{Outcome: replyFromAdmission(a.Outcome), Reason: a.Reason, StopReason: wire.StopReasonNoStop,
		Message: wire.Message{Role: wire.RoleAssistant, Parts: []wire.Part{}}}
}

// Fold assembles a reply from deltas: part deltas extend the part at their
// index, and the end delta supplies outcome, usage, host and model.
type Fold struct {
	parts map[int64]*wire.Part
	end   *wire.Reply
}

// Add folds one delta.
func (f *Fold) Add(d Delta) {
	switch d.Kind {
	case wire.DeltaKindPart:
		if d.Part == nil {
			return
		}
		if f.parts == nil {
			f.parts = map[int64]*wire.Part{}
		}
		p := f.parts[d.Index]
		if p == nil {
			copied := *d.Part
			f.parts[d.Index] = &copied
			return
		}
		p.Text += d.Part.Text
		p.Arguments += d.Part.Arguments
		for _, field := range []struct {
			into *string
			from string
		}{{&p.CallID, d.Part.CallID}, {&p.Name, d.Part.Name}, {&p.Digest, d.Part.Digest}, {&p.MediaType, d.Part.MediaType}} {
			if *field.into == "" {
				*field.into = field.from
			}
		}
	case wire.DeltaKindEnd:
		f.end = d.End
	}
}

// Reply is the folded reply, once the end delta arrived.
func (f *Fold) Reply() (Reply, bool) {
	if f.end == nil {
		return Reply{}, false
	}
	reply := *f.end
	indexes := make([]int64, 0, len(f.parts))
	for i := range f.parts {
		indexes = append(indexes, i)
	}
	sort.Slice(indexes, func(a, b int) bool { return indexes[a] < indexes[b] })
	reply.Message = wire.Message{Role: wire.RoleAssistant, Parts: make([]wire.Part, 0, len(indexes))}
	for _, i := range indexes {
		reply.Message.Parts = append(reply.Message.Parts, *f.parts[i])
	}
	return reply, true
}
