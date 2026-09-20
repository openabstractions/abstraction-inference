package inference

import (
	"context"
	"net/http"
	"slices"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// nativeChat delegates text chat to a declared local provider over an
// authenticated native connection. The downstream observes this OA service as
// its caller and creates its own operation identifier.
type nativeChat struct{}

func (nativeChat) unsupported(req wire.Request) string {
	return unsupportedBy(req, map[string]bool{FeatureTools: true}, map[string]bool{})
}

func localGuarantees(in []wire.RequestGuarantee) []wire.RequestGuarantee {
	out := make([]wire.RequestGuarantee, 0, len(in)+1)
	for _, guarantee := range in {
		if guarantee != wire.RequestGuaranteeHostedAllowed && guarantee != wire.RequestGuaranteeLocalOnly {
			out = append(out, guarantee)
		}
	}
	return append(out, wire.RequestGuaranteeLocalOnly)
}

func (nativeChat) run(ctx context.Context, _ *http.Client, host *router.Host, model string, req wire.Request, _ map[string][]byte, _ map[string]string, s *stream) wire.Reply {
	transport, ok := host.NativeTransport()
	if !ok {
		return failed(wire.ReplyOutcomeUnavailable, "native:transport")
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Guarantees = localGuarantees(req.Guarantees)
	forwarded.RequiredExtensions = slices.Clone(req.RequiredExtensions)
	forwarded.Extensions = nil
	admission, err := wire.NewChatClient(transport.WithContext(ctx)).Start(forwarded)
	if err != nil {
		return streamBroken(ctx, err)
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return failed(outcome, "native:"+admission.Reason)
	}
	cancel := func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = wire.NewChatClient(transport.WithContext(stop)).Cancel(admission.Operation)
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
			return failed(wire.ReplyOutcomeUnavailable, "native:unreachable")
		}
		if page.Outcome != wire.PageOutcomePage {
			cancel()
			return failed(wire.ReplyOutcomeUnavailable, "native:"+page.Outcome.String())
		}
		if !validateRelayChatPage(page, cursor) {
			cancel()
			return failed(wire.ReplyOutcomeUnavailable, "native:invalid_page")
		}
		if len(page.Deltas) == 0 && !idleRelay(ctx) {
			cancel()
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
		for _, delta := range page.Deltas {
			switch delta.Kind {
			case wire.DeltaKindPart:
				if delta.Part != nil {
					s.part(delta.Index, *delta.Part)
				}
			case wire.DeltaKindUsage:
				if delta.Usage != nil {
					s.usage(*delta.Usage)
				}
			case wire.DeltaKindEnd:
				if delta.End != nil {
					return *delta.End
				}
			}
		}
		cursor = page.Next
	}
}
