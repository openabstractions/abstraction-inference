package service

import "context"

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

type imageV2Receiver struct{ *receiver }

func (r *imageV2Receiver) Start(request wire.ImageRequestV2) (wire.AdmissionV2, error) {
	return r.start(request, nil)
}

func (r *imageV2Receiver) StartWithGuarantees(request wire.ImageRequestV2, guarantees []wire.RequestGuarantee) (wire.AdmissionV2, error) {
	return r.start(request, guarantees)
}

func (r *imageV2Receiver) start(request wire.ImageRequestV2, guarantees []wire.RequestGuarantee) (wire.AdmissionV2, error) {
	subject, rung, peer := r.bound()
	var result wire.AdmissionV2
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartImageV2WithGuarantees(ctx, subject, request, guarantees)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.AdmissionV2{Outcome: wire.StartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

func (r *imageV2Receiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveImageV2(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *imageV2Receiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelImageV2(r.caller(), operation), nil
}
