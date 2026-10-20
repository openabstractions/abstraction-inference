package client

import (
	"context"
	"errors"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type ProgramBudget = wire.ProgramBudget
type ProgramBudgetEntry = wire.ProgramBudgetEntry
type ProgramBudgetPage = wire.ProgramBudgetPage
type ProgramBudgetChange = wire.ProgramBudgetChange
type ProgramBudgetResult = wire.ProgramBudgetResult
type ProgramBudgetReadOutcome = wire.ProgramBudgetReadOutcome
type ProgramBudgetEditOutcome = wire.ProgramBudgetEditOutcome

const (
	ProgramBudgetReadOutcomePage        = wire.ProgramBudgetReadOutcomePage
	ProgramBudgetReadOutcomeInvalid     = wire.ProgramBudgetReadOutcomeInvalid
	ProgramBudgetReadOutcomeForbidden   = wire.ProgramBudgetReadOutcomeForbidden
	ProgramBudgetReadOutcomeUnavailable = wire.ProgramBudgetReadOutcomeUnavailable
	ProgramBudgetReadOutcomeUnsupported = wire.ProgramBudgetReadOutcomeUnsupported
	ProgramBudgetEditOutcomeApplied     = wire.ProgramBudgetEditOutcomeApplied
	ProgramBudgetEditOutcomeConflict    = wire.ProgramBudgetEditOutcomeConflict
	ProgramBudgetEditOutcomeInvalid     = wire.ProgramBudgetEditOutcomeInvalid
	ProgramBudgetEditOutcomeForbidden   = wire.ProgramBudgetEditOutcomeForbidden
	ProgramBudgetEditOutcomeUnavailable = wire.ProgramBudgetEditOutcomeUnavailable
	ProgramBudgetEditOutcomeUnsupported = wire.ProgramBudgetEditOutcomeUnsupported
)

func ProgramBudgetReadOutcomeValues() []ProgramBudgetReadOutcome {
	return wire.ProgramBudgetReadOutcomeValues()
}
func ProgramBudgetEditOutcomeValues() []ProgramBudgetEditOutcome {
	return wire.ProgramBudgetEditOutcomeValues()
}

func (o *Operator) ProgramBudgets(ctx context.Context, credential string) (ProgramBudgetPage, error) {
	c, err := o.client(ctx)
	if err != nil {
		return ProgramBudgetPage{}, err
	}
	page, err := c.ProgramBudgets(credential)
	var failure *wire.ServiceError
	if errors.As(err, &failure) && failure.Code == wire.ServiceErrorCodeUnknownMethod {
		return ProgramBudgetPage{Outcome: wire.ProgramBudgetReadOutcomeUnsupported, Entries: []wire.ProgramBudgetEntry{}, Profiles: []string{}}, nil
	}
	return page, err
}
func (o *Operator) SetProgramBudget(ctx context.Context, request ProgramBudgetChange) (ProgramBudgetResult, error) {
	c, err := o.client(ctx)
	if err != nil {
		return ProgramBudgetResult{}, err
	}
	result, err := c.SetProgramBudget(request)
	var failure *wire.ServiceError
	if errors.As(err, &failure) && failure.Code == wire.ServiceErrorCodeUnknownMethod {
		return ProgramBudgetResult{Outcome: wire.ProgramBudgetEditOutcomeUnsupported}, nil
	}
	return result, err
}
