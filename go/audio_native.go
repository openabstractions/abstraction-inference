package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
)

// runNativeTranscription relays one transcription to the exact selected
// native provider, as runNativeImage relays image work: the caller's audio,
// already resolved and verified under the caller's content policy, is staged
// at the provider's content-writer@1 by digest, and the provider's segments
// are checked and relayed in order. A lost Start reply is ambiguous: the
// relay never retries or picks another host.
func (p *Provider) runNativeTranscription(ctx context.Context, op *operation, req wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	transport, ok := op.host.NativeTransport()
	if !ok {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:transport")
	}
	if err := mediationcontent.Upload(ctx, transport, op.id+"-"+req.AudioDigest[7:19], req.AudioDigest, req.MediaType, audio, p.cfg.MaxAudioBytes); err != nil {
		if ctx.Err() != nil {
			return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:input")
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Credential = ""
	forwarded.Guarantees = localGuarantees(req.Guarantees)
	forwarded.Extensions, _ = forwardedNativeExtensions(req.Extensions, nil, op.subject.Program)
	client := wire.NewTranscriptionClient(transport.WithContext(ctx))
	admission, err := client.Start(forwarded)
	if err != nil {
		if ctx.Err() != nil {
			return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:start_unknown")
	}
	cancelNative := func() { cancelNativeTranscription(transport, admission.Operation) }
	if cancelledAfterNativeStart(ctx, admission, func(string) { cancelNative() }) {
		return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		return transcriptionFailed(nativeStartReply(admission.Outcome), "native:"+admission.Reason)
	}
	for cursor := int64(0); ; {
		page, err := client.Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancelNative()
			return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil || page.Outcome != wire.PageOutcomePage || page.Next < cursor {
			cancelNative()
			return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:unreachable")
		}
		for i, d := range page.Deltas {
			if d.Sequence != cursor+int64(i) {
				cancelNative()
				return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:invalid_page")
			}
			switch {
			case d.Kind == wire.DeltaKindSegment && d.Segment != nil && validSegment(*d.Segment):
				emit(*d.Segment)
			case d.Kind == wire.DeltaKindEnd && d.TranscriptionEnd != nil:
				end := *d.TranscriptionEnd
				if !end.Outcome.Known() || !validImageAccounting(end.Reason, end.Usage, end.Cost) || !validUpstreamLanguage(end.Language) || end.DurationMs < 0 {
					return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:terminal")
				}
				return end
			default:
				cancelNative()
				return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:invalid_page")
			}
		}
		if page.AtEnd {
			cancelNative()
			return transcriptionFailed(wire.ReplyOutcomeUnavailable, "native:terminal")
		}
		if len(page.Deltas) == 0 && !waitImagePoll(ctx, minimumImagePollDelay) {
			cancelNative()
			return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		cursor = page.Next
	}
}

// runNativeSpeech relays one speech request to the exact selected native
// provider. Its audio deltas are relayed as they arrive; the provider-local
// delivery is then read from its content-reader@1, checked against the
// relayed bytes' digest, size and media type, and published under the
// caller's own output authorization (INF-S7).
func (p *Provider) runNativeSpeech(ctx context.Context, op *operation, req wire.SpeechRequest, commit ContentCommitter, emit func([]byte)) wire.SpeechReply {
	transport, ok := op.host.NativeTransport()
	if !ok {
		return speechFailed(wire.ReplyOutcomeUnavailable, "native:transport")
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Credential = ""
	forwarded.Guarantees = localGuarantees(req.Guarantees)
	forwarded.Extensions, _ = forwardedNativeExtensions(req.Extensions, nil, op.subject.Program)
	client := wire.NewSpeechClient(transport.WithContext(ctx))
	admission, err := client.Start(forwarded)
	if err != nil {
		if ctx.Err() != nil {
			return speechFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		return speechFailed(wire.ReplyOutcomeUnavailable, "native:start_unknown")
	}
	cancelNative := func() { cancelNativeSpeech(transport, admission.Operation) }
	if cancelledAfterNativeStart(ctx, admission, func(string) { cancelNative() }) {
		return speechFailed(wire.ReplyOutcomeCancelled, "caller")
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		return speechFailed(nativeStartReply(admission.Outcome), "native:"+admission.Reason)
	}
	media := speechMediaType(req.Format)
	relayed := sha256.New()
	var size int64
	for cursor := int64(0); ; {
		page, err := client.Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancelNative()
			return speechFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil || page.Outcome != wire.PageOutcomePage || page.Next < cursor {
			cancelNative()
			return speechFailed(wire.ReplyOutcomeUnavailable, "native:unreachable")
		}
		for i, d := range page.Deltas {
			if d.Sequence != cursor+int64(i) {
				cancelNative()
				return speechFailed(wire.ReplyOutcomeUnavailable, "native:invalid_page")
			}
			switch {
			case d.Kind == wire.DeltaKindAudio && d.Audio != nil:
				if len(d.Audio.Data) == 0 || len(d.Audio.Data) > 65536 || size+int64(len(d.Audio.Data)) > p.cfg.MaxSpeechBytes {
					cancelNative()
					return speechFailed(wire.ReplyOutcomeUnavailable, "native:audio")
				}
				size += int64(len(d.Audio.Data))
				//unchecked: hash.Hash.Write never returns an error
				_, _ = relayed.Write(d.Audio.Data)
				emit(d.Audio.Data)
			case d.Kind == wire.DeltaKindEnd && d.SpeechEnd != nil:
				end := *d.SpeechEnd
				if !end.Outcome.Known() || len(end.Reason) > 4096 {
					return speechFailed(wire.ReplyOutcomeUnavailable, "native:terminal")
				}
				if end.Outcome != wire.ReplyOutcomeCompleted {
					return speechFailed(end.Outcome, end.Reason)
				}
				digest := "sha256:" + hex.EncodeToString(relayed.Sum(nil))
				if size == 0 || end.Delivery.Digest != digest || end.Delivery.Size != size || end.Delivery.MediaType != media || end.Delivery.Location != "local" {
					return speechFailed(wire.ReplyOutcomeUnavailable, "native:delivery")
				}
				audio, err := mediationcontent.Download(ctx, transport, digest, media, size, p.cfg.MaxSpeechBytes)
				if err != nil {
					if ctx.Err() != nil {
						return speechFailed(wire.ReplyOutcomeCancelled, "caller")
					}
					return speechFailed(wire.ReplyOutcomeUnavailable, "native:content")
				}
				return publishSpeech(ctx, audio, media, commit, op.beginCompletion)
			default:
				cancelNative()
				return speechFailed(wire.ReplyOutcomeUnavailable, "native:invalid_page")
			}
		}
		if page.AtEnd {
			cancelNative()
			return speechFailed(wire.ReplyOutcomeUnavailable, "native:terminal")
		}
		if len(page.Deltas) == 0 && !waitImagePoll(ctx, minimumImagePollDelay) {
			cancelNative()
			return speechFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		cursor = page.Next
	}
}

func nativeStartReply(outcome wire.StartOutcome) wire.ReplyOutcome {
	reply := replyFromStart(outcome)
	if !reply.Known() {
		return wire.ReplyOutcomeUnavailable
	}
	return reply
}

func cancelNativeTranscription(transport listen.FrameClient, operation string) {
	stop, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	//unchecked: best-effort cancel of an abandoned native operation; the caller already has its outcome
	_, _ = wire.NewTranscriptionClient(transport.WithContext(stop)).Cancel(operation)
}

func cancelNativeSpeech(transport listen.FrameClient, operation string) {
	stop, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	//unchecked: best-effort cancel of an abandoned native operation; the caller already has its outcome
	_, _ = wire.NewSpeechClient(transport.WithContext(stop)).Cancel(operation)
}
