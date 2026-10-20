package inference

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// callerGuarantees intersects a caller restriction with the endpoint's fixed
// placement. It never replaces a remote endpoint's policy or grants hosted use.
func callerGuarantees(ctx context.Context, guarantees, existing []wire.RequestGuarantee, credential string) (context.Context, string) {
	if len(guarantees) == 0 {
		return ctx, ""
	}
	if len(guarantees) != 1 || guarantees[0] != wire.RequestGuaranteeLocalOnly {
		return ctx, "guarantees:caller"
	}
	if executionPlacementOf(ctx) == ExecutionRemote || credential != "" {
		return ctx, "guarantees:conflict"
	}
	for _, guarantee := range existing {
		if guarantee != wire.RequestGuaranteeLocalOnly {
			return ctx, "guarantees:conflict"
		}
	}
	return WithExecutionPlacement(ctx, ExecutionLocal), ""
}

func (p *Provider) StartV2WithGuarantees(ctx context.Context, subject Subject, request wire.RequestV2, guarantees []wire.RequestGuarantee) wire.AdmissionV2 {
	ctx, reason := callerGuarantees(ctx, guarantees, request.Content.Guarantees, request.Content.Credential)
	if reason != "" {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeInvalid, Reason: reason}
	}
	return p.StartV2(ctx, subject, request)
}

func (p *Provider) StartImageV2WithGuarantees(ctx context.Context, subject Subject, request wire.ImageRequestV2, guarantees []wire.RequestGuarantee) wire.AdmissionV2 {
	ctx, reason := callerGuarantees(ctx, guarantees, request.Content.Guarantees, request.Content.Credential)
	if reason != "" {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeInvalid, Reason: reason}
	}
	return p.StartImageV2(ctx, subject, request)
}
