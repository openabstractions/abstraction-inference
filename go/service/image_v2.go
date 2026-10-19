package service

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

type imageV2Receiver struct{ *receiver }

func (r *imageV2Receiver) Start(request wire.ImageRequestV2) (wire.AdmissionV2, error) {
	subject, rung, peer := r.bound()
	return r.host.provider.StartImageV2(r.admittingContext(subject, rung, peer), subject, request), nil
}

func (r *imageV2Receiver) Observe(operation string, cursor, maxDeltas, maxBytes, waitMS int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveImageV2(r.call.WaitContext(), r.caller(), operation, cursor, maxDeltas, maxBytes, waitMS), nil
}

func (r *imageV2Receiver) Cancel(operation string) (wire.Cancellation, error) {
	return r.host.provider.CancelImageV2(r.caller(), operation), nil
}
