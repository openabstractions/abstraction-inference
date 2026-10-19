package inference

import (
	"context"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
)

// runNativeImage relays one image operation to the exact selected native
// provider. A lost Start reply is ambiguous: never retry or pick another host.
// Provider-local output digests are copied and verified before any caller-local
// delivery is emitted.
func (p *Provider) runNativeImage(ctx context.Context, op *operation, req wire.ImageRequest, inputs map[string]resolvedImageInput, commits []ContentCommitter) wire.ImageReply {
	transport, ok := op.host.NativeTransport()
	if !ok {
		return imageFailure(wire.ReplyOutcomeUnavailable, "native:transport")
	}
	for digest, input := range inputs {
		requestID := op.id + "-" + digest[7:19]
		if err := mediationcontent.Upload(ctx, transport, requestID, digest, input.mediaType, input.data, p.cfg.MaxImageBytes); err != nil {
			if ctx.Err() != nil {
				return imageFailure(wire.ReplyOutcomeCancelled, "caller")
			}
			return imageFailure(wire.ReplyOutcomeUnavailable, "native:input")
		}
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Credential = ""
	forwarded.Guarantees = localGuarantees(req.Guarantees)
	forwarded.Extensions, forwarded.RequiredExtensions = forwardedNativeExtensions(req.Extensions, req.RequiredExtensions, op.subject.Program)
	client := wire.NewImageClient(transport.WithContext(ctx))
	admission, err := client.Start(forwarded)
	if err != nil {
		if ctx.Err() != nil {
			return imageFailure(wire.ReplyOutcomeCancelled, "caller")
		}
		return imageFailure(wire.ReplyOutcomeUnavailable, "native:start_unknown")
	}
	if cancelledAfterNativeStart(ctx, admission, func(id string) { cancelAcceptedNativeImage(transport, id) }) {
		return imageFailure(wire.ReplyOutcomeCancelled, "caller")
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return imageFailure(outcome, "native:"+admission.Reason)
	}
	cancelNative := func() { cancelAcceptedNativeImage(transport, admission.Operation) }
	var results []wire.ImageResult
	for cursor := int64(0); ; {
		page, err := client.Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancelNative()
			return imageFailure(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil || page.Outcome != wire.PageOutcomePage {
			cancelNative()
			return imageFailure(wire.ReplyOutcomeUnavailable, "native:unreachable")
		}
		if reason := validateRemoteImagePage(page, cursor); reason != "" {
			cancelNative()
			return imageFailure(wire.ReplyOutcomeUnavailable, "native:invalid_page")
		}
		for _, delta := range page.Deltas {
			if reason := remoteImageDeltaShape(delta); reason != "" {
				cancelNative()
				return imageFailure(wire.ReplyOutcomeUnavailable, "native:invalid_page")
			}
			switch delta.Kind {
			case wire.DeltaKindImageProgress:
				progress := *delta.ImageProgress
				op.emit(wire.Delta{Kind: wire.DeltaKindImageProgress, ImageProgress: &progress}, p.cfg.Now())
			case wire.DeltaKindImageResult:
				result := *delta.ImageResult
				if int64(len(results)) >= req.Count || result.Index != int64(len(results)) || !canonicalSHA256(result.Delivery.Digest) || !generatedInputMedia(result.Delivery.MediaType) || result.Delivery.Size < 1 || result.Delivery.Size > p.cfg.MaxGeneratedImageBytes || result.Delivery.Location != "local" {
					cancelNative()
					return imageFailure(wire.ReplyOutcomeUnavailable, "native:delivery")
				}
				results = append(results, result)
			case wire.DeltaKindEnd:
				end := *delta.ImageEnd
				if !validImageAccounting(end.Reason, end.Usage, end.Cost) {
					cancelNative()
					return imageFailure(wire.ReplyOutcomeUnavailable, "native:terminal")
				}
				if reason := validateRemoteImageTerminal(req, results, end); reason != "" {
					cancelNative()
					return imageFailure(wire.ReplyOutcomeUnavailable, "native:terminal")
				}
				if end.Outcome != wire.ReplyOutcomeCompleted {
					return end
				}
				outputs := make([]generatedImage, 0, len(end.Deliveries))
				var total int64
				for _, delivery := range end.Deliveries {
					if delivery.Size > p.cfg.MaxImageOutputBytes-total {
						return imageFailure(wire.ReplyOutcomeUnavailable, "native:delivery")
					}
					data, err := mediationcontent.Download(ctx, transport, delivery.Digest, delivery.MediaType, delivery.Size, p.cfg.MaxGeneratedImageBytes)
					if err != nil {
						if ctx.Err() != nil {
							return imageFailure(wire.ReplyOutcomeCancelled, "caller")
						}
						return imageFailure(wire.ReplyOutcomeUnavailable, "native:content")
					}
					outputs = append(outputs, generatedImage{data: data, mediaType: delivery.MediaType})
					total += delivery.Size
				}
				if ctx.Err() != nil {
					return imageFailure(wire.ReplyOutcomeCancelled, "caller")
				}
				return p.publishImages(ctx, op, req, imageBackendReply{outcome: end.Outcome, reason: end.Reason, images: outputs, usage: end.Usage, cost: end.Cost}, commits)
			}
		}
		if page.AtEnd {
			cancelNative()
			return imageFailure(wire.ReplyOutcomeUnavailable, "native:terminal")
		}
		if len(page.Deltas) == 0 && !waitImagePoll(ctx, minimumImagePollDelay) {
			cancelNative()
			return imageFailure(wire.ReplyOutcomeCancelled, "caller")
		}
		cursor = page.Next
	}
}

func cancelAcceptedNativeImage(transport listen.FrameClient, operation string) {
	stop, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	_, _ = wire.NewImageClient(transport.WithContext(stop)).Cancel(operation)
}

func cancelledAfterNativeStart(ctx context.Context, admission wire.Admission, cancel func(string)) bool {
	if ctx.Err() == nil {
		return false
	}
	if admission.Outcome == wire.StartOutcomeAccepted {
		cancel(admission.Operation)
	}
	return true
}
