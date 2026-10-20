package service

import (
	"bytes"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"strings"
	"testing"
)

type legacyOperatorOnly struct{ Operator }

func TestProgramBudgetLegacyCustomHostIsUnsupported(t *testing.T) {
	r := &operatorReceiver{receiver: &receiver{host: &Host{Operator: &legacyOperatorOnly{}}}}
	page, err := r.ProgramBudgets("fixture")
	if err != nil || page.Outcome != wire.ProgramBudgetReadOutcomeUnsupported || page.Revision != "" || len(page.Entries) != 0 {
		t.Fatal(page, err)
	}
	change, err := r.SetProgramBudget(wire.ProgramBudgetChange{})
	if err != nil || change.Outcome != wire.ProgramBudgetEditOutcomeUnsupported || change.Revision != "" {
		t.Fatal(change, err)
	}
}

// Failure feedback: max_encoded qualifies the compact page, not its nested
// pretty representation plus service envelope. Always qualify the framed reply.
func TestProgramBudgetPageAggregateRefusesWholeFramedReply(t *testing.T) {
	for _, size := range []int{64, 3933, 3934, 3935, 4096} {
		page := wire.ProgramBudgetPage{Outcome: wire.ProgramBudgetReadOutcomePage, Revision: "program-budgets-1", Profiles: []string{"chat"}}
		for i := 0; i < 256; i++ {
			page.Entries = append(page.Entries, wire.ProgramBudgetEntry{Program: "/" + strings.Repeat("a", size-1), Spend: wire.Spend{Day: "2026-11-04"}})
		}
		raw := &budgetFramedTransport{page: page}
		_, rawErr := wire.NewOperatorClient(raw).ProgramBudgets("fixture")
		if size == 3934 && (raw.size != 1048606 || rawErr == nil) {
			t.Fatal("envelope-only counterexample lost", raw.size, rawErr)
		}
		if size == 3935 {
			var failure *wire.ServiceError
			if !errors.As(rawErr, &failure) || failure.Code != wire.ServiceErrorCodeInvalidResult {
				t.Fatal("nested counterexample lost", rawErr)
			}
		}
		framed := &budgetFramedTransport{page: page, qualify: true}
		got, err := wire.NewOperatorClient(framed).ProgramBudgets("fixture")
		if err != nil {
			t.Fatal(err)
		}
		if size == 64 || size == 3933 {
			if got.Outcome != wire.ProgramBudgetReadOutcomePage || len(got.Entries) != 256 {
				t.Fatal("ordinary page lost", got.Outcome, len(got.Entries))
			}
		} else if got.Outcome != wire.ProgramBudgetReadOutcomeUnavailable || got.Revision != "" || len(got.Entries) != 0 || len(got.Profiles) != 0 {
			t.Fatal("oversized page leaked partial configuration", got.Outcome, got.Revision, len(got.Entries), len(got.Profiles))
		}
	}
}

type budgetFramedValue struct {
	wire.Operator
	page    wire.ProgramBudgetPage
	qualify bool
}

func (v budgetFramedValue) ProgramBudgets(string) (wire.ProgramBudgetPage, error) {
	if v.qualify {
		return programBudgetPage(v.page)
	}
	return v.page, nil
}

type budgetFramedTransport struct {
	page    wire.ProgramBudgetPage
	qualify bool
	size    int
}

func (f *budgetFramedTransport) ExchangeFrame(request []byte) ([]byte, error) {
	reply, err := (&wire.OperatorDispatcher{Handler: budgetFramedValue{page: f.page, qualify: f.qualify}}).ExchangeFrame(request)
	if err != nil {
		return nil, err
	}
	f.size = len(reply)
	var framed bytes.Buffer
	if err = listen.WriteFrameTo(&framed, reply, 0); err != nil {
		return nil, err
	}
	return listen.ReadFrameFrom(&framed, 0)
}

func TestProgramBudgetPreflightIsClosedAndEmptyFallbackFits(t *testing.T) {
	capture := &programBudgetReplyCapture{}
	if _, err := wire.NewOperatorClient(capture).Hosts(); err == nil {
		t.Fatal("unexpected method admitted")
	}
	if _, err := capture.ExchangeFrame(make([]byte, int(listen.DefaultMaxFrame)+1)); err == nil {
		t.Fatal("oversized capture admitted")
	}
	framed := &budgetFramedTransport{page: wire.ProgramBudgetPage{}, qualify: true}
	page, err := wire.NewOperatorClient(framed).ProgramBudgets("fixture")
	if err != nil || page.Outcome != wire.ProgramBudgetReadOutcomeUnavailable || page.Revision != "" || len(page.Entries) != 0 || len(page.Profiles) != 0 {
		t.Fatal(page, err)
	}
}
