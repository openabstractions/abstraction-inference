package inference

import (
	"context"
	"maps"
	"net/http"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// remoteChat delegates an operation to another runtime over the remote
// transport (CONTRACT "Remote runtimes"). The request travels unchanged except
// for the chosen model name and the claim of the program that asked; the
// remote binds this runtime's certificate, applies the named credential from
// its own holder, and its deltas are relayed. No header or key crosses.
type remoteChat struct{}

// unsupported leaves feature checks to the remote, which refuses before spend.
func (remoteChat) unsupported(wire.Request) string { return "" }

// remoteObserveWaitMS is how long one relayed Observe waits at the remote.
const remoteObserveWaitMS = 20000

func (remoteChat) run(ctx context.Context, _ *http.Client, host *router.Host, model string, req wire.Request, _ map[string][]byte, _ map[string]string, s *stream) wire.Reply {
	transport, ok := host.RemoteTransport()
	if !ok {
		return failed(wire.ReplyOutcomeUnavailable, "remote:transport")
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Extensions = maps.Clone(req.Extensions)
	if forwarded.Extensions == nil {
		forwarded.Extensions = map[string]string{}
	}
	forwarded.Extensions[ClaimExtension] = s.op.subject.Program
	admission, err := wire.NewChatClient(transport.WithContext(ctx)).Start(forwarded)
	if err != nil {
		return streamBroken(ctx, err)
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return failed(outcome, "remote:"+admission.Reason)
	}
	cancel := func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		wire.NewChatClient(transport.WithContext(stop)).Cancel(admission.Operation)
	}
	cursor := int64(0)
	for {
		page, err := wire.NewChatClient(transport.WithContext(ctx)).Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancel()
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil {
			cancel()
			return failed(wire.ReplyOutcomeUnavailable, "remote:unreachable")
		}
		switch page.Outcome {
		case wire.PageOutcomePage:
		default:
			cancel()
			return failed(wire.ReplyOutcomeUnavailable, "remote:"+page.Outcome.String())
		}
		if !validateRelayChatPage(page, cursor) {
			cancel()
			return failed(wire.ReplyOutcomeUnavailable, "remote:invalid_page")
		}
		if len(page.Deltas) == 0 && !idleRelay(ctx) {
			cancel()
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
		for _, d := range page.Deltas {
			switch d.Kind {
			case wire.DeltaKindPart:
				if d.Part != nil {
					s.part(d.Index, *d.Part)
				}
			case wire.DeltaKindUsage:
				if d.Usage != nil {
					s.usage(*d.Usage)
				}
			case wire.DeltaKindEnd:
				if d.End != nil {
					return *d.End
				}
			}
		}
		cursor = page.Next
	}
}
