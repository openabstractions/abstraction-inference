package inference

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
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
// profile is the request's, which the hook reads by AdmissionProfile.
func (p *Provider) admitModel(ctx context.Context, host *router.Host, profile, model string, subject *Subject) (*modelActivity, string, bool, bool) {
	if ctx.Err() != nil {
		return nil, "caller", false, false
	}
	if p.ctx.Err() != nil {
		return nil, "provider:closed", false, false
	}
	if p.cfg.Admit == nil && subject == nil {
		return nil, "", true, false
	}
	activity, reserveReason := p.reserveModelActivity(model, subject)
	if activity == nil {
		return nil, reserveReason, false, reserveReason == "capacity"
	}
	if ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, "caller", false, false
	}
	if p.ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, "provider:closed", false, false
	}
	if reason, ok := p.admitHost(ctx, host, profile, model); !ok {
		p.releaseModelActivity(activity)
		return nil, reason, false, false
	}
	if ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, "caller", false, false
	}
	if p.ctx.Err() != nil {
		p.releaseModelActivity(activity)
		return nil, "provider:closed", false, false
	}
	return activity, "", true, false
}

// admitHost asks the provider's admission hook (INF-A5) about work for model
// on host under profile; a hook with no objection, or none, admits it. The
// hook is where a host's provider gets ready for work, a declared native
// provider's on-demand start among it (facade FAC-R8), so every path that
// sends work to a host asks here first: each Start through admitModel, and
// the job executor before any call reaches a job's host (INF-G5).
func (p *Provider) admitHost(ctx context.Context, host *router.Host, profile, model string) (string, bool) {
	if p.cfg.Admit == nil {
		return "", true
	}
	return p.cfg.Admit(WithAdmissionProfile(ctx, profile), host, model)
}

func (p *Provider) bindOperation(ctx context.Context, op *operation) {
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
	op.lastObserve = p.cfg.Now()
	op.mu.Unlock()
	p.ops[op.id] = op
	p.runs.Add(1)
	return wire.StartOutcomeAccepted, ""
}
