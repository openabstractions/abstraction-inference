package inference

import (
	"encoding/json"
	"errors"
	"fmt"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"github.com/openabstractions/abstraction-router/go/hostedcatalogue"
	"sort"
)

type programBudgetRecord struct {
	Credential string   `json:"credential"`
	Subject    Subject  `json:"subject"`
	Budget     *Ceiling `json:"budget,omitempty"`
}

func budgetStartOutcome(reason string) wire.StartOutcome {
	if reason == "cost:unreported" {
		return wire.StartOutcomeExhausted
	}
	return wire.StartOutcomeBudgetExceeded
}

type ceilingState struct {
	Version        int                            `json:"version"`
	Counts         map[string]dayCount            `json:"counts"`
	Programs       map[string]programBudgetRecord `json:"programs"`
	BudgetRevision uint64                         `json:"budget_revision"`
}

func (c *Ceilings) readState(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var version int
	if value, ok := fields["version"]; !ok || json.Unmarshal(value, &version) != nil {
		return json.Unmarshal(raw, &c.counts)
	}
	var state ceilingState
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if version != 1 || state.Counts == nil || state.Programs == nil {
		return errors.New("unsupported ceiling state")
	}
	for key, entry := range state.Programs {
		if entry.Credential == "" || entry.Subject.Account == "" || entry.Subject.Program == "" || key != programBudgetKey(entry.Credential, entry.Subject) || entry.Budget != nil && entry.Budget.Negative() {
			return errors.New("invalid persisted program budget")
		}
		if entry.Budget != nil {
			c.programLimits[key] = *entry.Budget
		}
	}
	c.counts, c.programs, c.budgetRevision = state.Counts, state.Programs, state.BudgetRevision
	return nil
}

func (c *Ceilings) programRevision() string {
	return fmt.Sprintf("program-budgets-%d", c.budgetRevision)
}

// ProgramBudgetState is a detached snapshot of one native program's configured
// choice and observed UTC-day usage. Nil Budget differs from explicit No limit.
type ProgramBudgetState struct {
	CostUnreported                                             bool
	Program                                                    string
	Budget                                                     *Ceiling
	Day                                                        string
	Tokens, Micros, Requests, Images, AudioSeconds, Characters int64
}

func (c *Ceilings) ProgramBudgets(name, account string) (string, []ProgramBudgetState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := []ProgramBudgetState{}
	for key, entry := range c.programs {
		if entry.Credential != name || entry.Subject.Account != account {
			continue
		}
		n := c.count(key)
		if entry.Budget == nil && n.Tokens == 0 && n.Micros == 0 && n.Requests == 0 && n.Images == 0 && n.AudioSeconds == 0 && n.Characters == 0 {
			continue
		}
		out := ProgramBudgetState{Program: entry.Subject.Program, Day: n.Day, Tokens: n.Tokens, Micros: n.Micros, Requests: n.Requests, Images: n.Images, AudioSeconds: n.AudioSeconds, Characters: n.Characters}
		out.CostUnreported = entry.Budget != nil && entry.Budget.MicrosPerDay > 0 && n.CostUnreported
		if entry.Budget != nil {
			copy := *entry.Budget
			out.Budget = &copy
		}
		entries = append(entries, out)
	}
	if len(entries) > 256 {
		return "", nil, errors.New("program budget snapshot exceeds 256 entries")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Program < entries[j].Program })
	return c.programRevision(), entries, nil
}

// SetProgramBudget conditionally persists the choice in the existing counts
// state. Usage is retained, and a present zero value records explicit No limit.
func (c *Ceilings) SetProgramBudget(expected, name string, subject Subject, budget *Ceiling, admit ...func() error) (revision string, applied bool, err error) {
	if name == "" || subject.Account == "" || subject.Program == "" || budget != nil && budget.Negative() {
		return "", false, errors.New("invalid program budget")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, check := range admit {
		if check != nil {
			if err := check(); err != nil {
				return "", false, err
			}
		}
	}
	if expected != c.programRevision() {
		return c.programRevision(), false, nil
	}
	if err := c.changeProgramBudget(name, subject, budget, admit...); err != nil {
		return "", false, err
	}
	return c.programRevision(), true, nil
}

func (c *Ceilings) changeProgramBudget(name string, subject Subject, budget *Ceiling, admit ...func() error) error {
	key := programBudgetKey(name, subject)
	before, hadBefore := c.programs[key]
	if budget != nil && (!hadBefore || before.Budget == nil) {
		configured := 0
		for _, entry := range c.programs {
			if entry.Credential == name && entry.Subject.Account == subject.Account && entry.Budget != nil {
				configured++
			}
		}
		if configured >= 256 {
			return errors.New("program budget configuration exceeds 256 entries")
		}
	}
	previousLimit, hadLimit := c.programLimits[key]
	previousCount, hadCount := c.counts[key]
	count := c.count(key)
	count.CostUnreported = false
	c.counts[key] = count
	record := programBudgetRecord{Credential: name, Subject: subject}
	if budget != nil {
		copy := *budget
		record.Budget = &copy
		c.programLimits[key] = copy
	} else {
		delete(c.programLimits, key)
	}
	c.programs[key] = record
	c.budgetRevision++
	if err := c.persist(admit...); err != nil {
		if hadCount {
			c.counts[key] = previousCount
		} else {
			delete(c.counts, key)
		}
		c.budgetRevision--
		if hadBefore {
			c.programs[key] = before
		} else {
			delete(c.programs, key)
		}
		if hadLimit {
			c.programLimits[key] = previousLimit
		} else {
			delete(c.programLimits, key)
		}
		return err
	}
	return nil
}

func (c *Ceilings) addModalitiesForSubject(name string, subject Subject, tokens, micros, images, audioSeconds, characters int64, costObserved ...bool) error {
	// Native adapters may supply a Cost directly. A negative value cannot be
	// applied to nonnegative spend counters; retain other usage and fail closed
	// for a configured money budget as for an absent observation.
	if micros < 0 && len(costObserved) > 0 {
		micros, costObserved = 0, []bool{false}
	}
	if name == "" || subject.Account == "" || subject.Program == "" || tokens < 0 || micros < 0 || images < 0 || audioSeconds < 0 || characters < 0 {
		return errors.New("invalid program accounting observation")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := programBudgetKey(name, subject)
	if _, ok := c.programs[key]; !ok {
		c.programs[key] = programBudgetRecord{Credential: name, Subject: subject}
	}
	for _, key := range []string{name, key} {
		c.addModalities(key, tokens, micros, 1, images, audioSeconds, characters)
	}
	if len(costObserved) > 0 && !costObserved[0] {
		c.markCostUnreported(key)
	}
	return c.persist()
}

// markCostUnreported shares the usage state and UTC day. A valid completion
// remains valid; a money limit cannot admit another call without an observation.
// The caller holds mu.
func (c *Ceilings) markCostUnreported(key string) {
	if key != "" && c.programLimits[key].MicrosPerDay > 0 {
		n := c.count(key)
		n.CostUnreported = true
		c.counts[key] = n
	}
}

// budgetRefusal takes the single admission snapshot for every charged modality.
func (c *Ceilings) budgetRefusal(name string, subject Subject, selected ...*router.Host) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := programBudgetKey(name, subject)
	if len(selected) > 0 && c.programLimits[key].MicrosPerDay > 0 {
		h := selected[0]
		if h == nil || !hostedcatalogue.Observations(h.Name, h.Base, h.Wire, h.Credential).Micros {
			return "cost:unreported"
		}
	}
	if c.programLimits[key].MicrosPerDay > 0 && c.count(key).CostUnreported {
		return "cost:unreported"
	}
	if unit, over := exceeded(c.limits[name], c.count(name)); over {
		return "ceiling:" + unit + ":" + name
	}
	if unit, over := exceeded(c.programLimits[key], c.count(key)); over {
		return "budget:" + unit + ":" + subject.Program
	}
	return ""
}
