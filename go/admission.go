package inference

import (
	"context"
	"github.com/openabstractions/abstraction-identity/remote"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	watch "github.com/openabstractions/abstraction-watch/go"
)

type admissionProfileKey struct{}

// AdmissionProfile is the profile of the request an Admitter is asked about
// (INF-A5): chat, embed, transcription, speech, image or live. A hook checks
// it against what its server declares for the model (ROUTE-D9). It is ""
// outside an admission.
func AdmissionProfile(ctx context.Context) string {
	profile, _ := ctx.Value(admissionProfileKey{}).(string)
	return profile
}

// WithAdmissionProfile marks ctx with the profile an Admitter is asked about.
// The provider sets it on every admission; a test of a hook sets it itself.
func WithAdmissionProfile(ctx context.Context, profile string) context.Context {
	return context.WithValue(ctx, admissionProfileKey{}, profile)
}

// admitModel holds model activity across the provider's potentially
// side-effecting admission hook and operation registration. Call only after
// request, authority, content, budget and credential preflights have passed.
// profile is the request's, which the hook reads by AdmissionProfile. The
// typed outcome preserves authority refusal separately from a provider hook's
// existing refusal-word mapping.
func (p *Provider) admitModel(ctx context.Context, host *router.Host, profile, model string, subject *Subject, authority Subject, credential string) (*modelActivity, wire.StartOutcome, string) {
	if ctx.Err() != nil {
		return nil, wire.StartOutcomeUnavailable, "caller"
	}
	if p.ctx.Err() != nil {
		return nil, wire.StartOutcomeUnavailable, "provider:closed"
	}
	// Earlier preflights may wait for permission. Re-read complete and the
	// applied credential quietly before the provider's side-effecting hook.
	// Reservation accounting still uses subject; Embed intentionally has none.
	if host.Wire == router.WireRemote {
		credential = ""
	}
	if refusal, err := p.standing(ctx, authority, host.Name, credential); err != nil {
		return nil, wire.StartOutcomeUnavailable, "rights:unavailable"
	} else if refusal != "" {
		return nil, wire.StartOutcomeNotPermitted, refusal
	}
	if p.cfg.Admit == nil && subject == nil {
		return nil, wire.StartOutcomeAccepted, ""
	}
	activity, reserveReason := p.reserveModelActivity(model, subject)
	if activity == nil {
		if reserveReason == "capacity" {
			return nil, wire.StartOutcomeExhausted, reserveReason
		}
		return nil, wire.StartOutcomeUnavailable, reserveReason
	}
	if ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeUnavailable, "caller"
	}
	if p.ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeUnavailable, "provider:closed"
	}
	if reason, ok := p.admitHost(ctx, host, profile, model); !ok {
		p.releaseModelActivity(activity)
		return nil, admitStartOutcome(reason), reason
	}
	if ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeUnavailable, "caller"
	}
	if p.ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeUnavailable, "provider:closed"
	}
	// Readiness may wait without an operation for Recheck to observe. Read
	// authority again before the caller can accept or send upstream work.
	if refusal, err := p.standing(ctx, authority, host.Name, credential); err != nil {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeUnavailable, "rights:unavailable"
	} else if refusal != "" {
		p.releaseModelActivity(activity)
		return nil, wire.StartOutcomeNotPermitted, refusal
	}
	return activity, wire.StartOutcomeAccepted, ""
}

// admitHost asks the provider's admission hook (INF-A5) about work for model
// on host under profile; a hook with no objection, or none, admits it. The
// hook is where a host's provider gets ready for work, a declared native
// provider's on-demand start among it (facade FAC-R8), so every path that
// sends work to a host asks here first: each Start through admitModel, and
// the job executor before any call reaches a job's host (INF-G5).
func (p *Provider) admitHost(ctx context.Context, host *router.Host, profile, model string) (string, bool) {
	if FreeCapacityAdmission(ctx) && host.Wire != router.WireNative && host.Wire != router.WireRemote && !p.supportsFreeCapacity() {
		return "guarantee:free-capacity", false
	}
	if p.cfg.Admit == nil {
		return "", true
	}
	deadline, ok := ctx.Deadline()
	if reply, set := ctx.Value(replyDeadlineKey{}).(time.Time); set && (!ok || reply.Before(deadline)) {
		deadline, ok = reply, true
	}
	if ok {
		if time.Until(deadline) > admitReplyMargin {
			deadline = deadline.Add(-admitReplyMargin)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return p.cfg.Admit(WithAdmissionProfile(ctx, profile), host, model)
}

// StartFailedPrefix begins an admission or routing reason word for a
// provider that stopped after failed starts: start_failed:<name>. It is a
// definite refusal; a job that reads it fails at once, and a later request
// reads it from the router until the provider starts again.
const StartFailedPrefix = "start_failed:"

type replyDeadlineKey struct{}

// WithReplyDeadline names when the caller stops listening for this call's
// answer, where the call's own context lives longer. A native frame carries
// no client deadline: the endpoint's call context lasts its whole connection
// budget, while every generated client gives Start the default call budget.
// Admission hooks then answer by this deadline less admitReplyMargin (R28: a
// launch wait ran to the connection budget, the client had hung up at five
// seconds, and the client read the definite activating refusal as a lost call).
func WithReplyDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, replyDeadlineKey{}, deadline)
}

// admitReplyMargin is the part of a caller's deadline an admission hook leaves
// for its answer to reach the caller. A hook that waits for a launch until the
// caller's own deadline answers when the caller has already hung up, and a
// definite refusal such as unavailable activating:<name> then reads as a lost
// call. A deadline closer than the margin is left to the hook unchanged.
const admitReplyMargin = 1500 * time.Millisecond

func (p *Provider) bindOperation(ctx context.Context, op *operation) {
	if routeOf(ctx).name == RouteRemote || remote.ValidSubjectProgram(op.subject.Program) {
		op.remoteValues = context.WithoutCancel(ctx)
	}
	if binding, pinned := ctx.Value(executionBindingKey{}).(ExecutionBinding); pinned {
		op.bindingID = binding.BindingID
	}
}

// admitOperationWithActivity transfers a pending model admission into the
// operation map under the same lock. ActiveForModel therefore sees one or the
// other without a gap while the operation begins.
func (p *Provider) admitOperationWithActivity(ctx context.Context, op *operation, activity *modelActivity) (wire.StartOutcome, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ctx.Err() != nil {
		return wire.StartOutcomeUnavailable, "caller"
	}
	if p.ctx.Err() != nil {
		return wire.StartOutcomeUnavailable, "provider:closed"
	}
	active := 0
	for _, existing := range p.ops {
		if existing.subject == op.subject && !existing.isEnded() {
			active++
		}
	}
	if active >= p.cfg.MaxOperations {
		return wire.StartOutcomeExhausted, "capacity"
	}
	p.releaseModelActivityLocked(activity)
	// The caller can observe only after admission returns its operation ID.
	// Preserve started for wall metrics while arming idle at registration.
	op.mu.Lock()
	if p.observe == nil {
		p.observe = watch.NewGate(watch.DefaultWaiters)
	}
	op.observe = p.observe
	op.lastObserve = p.cfg.Now()
	op.mu.Unlock()
	p.ops[op.id] = op
	p.runs.Add(1)
	return wire.StartOutcomeAccepted, ""
}
