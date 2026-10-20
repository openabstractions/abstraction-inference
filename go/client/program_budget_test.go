package client

import (
	"context"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"net"
	"strings"
	"sync"
	"testing"
)

// The legacy endpoint's generated error frame carries unknown_method. Ordinary
// failures remain errors, and neither method is retried by the client.
type budgetUnknownOperator struct {
	wire.Operator
	code  string
	calls int
}

func (o *budgetUnknownOperator) ProgramBudgets(string) (wire.ProgramBudgetPage, error) {
	o.calls++
	return wire.ProgramBudgetPage{}, wire.DispatchError(o.code)
}
func (o *budgetUnknownOperator) SetProgramBudget(wire.ProgramBudgetChange) (wire.ProgramBudgetResult, error) {
	o.calls++
	return wire.ProgramBudgetResult{}, wire.DispatchError(o.code)
}
func TestProgramBudgetUnknownMethodIsTypedUnsupported(t *testing.T) {
	for _, code := range []string{"unknown_method", "unavailable"} {
		t.Run(code, func(t *testing.T) {
			port := &budgetUnknownOperator{code: code}
			var wg sync.WaitGroup
			o := NewOperatorWithTransport(listen.FrameClient{Endpoint: "fixture", Dialer: func(context.Context, string) (net.Conn, error) {
				a, b := net.Pipe()
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer b.Close()
					frame, err := listen.ReadFrameFrom(b, 1<<20)
					if err != nil {
						return
					}
					reply, err := (&wire.OperatorDispatcher{Handler: port}).ExchangeFrame(frame)
					if err == nil {
						_ = listen.WriteFrameTo(b, reply, 1<<20)
					}
				}()
				return a, nil
			}})
			page, readErr := o.ProgramBudgets(t.Context(), strings.Repeat("c", 64))
			change, writeErr := o.SetProgramBudget(t.Context(), ProgramBudgetChange{ExpectedRevision: strings.Repeat("r", 128), Credential: strings.Repeat("c", 64), Program: strings.Repeat("p", 4096)})
			wg.Wait()
			if port.calls != 2 {
				t.Fatal("method repeated", port.calls)
			}
			if code == "unknown_method" {
				if readErr != nil || writeErr != nil || page.Outcome != wire.ProgramBudgetReadOutcomeUnsupported || change.Outcome != wire.ProgramBudgetEditOutcomeUnsupported || page.Revision != "" || len(page.Entries) != 0 {
					t.Fatal(page, change, readErr, writeErr)
				}
			} else if readErr == nil || writeErr == nil {
				t.Fatal("ordinary failure became supported or unsupported", readErr, writeErr)
			}
		})
	}
}

func TestProgramBudgetOverBoundNativeCallIsBadValueNotUnsupported(t *testing.T) {
	dials := 0
	o := NewOperatorWithTransport(listen.FrameClient{Endpoint: "fixture", Dialer: func(context.Context, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected transport")
	}})
	page, err := o.ProgramBudgets(t.Context(), strings.Repeat("c", 65))
	var refusal *wire.Refusal
	if !errors.As(err, &refusal) || refusal.Word != "bad_value" || page.Outcome == ProgramBudgetReadOutcomeUnsupported {
		t.Fatal(page, err)
	}
	for _, request := range []ProgramBudgetChange{{Credential: strings.Repeat("c", 65)}, {Program: strings.Repeat("p", 4097)}, {ExpectedRevision: strings.Repeat("r", 129)}} {
		result, err := o.SetProgramBudget(t.Context(), request)
		if !errors.As(err, &refusal) || refusal.Word != "bad_value" || result.Outcome == ProgramBudgetEditOutcomeUnsupported {
			t.Fatal(result, err)
		}
	}
	if dials != 0 {
		t.Fatal("over-bound call reached transport", dials)
	}
}
