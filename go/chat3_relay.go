package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

// A relay uses the same generated profile over its selected authenticated
// transport. The factory's context belongs to each call, never a public SDK
// remote transport or a caller-supplied authority claim.
type chat3RelayClient interface {
	GetHistoryWindow() (wire.ChatHistoryRead, error)
	Start(wire.RequestV3) (wire.ChatAdmission, error)
	Observe(string, int64, int64, int64, int64) (wire.ChatPage, error)
	Cancel(string) (wire.ChatCancellation, error)
}

func chat3RelayKey(subject Subject, identity acceptance.RequestIdentity) string {
	// A JSON tuple keeps independently bounded UTF-8 components unambiguous.
	//unchecked: a slice of strings always has a JSON encoding
	b, _ := json.Marshal([]string{subject.Account, subject.Program, identity.Key, identity.HistoryEpoch})
	digest := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// relayChat3 forwards a chat@3 operation to its selected native or remote
// provider. images are the input images this runtime resolved for a native
// provider; each is staged in that provider's content store before Start, so
// the provider reads the request's digests from its own store.
func (p *Provider) relayChat3(ctx context.Context, op *operation, legacy wire.Request, images map[string][]byte) wire.Reply {
	var transport listen.FrameClient
	var ok bool
	if op.host.Wire == router.WireRemote {
		transport, ok = op.host.RemoteTransport()
	} else {
		transport, ok = op.host.NativeTransport()
	}
	if !ok {
		return failed(wire.ReplyOutcomeUnavailable, "relay:transport")
	}
	if op.host.Wire == router.WireNative {
		if err := stageNativeImages(ctx, transport, op.id, legacy, images); err != nil {
			if ctx.Err() != nil {
				return failed(wire.ReplyOutcomeCancelled, "caller")
			}
			p.report(err)
			return failed(wire.ReplyOutcomeUnavailable, "native:input")
		}
	}
	return p.runChat3Relay(ctx, op, legacy, func(ctx context.Context) chat3RelayClient { return wire.NewChatV3Client(transport.WithContext(ctx)) })
}

func (p *Provider) runChat3Relay(ctx context.Context, op *operation, legacy wire.Request, client func(context.Context) chat3RelayClient) wire.Reply {
	forwarded := op.chat3.request
	forwarded.Target = wire.ChatTarget{Model: op.model}
	forwarded.Content.Credential = legacy.Credential
	forwarded.Content.Guarantees = nil
	guarantees := legacy.Guarantees
	if op.host.Wire == router.WireNative {
		guarantees = localGuarantees(guarantees)
	}
	for _, g := range guarantees {
		forwarded.Content.Guarantees = append(forwarded.Content.Guarantees, wire.ChatGuarantee(g))
	}
	if chat3NoRetention(op.chat3.request.Content.Guarantees) {
		forwarded.Content.Guarantees = append(forwarded.Content.Guarantees, wire.ChatGuaranteeNoRetention)
	}
	if chat3HasGuarantee(op.chat3.request, wire.ChatGuaranteeFreeCapacity) {
		forwarded.Content.Guarantees = append(forwarded.Content.Guarantees, wire.ChatGuaranteeFreeCapacity)
	}
	if op.host.Wire == router.WireRemote {
		forwarded.Content.Extensions, forwarded.Content.RequiredExtensions = forwardedRemoteExtensions(legacy.Extensions, legacy.RequiredExtensions, op.subject.Program)
	} else {
		forwarded.Content.Extensions, forwarded.Content.RequiredExtensions = forwardedNativeExtensions(legacy.Extensions, legacy.RequiredExtensions, op.subject.Program)
		forwarded.Content.Extensions = forwardedChatCacheExtensions(forwarded.Content.Extensions, op.cache)
	}
	epoch := p.chat3History.remoteEpoch(op.chat3.history)
	if epoch == "" {
		window, err := client(ctx).GetHistoryWindow()
		if err != nil || window.Outcome != wire.ChatHistoryOutcomeAvailable || window.Window == nil || window.Window.HistoryEpoch == "" {
			return failed(wire.ReplyOutcomeUnavailable, "remote:history")
		}
		epoch = window.Window.HistoryEpoch
		p.chat3History.bindRemoteEpoch(op.chat3.history, epoch)
	}
	forwarded.Identity = acceptance.RequestIdentity{Key: chat3RelayKey(op.subject, op.chat3.request.Identity), HistoryEpoch: epoch, Attempt: op.chat3.request.Identity.Attempt}
	op.mu.Lock()
	op.timing.relaySend(p.cfg.Now())
	op.mu.Unlock()
	a, err := client(ctx).Start(forwarded)
	if err != nil {
		return failed(wire.ReplyOutcomeUnavailable, remoteRequestReason(err))
	}
	if a.Outcome != wire.ChatStartOutcomeAccepted {
		cause := "remote:" + a.Outcome.String()
		if a.Outcome == wire.ChatStartOutcomeUnknown {
			cause = "remote:uncertain"
		}
		op.chat3.relayEnd = &wire.ChatResult{Outcome: wire.ChatResultOutcomeFailed, Cause: cause, RetryAfterMs: a.RetryAfterMs, Refusal: a.Refusal}
		return failed(wire.ReplyOutcomeUnavailable, cause)
	}
	cancel := func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := client(stop).Cancel(a.Request); err != nil {
			p.report(err)
		}
	}
	var cursor int64
	for {
		page, err := client(ctx).Observe(a.Request, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancel()
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
		// A failed wait, refused page or gap does not authorize Cancel or another
		// Start. The remote's existing idle owner bounds unobserved execution.
		if err != nil {
			return failed(wire.ReplyOutcomeUnavailable, "remote:uncertain")
		}
		if page.Outcome != wire.PageOutcomePage || !validChat3RelayPage(page, cursor) {
			return failed(wire.ReplyOutcomeUnavailable, "remote:uncertain")
		}
		for _, event := range page.Events {
			op.mu.Lock()
			op.timing.relayDelta(p.cfg.Now())
			op.mu.Unlock()
			if event.End != nil {
				op.chat3.relayEnd = event.End
				end := wire.Reply{Outcome: wire.ReplyOutcomeUnavailable, Reason: event.End.Cause, StopReason: wire.StopReasonNoStop, Cost: event.End.Cost}
				if event.End.Usage.Input != nil {
					end.Usage.Input = *event.End.Usage.Input
				}
				if event.End.Usage.Output != nil {
					end.Usage.Output = *event.End.Usage.Output
				}
				if event.End.Outcome == wire.ChatResultOutcomeCompleted {
					end.Outcome = wire.ReplyOutcomeCompleted
					end.StopReason = wire.StopReasonEnd
				} else if event.End.Outcome == wire.ChatResultOutcomeCancelled {
					end.Outcome = wire.ReplyOutcomeCancelled
				}
				return end
			}
			if err := op.emitChat3(event, p.cfg.Now()); err != nil {
				return failed(wire.ReplyOutcomeUnavailable, "bound:reply_bytes")
			}
		}
		cursor = page.Next
		if len(page.Events) == 0 && !idleRelay(ctx) {
			cancel()
			return failed(wire.ReplyOutcomeCancelled, "caller")
		}
	}
}

func validChat3RelayPage(page wire.ChatPage, cursor int64) bool {
	if page.Outcome != wire.PageOutcomePage || page.Next != cursor+int64(len(page.Events)) {
		return false
	}
	ended := false
	for i, event := range page.Events {
		if event.Sequence != cursor+int64(i) || ended {
			return false
		}
		payloads := 0
		for _, present := range []bool{event.Progress != nil, event.Part != nil, event.BlockEnd != nil, event.Usage != nil, event.End != nil} {
			if present {
				payloads++
			}
		}
		if payloads != 1 {
			return false
		}
		match := event.Kind == wire.ChatEventKindProgress && event.Progress != nil || event.Kind == wire.ChatEventKindPart && event.Part != nil || event.Kind == wire.ChatEventKindBlockEnd && event.BlockEnd != nil || event.Kind == wire.ChatEventKindUsage && event.Usage != nil || event.Kind == wire.ChatEventKindEnd && event.End != nil
		if !match {
			return false
		}
		ended = event.End != nil
	}
	return page.AtEnd == ended
}
