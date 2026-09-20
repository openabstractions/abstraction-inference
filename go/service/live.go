package service

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

type liveReceiver struct{ *receiver }

func (r *liveReceiver) Start(q wire.LiveRequest) (wire.Admission, error) {
	s, rung := r.bound()
	return r.host.provider.StartLive(r.admittingContext(rung), s, q), nil
}
func (r *liveReceiver) Append(id string, seq int64, data []byte) (wire.LiveInputResult, error) {
	return r.host.provider.AppendLive(r.ctx, r.caller(), id, seq, data), nil
}
func (r *liveReceiver) Observe(id string, cursor, n, size, wait int64) (wire.DeltaPage, error) {
	return r.host.provider.ObserveLive(r.call.WaitContext(), r.caller(), id, cursor, n, size, wait), nil
}
func (r *liveReceiver) Commit(id string) (wire.LiveInputResult, error) {
	return r.host.provider.CommitLive(r.ctx, r.caller(), id), nil
}
func (r *liveReceiver) Cancel(id string) (wire.Cancellation, error) {
	return r.host.provider.CancelLive(r.caller(), id), nil
}

type remoteLive struct{ *remoteChat }

func (r *remoteLive) Start(q wire.LiveRequest) (wire.Admission, error) {
	return r.provider.StartLive(r.ctx, r.subject, q), nil
}
func (r *remoteLive) Append(id string, seq int64, data []byte) (wire.LiveInputResult, error) {
	return r.provider.AppendLive(r.ctx, r.subject, id, seq, data), nil
}
func (r *remoteLive) Observe(id string, cursor, n, size, wait int64) (wire.DeltaPage, error) {
	return r.provider.ObserveLive(r.ctx, r.subject, id, cursor, n, size, wait), nil
}
func (r *remoteLive) Commit(id string) (wire.LiveInputResult, error) {
	return r.provider.CommitLive(r.ctx, r.subject, id), nil
}
func (r *remoteLive) Cancel(id string) (wire.Cancellation, error) {
	return r.provider.CancelLive(r.subject, id), nil
}
