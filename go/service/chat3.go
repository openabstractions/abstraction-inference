package service

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// chatV3Receiver uses the same attested boundary and first-use wait owner.
// Registration stays explicit in host.go; this receiver alone offers no profile.
type chatV3Receiver struct{ *receiver }

func (r *chatV3Receiver) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	return r.host.provider.GetChatHistoryWindow(r.caller()), nil
}

func (r *chatV3Receiver) Start(request wire.RequestV3) (wire.ChatAdmission, error) {
	r.host.admission.RLock()
	defer r.host.admission.RUnlock()
	if r.host.retired {
		return wire.ChatAdmission{Outcome: wire.ChatStartOutcomeNoHost, Reason: "binding:retired"}, nil
	}
	subject, rung, peer := r.bound()
	var result wire.ChatAdmission
	err := r.awaitAdmission(subject, rung, peer, func(ctx context.Context) (string, *string) {
		result = r.host.provider.StartV3(ctx, subject, request)
		return result.Outcome.String(), &result.Reason
	})
	if err != nil {
		return wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnavailable, Reason: "admission:unavailable"}, nil
	}
	return result, nil
}

func (r *chatV3Receiver) Observe(request string, cursor, maxEvents, maxBytes, waitMs int64) (wire.ChatPage, error) {
	return r.host.provider.ObserveV3(r.call.WaitContext(), r.caller(), request, cursor, maxEvents, maxBytes, waitMs), nil
}

func (r *chatV3Receiver) Cancel(request string) (wire.ChatCancellation, error) {
	return r.host.provider.CancelV3(r.caller(), request), nil
}

type remoteChatV3 struct{ *remoteChat }

func (r *remoteChatV3) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	return r.provider.GetChatHistoryWindow(r.subject), nil
}
func (r *remoteChatV3) Start(request wire.RequestV3) (wire.ChatAdmission, error) {
	return r.provider.StartV3(r.ctx, r.subject, request), nil
}
func (r *remoteChatV3) Observe(request string, cursor, maxEvents, maxBytes, waitMs int64) (wire.ChatPage, error) {
	return r.provider.ObserveV3(r.ctx, r.subject, request, cursor, maxEvents, maxBytes, waitMs), nil
}
func (r *remoteChatV3) Cancel(request string) (wire.ChatCancellation, error) {
	return r.provider.CancelV3(r.subject, request), nil
}

// DescribeMetadata states the provider's model capabilities for Describe; it
// grants nothing and changes no admission.
func (r *chatV3Receiver) DescribeMetadata() ([]string, map[string]string) {
	if r.host == nil || r.host.ModelCapabilities == nil {
		return nil, nil
	}
	names, caps := r.host.ModelCapabilities()
	return nil, router.CapabilityFacts(names, caps)
}
