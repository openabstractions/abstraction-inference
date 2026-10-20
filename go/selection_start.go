package inference

import (
	"context"
	"slices"
	"strings"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const selectionBudget = 30 * time.Second

// StartV2 resolves one explicit target or a service-owned policy, then uses
// the same admission and operation registry as chat@1. Selection ends before
// admission begins; no result of start or dispatch can trigger another route.
func (p *Provider) StartV2(ctx context.Context, subject Subject, request wire.RequestV2) wire.AdmissionV2 {
	if subject.Account == "" || subject.Program == "" {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeForbidden, Reason: "caller"}
	}
	// Every refusal of a proven caller is recorded, so the caller can read
	// why its request failed (logging LOG-P9).
	var tried string
	var last selectionNote
	refuse := func(outcome wire.StartOutcome, reason string) wire.AdmissionV2 {
		p.recordSelectionRefusal(ctx, subject, "", tried, last, outcome, reason)
		return wire.AdmissionV2{Outcome: outcome, Reason: reason}
	}
	if !ValidateTarget(request.Target) {
		return refuse(wire.StartOutcomeInvalid, "target")
	}
	if request.Target.Model != "" {
		req := RequestFromContent(request.Target.Model, request.Content)
		return admissionV2(p.Start(ctx, subject, req), request.Target.Model, wire.SelectionReasonExplicitModel)
	}
	if request.Content.Credential != "" || len(request.Content.Guarantees) != 0 {
		return refuse(wire.StartOutcomeInvalid, "selection:override")
	}
	if reason := validate(RequestFromContent("selection-validation", request.Content)); reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason)
	}
	if p.cfg.ResolveSelectionV2 == nil {
		return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
	}
	selectionCtx, cancel := context.WithTimeout(ctx, selectionBudget)
	defer cancel()
	policy, found, err := p.resolveSelection(selectionCtx, subject, request.Target.SelectionPolicy)
	if err != nil {
		return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
	}
	if selectionCtx.Err() != nil {
		return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
	}
	if !found {
		return refuse(wire.StartOutcomeNoHost, "selection:unknown")
	}
	if policy.Name != request.Target.SelectionPolicy || len(policy.Candidates) == 0 || len(policy.Candidates) > 8 {
		return refuse(wire.StartOutcomeUnavailable, "selection:invalid")
	}
	preferred := selectionNote{preferred: policy.Candidates[0].Model}
	for i, candidate := range policy.Candidates {
		tried = candidate.Model
		if selectionCtx.Err() != nil {
			return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
		}
		if !validSelectionCandidate(candidate) {
			return refuse(wire.StartOutcomeUnavailable, "selection:invalid")
		}
		req := RequestFromContent(candidate.Model, request.Content)
		if candidate.Placement == wire.CandidatePlacementHosted {
			req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}
			req.Credential = candidate.Credential
		} else {
			req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
			req.Credential = ""
		}
		if reason := validate(req); reason != "" {
			return refuse(wire.StartOutcomeInvalid, reason)
		}
		host, model, routeReason, unavailable := p.pickCandidate(selectionCtx, candidate, router.ProfileChat, i == 0)
		if selectionCtx.Err() != nil {
			return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
		}
		note := preferred.candidate(i, host, unavailable)
		if host == nil {
			if len(unavailable) != 0 && i+1 < len(policy.Candidates) {
				preferred = note
				// A refusal before picking the next candidate is already a fallback.
				last = note.candidate(i+1, nil, nil)
				continue
			}
			last = note
			return refuse(wire.StartOutcomeNoHost, routeReason)
		}
		return admissionV2(p.start(selectionCtx, subject, req, &selectedRoute{host: host, model: model, bindingID: host.BindingID, note: note}), preferred.preferred, note.reason)
	}
	return refuse(wire.StartOutcomeNoHost, "selection:no_route")
}

// candidate is the note for policy candidate i. The preferred candidate (i 0)
// names the host chosen for it, or the hosts known unavailable for it; a later
// candidate keeps those hosts and records the fallback.
func (n selectionNote) candidate(i int, host *router.Host, unavailable []string) selectionNote {
	if i > 0 {
		n.reason = wire.SelectionReasonPreferredRouteUnavailable
		return n
	}
	n.reason = wire.SelectionReasonPreferred
	if host != nil {
		n.preferredHost = host.Name
	} else {
		n.preferredHost = strings.Join(unavailable, ",")
	}
	return n
}

func admissionV2(a wire.Admission, preferred string, reason wire.SelectionReason) wire.AdmissionV2 {
	out := wire.AdmissionV2{Outcome: a.Outcome, Reason: a.Reason, Operation: a.Operation, Host: a.Host, Model: a.Model,
		RetentionMs: a.RetentionMs, IdleMs: a.IdleMs, RetainedDeltas: a.RetainedDeltas}
	if a.Outcome == wire.StartOutcomeAccepted {
		out.PreferredModel = preferred
		out.SelectionReason = &reason
	}
	return out
}

// recordSelectionRefusal records a selection refusal: model is the candidate
// last tried, empty before any candidate. A refusal after a fallback keeps the fallback note; a
// refusal of the preferred candidate itself selected nothing.
func (p *Provider) recordSelectionRefusal(ctx context.Context, subject Subject, profile, model string, note selectionNote, outcome wire.StartOutcome, reason string) {
	via := routeOf(ctx)
	if note.reason != wire.SelectionReasonPreferredRouteUnavailable {
		note.reason = 0
	}
	r := Record{Profile: profile, Route: via.name, Rung: via.rung, Account: subject.Account, Program: subject.Program,
		Model: model, Family: familyOf(model), Outcome: outcome.String(), Reason: reason,
		Domain: via.domain, Claim: via.claim}
	if state, _ := ctx.Value(chat3RunKey{}).(*chat3Run); state != nil {
		r.ChatVersion = 3
	}
	note.apply(&r)
	p.record(r)
}

// pickCandidate uses the selection's fresh router snapshot and restricts both
// placement and hosted credential. HostAllowance remains the router's boundary.
// Without a host it names the eligible hosts known transport-unavailable; only
// a nonempty list, with no other refusal cause, permits fallback.
func (p *Provider) pickCandidate(ctx context.Context, candidate wire.SelectionCandidateV2, profile string, fresh bool) (*router.Host, string, string, []string) {
	local := candidate.Placement == wire.CandidatePlacementLocal
	placement := executionPlacementOf(ctx)
	binding, pinned := ctx.Value(executionBindingKey{}).(ExecutionBinding)
	if local && placement == ExecutionRemote || !local && placement == ExecutionLocal {
		return nil, "", "selection:placement", nil
	}
	allowed := []string{}
	byName := map[string]*router.Host{}
	seenPin := false
	for _, host := range p.cfg.Router.Hosts() {
		if candidate.Provider != "" && host.Name == candidate.Provider {
			seenPin = true
			if host.BindingID == "" {
				return nil, "", "selection:provider_stale", nil
			}
		}
		if candidate.Provider != "" && host.Name != candidate.Provider {
			continue
		}
		if !slices.Contains(host.HostProfiles(), profile) {
			if candidate.Provider != "" {
				return nil, "", "selection:profile", nil
			}
			continue
		}
		if !host.Servable() || host.Hosted == local {
			continue
		}
		if pinned && (host.Name != binding.Host || host.BindingID == "" || host.BindingID != binding.BindingID) {
			continue
		}
		if leaf, _ := ctx.Value(leafExecutionKey{}).(bool); leaf && (host.Wire == router.WireNative || host.Wire == router.WireRemote) {
			continue
		}
		if !local && host.Credential != candidate.Credential {
			continue
		}
		if byName[host.Name] != nil {
			return nil, "", "selection:ambiguous_host", nil
		}
		allowed = append(allowed, host.Name)
		byName[host.Name] = host
	}
	if candidate.Provider != "" && (!seenPin || len(allowed) == 0) {
		return nil, "", "selection:provider_stale", nil
	}
	if len(allowed) == 0 {
		return nil, "", "selection:no_eligible_host", nil
	}
	p.survey.Lock()
	decision, observedAt, err := p.cfg.Router.RouteContext(ctx, router.Request{Model: candidate.Model, Fresh: fresh, Hosts: &allowed, Profile: profile})
	p.survey.Unlock()
	if err != nil {
		return nil, "", "selection:unavailable", nil
	}
	switch decision.Verdict {
	case router.Resident, router.WouldLoad, router.Hosted:
		if host := byName[decision.Host]; host != nil {
			return host, decision.Model, "", nil
		}
	}
	states, _, _, _, statesAt := p.cfg.Router.Residency(false)
	if !statesAt.Equal(observedAt) {
		return nil, "", "router:" + decision.Verdict, nil
	}
	if reason := unverifiedEngineReason(decision, states, byName, profile); reason != "" {
		// Missing native proof never grants the transport-outage fallback.
		return nil, "", reason, nil
	}
	var unavailable []string
	for _, state := range states {
		if byName[state.Host] == nil || state.Up {
			continue
		}
		if state.AvailabilityCause == router.AvailabilityTransportUnavailable {
			unavailable = append(unavailable, state.Host)
		} else {
			// A known credential, rights, configuration or malformed refusal,
			// and an unknown cause, can never authorize fallback.
			return nil, "", "router:" + decision.Verdict, nil
		}
	}
	slices.Sort(unavailable)
	return nil, "", "router:" + strings.TrimSpace(decision.Verdict), unavailable
}
