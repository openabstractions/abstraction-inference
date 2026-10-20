package inference

import (
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"strings"
	"testing"
)

func TestProgramBudgetNativeBoundsRefuseBeforeTransport(t *testing.T) {
	for _, request := range []wire.ProgramBudgetChange{
		{Credential: strings.Repeat("a", 65)},
		{Program: strings.Repeat("a", 4097)},
		{ExpectedRevision: strings.Repeat("a", 129)},
	} {
		transport := &chat3NoEffectTransport{}
		_, err := wire.NewOperatorClient(transport).SetProgramBudget(request)
		if chat3RefusalWord(err) != "bad_value" || transport.calls != 0 {
			t.Fatalf("%v calls=%d", err, transport.calls)
		}
	}
	transport := &chat3NoEffectTransport{}
	_, err := wire.NewOperatorClient(transport).ProgramBudgets(strings.Repeat("a", 65))
	if chat3RefusalWord(err) != "bad_value" || transport.calls != 0 {
		t.Fatalf("%v calls=%d", err, transport.calls)
	}
}

func TestProgramBudgetWriterBounds(t *testing.T) {
	page := wire.ProgramBudgetPage{Outcome: wire.ProgramBudgetReadOutcomePage, Entries: []wire.ProgramBudgetEntry{}, Profiles: []string{}}
	for _, mutate := range []func(*wire.ProgramBudgetPage){
		func(p *wire.ProgramBudgetPage) {
			for i := 0; i < 257; i++ {
				p.Entries = append(p.Entries, wire.ProgramBudgetEntry{Program: "/p"})
			}
		},
		func(p *wire.ProgramBudgetPage) {
			p.Profiles = make([]string, 17)
			for i := range p.Profiles {
				p.Profiles[i] = "chat"
			}
		},
		func(p *wire.ProgramBudgetPage) { p.Profiles = []string{strings.Repeat("p", 257)} },
	} {
		bad := page
		mutate(&bad)
		b, err := wire.ToJSON(bad)
		if chat3RefusalWord(err) != "bad_value" || len(b) != 0 {
			t.Fatalf("%v bytes=%d", err, len(b))
		}
	}
	// Identity permits tabs and C1 bytes in native program paths.
	page.Entries = []wire.ProgramBudgetEntry{{Program: "/a\t\u0085"}}
	if _, err := wire.ToJSON(page); err != nil {
		t.Fatal(err)
	}
}
