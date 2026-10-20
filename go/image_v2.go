package inference

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// ImageRequestFromContent preserves image@1 request semantics after selection.
func ImageRequestFromContent(model string, content wire.ImageRequestContent) wire.ImageRequest {
	return wire.ImageRequest{Model: model, Mode: content.Mode, Prompt: content.Prompt, Size: content.Size,
		Count: content.Count, ImageDigest: content.ImageDigest, ImageMediaType: content.ImageMediaType,
		MaskDigest: content.MaskDigest, MaskMediaType: content.MaskMediaType, Guarantees: content.Guarantees,
		Credential: content.Credential, RequiredExtensions: content.RequiredExtensions, Extensions: content.Extensions}
}

// StartImageV2 chooses a route before invoking the image@1 admission path.
// Once that path begins, its refusal or accepted operation is terminal.
func (p *Provider) StartImageV2(ctx context.Context, subject Subject, request wire.ImageRequestV2) wire.AdmissionV2 {
	if subject.Account == "" || subject.Program == "" {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeForbidden, Reason: "caller"}
	}
	// Every refusal of a proven caller is recorded, so the caller can read
	// why its request failed (logging LOG-P9).
	var tried string
	var last selectionNote
	refuse := func(outcome wire.StartOutcome, reason string) wire.AdmissionV2 {
		p.recordSelectionRefusal(ctx, subject, router.ProfileImage, tried, last, outcome, reason)
		return wire.AdmissionV2{Outcome: outcome, Reason: reason}
	}
	if !ValidateTarget(request.Target) {
		return refuse(wire.StartOutcomeInvalid, "target")
	}
	if request.Target.Model != "" {
		return admissionV2(p.StartImage(ctx, subject, ImageRequestFromContent(request.Target.Model, request.Content)), request.Target.Model, wire.SelectionReasonExplicitModel)
	}
	if request.Content.Credential != "" || len(request.Content.Guarantees) != 0 {
		return refuse(wire.StartOutcomeInvalid, "selection:override")
	}
	if reason := validateImageRequest(ImageRequestFromContent("selection-validation", request.Content)); reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason)
	}
	if p.cfg.ResolveSelectionV2 == nil {
		return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
	}
	selectionCtx, cancel := context.WithTimeout(ctx, selectionBudget)
	defer cancel()
	policy, found, err := p.resolveSelection(selectionCtx, subject, request.Target.SelectionPolicy)
	if err != nil || selectionCtx.Err() != nil {
		return refuse(wire.StartOutcomeUnavailable, "selection:unavailable")
	}
	if !found {
		return refuse(wire.StartOutcomeNoHost, "selection:unknown")
	}
	if policy.Name != request.Target.SelectionPolicy || len(policy.Candidates) < 1 || len(policy.Candidates) > 8 {
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
		req := ImageRequestFromContent(candidate.Model, request.Content)
		if candidate.Placement == wire.CandidatePlacementHosted {
			req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}
			req.Credential = candidate.Credential
		} else {
			req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
		}
		if reason := validateImageRequest(req); reason != "" {
			return refuse(wire.StartOutcomeInvalid, reason)
		}
		host, model, routeReason, unavailable := p.pickCandidate(selectionCtx, candidate, router.ProfileImage, i == 0)
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
		return admissionV2(p.startImage(selectionCtx, subject, req, &selectedRoute{host: host, model: model, bindingID: host.BindingID, note: note}), preferred.preferred, note.reason)
	}
	return refuse(wire.StartOutcomeNoHost, "selection:no_route")
}

func (p *Provider) ObserveImageV2(ctx context.Context, subject Subject, id string, cursor, maxDeltas, maxBytes, waitMS int64) wire.DeltaPage {
	return p.ObserveImage(ctx, subject, id, cursor, maxDeltas, maxBytes, waitMS)
}

func (p *Provider) CancelImageV2(subject Subject, id string) wire.Cancellation {
	return p.CancelImage(subject, id)
}
