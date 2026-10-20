package inference

import (
	"context"
	"net/http"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
)

// nativeChat delegates text chat to a declared local provider over an
// authenticated native connection. The downstream observes this OA service as
// its caller and creates its own operation identifier.
type nativeChat struct{}

// A native provider takes input images: the runtime resolves each one under
// the caller's rights and stages it in the provider's content store before
// Start (stageNativeImages).
func (nativeChat) unsupported(req wire.Request) string {
	return unsupportedBy(req, map[string]bool{FeatureTools: true, FeatureVision: true}, nil)
}

// stageNativeImages uploads each distinct input image of req, resolved in
// images, to the native provider's content store, as runNativeImage stages
// image inputs; the provider resolves the request's digests from there.
func stageNativeImages(ctx context.Context, transport listen.FrameClient, operation string, req wire.Request, images map[string][]byte) error {
	staged := map[string]bool{}
	for _, message := range req.Messages {
		for _, part := range message.Parts {
			data, ok := images[part.Digest]
			if part.Kind != wire.PartKindImage || !ok || staged[part.Digest] || len(part.Digest) < 19 {
				continue
			}
			if err := mediationcontent.Upload(ctx, transport, operation+"-"+part.Digest[7:19], part.Digest, part.MediaType, data, int64(len(data))); err != nil {
				return err
			}
			staged[part.Digest] = true
		}
	}
	return nil
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

// nativeChatRequest is req as this service forwards it to a native leaf. The
// leaf keeps the caller's cache partition, derived here from the attested
// account and program: calls of one program may reuse a prompt prefix, and
// calls of another program never do.
func nativeChatRequest(req wire.Request, model string, op *operation) wire.Request {
	forwarded := req
	forwarded.Model = model
	forwarded.Guarantees = localGuarantees(req.Guarantees)
	forwarded.Extensions, forwarded.RequiredExtensions = forwardedNativeExtensions(req.Extensions, req.RequiredExtensions, op.subject.Program)
	forwarded.Extensions = forwardedChatCacheExtensions(forwarded.Extensions, op.cache)
	return forwarded
}

func (nativeChat) run(ctx context.Context, _ *http.Client, host *router.Host, model string, req wire.Request, images map[string][]byte, _ map[string]string, s *stream) wire.Reply {
	transport, ok := host.NativeTransport()
	if !ok {
		return failed(wire.ReplyOutcomeUnavailable, "native:transport")
	}
	if err := stageNativeImages(ctx, transport, s.op.id, req, images); err != nil {
		if ctx.Err() != nil {
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
		return failed(wire.ReplyOutcomeUnavailable, "native:input")
	}
	forwarded := nativeChatRequest(req, model, s.op)
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
		//unchecked: best-effort cancel in a cleanup closure; the caller already has its own error and nothing left to receive a second one
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
