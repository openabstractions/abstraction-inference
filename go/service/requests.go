package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// RequestOperator adds request metadata without changing custom Operator hosts.
type RequestOperator interface {
	Requests(context.Context, inference.Subject, int64, int64) wire.RequestPage
}

func (r *operatorReceiver) Requests(cursor, maxEntries int64) (wire.RequestPage, error) {
	refused := wire.RequestPage{Outcome: wire.AuditOutcomeUnavailable, Entries: []wire.RequestEntry{}, Next: cursor}
	owner, ok := r.host.Operator.(RequestOperator)
	if !ok {
		return refused, nil
	}
	page := owner.Requests(r.ctx, r.caller(), cursor, maxEntries)
	return requestPage(page, cursor, maxEntries)
}

func requestPage(page wire.RequestPage, cursor, maxEntries int64) (wire.RequestPage, error) {
	refused := wire.RequestPage{Outcome: wire.AuditOutcomeUnavailable, Entries: []wire.RequestEntry{}, Next: cursor}
	// The generated dispatcher formats the actual reply, including its envelope.
	if _, err := wire.NewOperatorClient(&requestReplyCapture{page: page}).Requests(cursor, maxEntries); err != nil {
		return refused, nil
	}
	return page, nil
}

type requestReplyCapture struct{ page wire.RequestPage }
type requestReplyValue struct {
	wire.Operator
	page wire.RequestPage
}

func (v requestReplyValue) Requests(int64, int64) (wire.RequestPage, error) { return v.page, nil }
func (c *requestReplyCapture) ExchangeFrame(frame []byte) ([]byte, error) {
	var call struct {
		Service string `json:"service"`
		Method  string `json:"method"`
	}
	if len(frame) > int(listen.DefaultMaxFrame) || json.Unmarshal(frame, &call) != nil || call.Service != "abstraction.inference/operator@1" || call.Method != "Requests" {
		return nil, errors.New("unexpected request preflight method")
	}
	reply, err := (&wire.OperatorDispatcher{Handler: requestReplyValue{page: c.page}}).ExchangeFrame(frame)
	if err != nil {
		return nil, err
	}
	if len(reply) > int(listen.DefaultMaxFrame) {
		return nil, errors.New("request preflight reply exceeds frame")
	}
	return reply, nil
}
