package inference

import (
	"context"
	"encoding/json"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// PreloadContract is the service through which an application has its own
// chat request prefix evaluated before its next request (INF-PL1..INF-PL4).
const PreloadContract = "abstraction.inference/preload@1"

// recordProfilePreload names a preload in log records; it is no router
// profile: the host is chosen as for chat.
const recordProfilePreload = "preload"

// ReasonResourcesHeld is the busy reason of a preload whose model is not
// loaded and whose load would ask another holder to yield (INF-PL2).
const ReasonResourcesHeld = "resources:held"

type preloadAdmissionKey struct{}

// PreloadAdmission reports whether ctx is the admission of a preload. A
// provider's Admit hook reads it to load only without asking another holder
// to yield: a preload is speculative work and holds the lowest standing
// (INF-PL2). It answers ReasonResourcesHeld instead.
func PreloadAdmission(ctx context.Context) bool {
	marked, _ := ctx.Value(preloadAdmissionKey{}).(bool)
	return marked
}

// PrefixEvaluator has host, a server this provider sends to itself, evaluate
// the prefix body for model and reports the outcome, its reason and the
// tokens evaluated. body is the engine request the chat path makes of the
// prefix (openAIBody): the request's messages, tools and rendered fields.
// The evaluator keeps nothing of it after the call (INF-PL3).
type PrefixEvaluator func(ctx context.Context, host *router.Host, model string, body []byte) (wire.PreloadOutcome, string, int64)

// validatePreload returns the reason a preload request is invalid, or "": a
// chat@1 request whose last message is not the user's, because the caller's
// next request is these messages followed by its own (INF-PL1). An image part
// is refused: a prefix is the text a conversation opens with.
func validatePreload(req wire.Request) string {
	if reason := validate(req); reason != "" {
		return reason
	}
	if req.Messages[len(req.Messages)-1].Role == wire.RoleUser {
		return "messages:last_user"
	}
	if len(imagePartDigests(req)) > 0 {
		return "part:image"
	}
	return ""
}

// Preload admits one preload for subject in chat@1 Start's order and has the
// chosen host's server evaluate the request as a prefix (INF-PL2). Only a
// server this provider evaluates prefixes for, or a declared native provider
// that serves preload@1, takes one; every other host answers
// unsupported_feature.
func (p *Provider) Preload(ctx context.Context, subject Subject, req wire.Request) wire.PreloadReply {
	started := p.cfg.Now()
	via := routeOf(ctx)
	extensions := separateExtensions(ctx, req.Extensions, req.RequiredExtensions)
	via.claim = extensions.claim
	var host *router.Host
	model := ""
	finish := func(reply wire.PreloadReply) wire.PreloadReply {
		served := reply.Outcome == wire.PreloadOutcomeEvaluated || reply.Outcome == wire.PreloadOutcomeLoaded ||
			reply.Outcome == wire.PreloadOutcomeBusy && reply.Reason != ReasonResourcesHeld
		if !served {
			reply.Host, reply.Model = "", ""
		}
		if reply.Outcome != wire.PreloadOutcomeEvaluated {
			reply.Tokens = 0
		}
		r := Record{Profile: recordProfilePreload, Route: via.name, Rung: via.rung, Account: subject.Account, Program: subject.Program,
			Model: req.Model, Family: familyOf(req.Model), Credential: req.Credential, Outcome: reply.Outcome.String(), Reason: reply.Reason,
			TokensIn: reply.Tokens, WallMS: p.cfg.Now().Sub(started).Milliseconds(), Domain: via.domain, Claim: via.claim}
		if host != nil {
			r.Host, r.Model = host.Name, model
		}
		p.record(r)
		return reply
	}
	refuse := func(outcome wire.PreloadOutcome, reason string) wire.PreloadReply {
		return finish(wire.PreloadReply{Outcome: outcome, Reason: reason})
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.PreloadOutcomeForbidden, "caller")
	}
	if reason := validatePreload(req); reason != "" {
		return refuse(wire.PreloadOutcomeInvalid, reason)
	}
	if extensions.required != "" {
		return refuse(wire.PreloadOutcomeUnsupportedFeature, "extension:"+extensions.required)
	}
	req.Extensions = extensions.vendor
	var reason string
	host, model, reason = p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileChat)
	if host == nil {
		return refuse(wire.PreloadOutcomeNoHost, reason)
	}
	native := host.Wire == router.WireNative
	switch {
	case native && !host.Preloads:
		return refuse(wire.PreloadOutcomeUnsupportedFeature, "preload")
	case native:
	case host.Hosted || host.Wire == router.WireRemote:
		// A hosted server and a remote runtime keep their caches to
		// themselves; neither is asked to evaluate a prefix.
		return refuse(wire.PreloadOutcomeUnsupportedFeature, "preload")
	case p.cfg.EvaluatePrefix == nil || (host.Wire != "" && host.Wire != router.WireOpenAICompatible):
		return refuse(wire.PreloadOutcomeUnsupportedFeature, "preload")
	}
	if reason := (openAI{}).unsupported(req); reason != "" {
		return refuse(wire.PreloadOutcomeUnsupportedFeature, reason)
	}
	if native {
		if reason := unsupportedSelectedExtensions(host, router.ProfileChat, req.RequiredExtensions); reason != "" {
			return refuse(wire.PreloadOutcomeUnsupportedFeature, reason)
		}
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		p.report(err)
		return refuse(wire.PreloadOutcomeUnavailable, "rights:unavailable")
	}
	switch word {
	case "permitted":
	case "denied", "not_granted", "unknown_action":
		return refuse(wire.PreloadOutcomeNotPermitted, "rights:"+word)
	default:
		return refuse(wire.PreloadOutcomeUnavailable, "rights:unavailable")
	}
	// A preload reserves no model activity, for its load or its evaluation
	// (INF-PL2): it is speculative work, so neither an idle clock nor a
	// yield ever waits for it. The admission is marked so the provider's
	// Admit loads only without asking another holder to yield.
	if ctx.Err() != nil {
		return refuse(wire.PreloadOutcomeUnavailable, "caller")
	}
	if p.ctx.Err() != nil {
		return refuse(wire.PreloadOutcomeUnavailable, "provider:closed")
	}
	if admitReason, ok := p.admitHost(context.WithValue(ctx, preloadAdmissionKey{}, true), host, router.ProfileChat, model); !ok {
		switch admitReason {
		case ReasonResourcesHeld:
			return refuse(wire.PreloadOutcomeBusy, admitReason)
		case "not_permitted":
			return refuse(wire.PreloadOutcomeNotPermitted, admitReason)
		}
		return refuse(wire.PreloadOutcomeUnavailable, admitReason)
	}
	if ctx.Err() != nil {
		return refuse(wire.PreloadOutcomeUnavailable, "caller")
	}
	var reply wire.PreloadReply
	if native {
		reply = nativePreload(ctx, host, model, req, subject)
	} else {
		raw, err := json.Marshal(openAIBody(model, req, nil))
		if err != nil {
			return refuse(wire.PreloadOutcomeInvalid, "upstream:encoding")
		}
		outcome, reason, tokens := p.cfg.EvaluatePrefix(ctx, host, model, raw)
		reply = wire.PreloadReply{Outcome: outcome, Reason: reason, Tokens: tokens}
	}
	if !reply.Outcome.Known() {
		reply = wire.PreloadReply{Outcome: wire.PreloadOutcomeUnavailable, Reason: "upstream:outcome"}
	}
	reply.Host, reply.Model = host.Name, model
	return finish(reply)
}

// nativePreload delegates preload@1 to a declared local provider's own OA
// endpoint over the identity-bound native transport, as nativeEmbed does:
// the request travels with the chosen model name and its vendor extensions,
// and the claim names subject, the bound original caller.
func nativePreload(ctx context.Context, host *router.Host, model string, req wire.Request, subject Subject) wire.PreloadReply {
	transport, ok := host.NativeTransport()
	if !ok {
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeUnavailable, Reason: "native:transport"}
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Extensions, forwarded.RequiredExtensions = forwardedNativeExtensions(req.Extensions, req.RequiredExtensions, subject.Program)
	result, err := wire.NewPreloaderClient(transport.WithContext(ctx)).Preload(forwarded)
	if err != nil {
		return wire.PreloadReply{Outcome: wire.PreloadOutcomeUnavailable, Reason: "native:unreachable"}
	}
	switch result.Outcome {
	case wire.PreloadOutcomeEvaluated, wire.PreloadOutcomeLoaded, wire.PreloadOutcomeBusy:
		return wire.PreloadReply{Outcome: result.Outcome, Reason: result.Reason, Tokens: result.Tokens}
	}
	outcome := result.Outcome
	if !outcome.Known() {
		outcome = wire.PreloadOutcomeUnavailable
	}
	return wire.PreloadReply{Outcome: outcome, Reason: "native:" + result.Reason}
}
