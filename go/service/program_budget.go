package service

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// ProgramBudgetOperator is additive native composition on the existing operator.
// Existing custom Operator hosts remain valid and report typed unsupported.
type ProgramBudgetOperator interface {
	ProgramBudgets(context.Context, inference.Subject, string) wire.ProgramBudgetPage
	SetProgramBudget(context.Context, inference.Subject, wire.ProgramBudgetChange) wire.ProgramBudgetResult
}

func (r *operatorReceiver) ProgramBudgets(credential string) (wire.ProgramBudgetPage, error) {
	if owner, ok := r.host.Operator.(ProgramBudgetOperator); ok {
		return programBudgetPage(owner.ProgramBudgets(r.ctx, r.caller(), credential))
	}
	return wire.ProgramBudgetPage{Outcome: wire.ProgramBudgetReadOutcomeUnsupported, Entries: []wire.ProgramBudgetEntry{}, Profiles: []string{}}, nil
}

// Qualify the actual generated reply, including nested formatting and envelope.
// A standalone compact record limit does not prove that its framed reply fits.
func programBudgetPage(page wire.ProgramBudgetPage) (wire.ProgramBudgetPage, error) {
	capture := &programBudgetReplyCapture{page: page}
	if _, err := wire.NewOperatorClient(capture).ProgramBudgets(""); err == nil {
		return page, nil
	}
	return wire.ProgramBudgetPage{Outcome: wire.ProgramBudgetReadOutcomeUnavailable, Entries: []wire.ProgramBudgetEntry{}, Profiles: []string{}}, nil
}

// This private adapter serializes one fixed value only. It never calls the real
// operator, resolves a caller, or opens a transport. The generated client supplies
// the closed method; check it before dispatch so embedded methods cannot run.
type programBudgetReplyCapture struct{ page wire.ProgramBudgetPage }
type programBudgetReplyValue struct {
	wire.Operator
	page wire.ProgramBudgetPage
}

func (v programBudgetReplyValue) ProgramBudgets(string) (wire.ProgramBudgetPage, error) {
	return v.page, nil
}
func (c *programBudgetReplyCapture) ExchangeFrame(frame []byte) ([]byte, error) {
	if len(frame) > int(listen.DefaultMaxFrame) {
		return nil, errors.New("budget preflight request exceeds frame")
	}
	var call struct {
		Service string `json:"service"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(frame, &call); err != nil || call.Service != "abstraction.inference/operator@1" || call.Method != "ProgramBudgets" {
		return nil, errors.New("unexpected budget preflight method")
	}
	reply, err := (&wire.OperatorDispatcher{Handler: programBudgetReplyValue{page: c.page}}).ExchangeFrame(frame)
	if err != nil {
		return nil, err
	}
	// Keep no copy or growing capture buffer; reject before the client decodes.
	if len(reply) > int(listen.DefaultMaxFrame) {
		return nil, errors.New("budget preflight reply exceeds frame")
	}
	return reply, nil
}
func (r *operatorReceiver) SetProgramBudget(request wire.ProgramBudgetChange) (wire.ProgramBudgetResult, error) {
	if owner, ok := r.host.Operator.(ProgramBudgetOperator); ok {
		return owner.SetProgramBudget(r.ctx, r.caller(), request), nil
	}
	return wire.ProgramBudgetResult{Outcome: wire.ProgramBudgetEditOutcomeUnsupported}, nil
}
