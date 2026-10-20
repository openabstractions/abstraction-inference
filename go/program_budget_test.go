package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProgramMoneyBudgetUnreportedCostPersistsAndClears(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "counts.json")
	open := func() *Ceilings {
		c, err := OpenCeilings(path, nil, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	a, b := Subject{Account: "account", Program: "a"}, Subject{Account: "account", Program: "b"}
	set := func() {
		rev, _, _ := c.ProgramBudgets("key", a.Account)
		if _, ok, err := c.SetProgramBudget(rev, "key", a, &Ceiling{MicrosPerDay: 100}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	set()
	configuredRevision, _, _ := c.ProgramBudgets("key", a.Account)
	// A reported zero is an observation, and cannot exhaust a positive limit.
	if err := c.addModalitiesForSubject("key", a, 3, 0, 0, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	if reason := c.budgetRefusal("key", a); reason != "" {
		t.Fatal(reason)
	}
	// Missing cost keeps the valid response and all observed usage.
	if err := c.addModalitiesForSubject("key", a, 4, 0, 0, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	c = open()
	readRevision, entries, err := c.ProgramBudgets("key", a.Account)
	if err != nil || readRevision != configuredRevision || len(entries) != 1 || !entries[0].CostUnreported {
		t.Fatal("usage readback/config revision", readRevision, entries, err)
	}
	if reason := c.budgetRefusal("key", a); reason != "cost:unreported" {
		t.Fatal(reason)
	}
	if reason := c.budgetRefusal("key", b); reason != "" {
		t.Fatal("cross-program state", reason)
	}
	_, tokens, micros, requests := c.ProgramSpend("key", a)
	if tokens != 7 || micros != 0 || requests != 2 {
		t.Fatal(tokens, micros, requests)
	}
	set()
	_, entries, _ = c.ProgramBudgets("key", a.Account)
	if entries[0].CostUnreported {
		t.Fatal("person change retained readback flag")
	}
	if reason := c.budgetRefusal("key", a); reason != "" {
		t.Fatal("person change failed to clear state", reason)
	}
	if err := c.addModalitiesForSubject("key", a, 0, 0, 0, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	c = open()
	if reason := c.budgetRefusal("key", a); reason != "" {
		t.Fatal("UTC rollover retained state", reason)
	}
	if err := c.addModalitiesForSubject("key", a, 9, -10000, 0, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	c = open()
	if c.budgetRefusal("key", a) != "cost:unreported" {
		t.Fatal("negative native cost admitted more work")
	}
	_, tokens, micros, requests = c.ProgramSpend("key", a)
	if tokens != 9 || micros != 0 || requests != 1 {
		t.Fatal(tokens, micros, requests)
	}
}

func TestProgramMoneyBudgetDurableMissingCostKeepsOriginalAttempt(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := Subject{Account: "account", Program: "program"}
	if err := c.SetProgramLimit("key", s, Ceiling{MicrosPerDay: 100}); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"attempt", "images", "images"} {
		if err := c.addDurableForSubject(stage, "original", "key", &s, map[string]int64{"attempt": 1}[stage], 0); err != nil {
			t.Fatal(err)
		}
	}
	if !c.DurableAttemptCounted("original") || c.budgetRefusal("key", s) != "cost:unreported" {
		t.Fatal("lost original or missing terminal state")
	}
	_, _, _, requests := c.ProgramSpend("key", s)
	if requests != 1 {
		t.Fatal("retried accounting", requests)
	}
}

func TestReportedCostZeroAndAbsenceStayDistinct(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		observed bool
	}{{"", false}, {"0", true}, {"0.000000", true}, {"0.12", true}, {"garbage", false}, {"-0.01", false}} {
		_, ok := micros(json.Number(tc.raw))
		if ok != tc.observed {
			t.Fatal(tc.raw, ok)
		}
	}
}

func TestCostConversionRoundsUpWithBoundedJSONDecimals(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		value    int64
		observed bool
	}{
		{"0", 0, true}, {"0.000000", 0, true}, {"0e30", 0, true},
		{"0.0012340", 1234, true}, {"0.0012344", 1235, true}, {"0.0012345", 1235, true}, {"1.2345e-3", 1235, true},
		{"0.0000002", 1, true}, {"0.0000011", 2, true}, {"2e-07", 1, true}, {"1E+0", 1000000, true}, {"1e-30", 1, true},
		{"9223372036854.775807", 9223372036854775807, true},
		{"9223372036854.7758070", 9223372036854775807, true},
		{"9223372036854.7758071", 0, false}, {"9223372036854.775808", 0, false},
		{"1e31", 0, false}, {"1e-31", 0, false}, {"0e31", 0, false},
		{"-0", 0, false}, {"-0.0000000", 0, false}, {"-0.01", 0, false},
		{"0.000000xyz", 0, false}, {"1.", 0, false}, {"01", 0, false}, {".1", 0, false}, {"1e", 0, false}, {"1e+", 0, false}, {"+1", 0, false},
		{"999999999999999999999999999999999", 0, false},
	} {
		value, ok := micros(json.Number(tc.raw))
		if value != tc.value || ok != tc.observed {
			t.Fatal(tc.raw, value, ok)
		}
	}
	if _, ok := micros(json.Number("0." + strings.Repeat("0", 64) + "1")); ok {
		t.Fatal("unbounded mantissa accepted")
	}
}

type programCostFixtureTransport struct{ target *url.URL }

func (p programCostFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	u := *r.URL
	c.URL = &u
	c.URL.Scheme, c.URL.Host = p.target.Scheme, p.target.Host
	return http.DefaultTransport.RoundTrip(c)
}

func TestSubmicroReportedCostsCannotBypassNativeProgramBudget(t *testing.T) {
	for _, raw := range []string{"0.0000002", "2e-07"} {
		t.Run(raw, func(t *testing.T) {
			c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetProgramLimit("openrouter", caller, Ceiling{MicrosPerDay: 6}); err != nil {
				t.Fatal(err)
			}
			f := setup(t, router.WireOpenAICompatible, func(cfg *Config) { cfg.Ceilings = c })
			// All traffic goes to the inert fixture, including the fixed binding.
			target, err := url.Parse(f.up.server.URL)
			if err != nil {
				t.Fatal(err)
			}
			f.provider.cfg.HTTP = &http.Client{Transport: programCostFixtureTransport{target}}
			f.provider.cfg.Router.Hosts()[0].Base = "https://openrouter.ai/api/v1"
			f.up.cost = raw
			for i := 0; i < 6; i++ {
				a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
				if a.Outcome != wire.StartOutcomeAccepted {
					t.Fatal(i, a)
				}
				_, r, _ := drain(t, f.provider, caller, a.Operation)
				if r.Outcome != wire.ReplyOutcomeCompleted || r.Cost == nil || r.Cost.Micros != 1 {
					t.Fatal(i, r)
				}
			}
			a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
			if a.Outcome != wire.StartOutcomeBudgetExceeded || a.Reason != "budget:micros:"+caller.Program || len(f.up.posts()) != 6 {
				t.Fatal(a, len(f.up.posts()))
			}
			_, entries, err := c.ProgramBudgets("openrouter", caller.Account)
			if err != nil || len(entries) != 1 || entries[0].Micros != 6 || entries[0].Requests != 6 || entries[0].CostUnreported {
				t.Fatal(entries, err)
			}
		})
	}
}

func TestNativeCostParserPreservesZeroAndMissing(t *testing.T) {
	for _, raw := range []string{"", "0", "-0.01"} {
		t.Run("cost="+raw, func(t *testing.T) {
			c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			f := setup(t, router.WireOpenAICompatible, func(cfg *Config) { cfg.Ceilings = c })
			f.up.cost = raw
			f.up.release = make(chan struct{})
			a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatal(a)
			}
			// The person can configure a limit while an admitted call is active.
			if err := c.SetProgramLimit("openrouter", caller, Ceiling{MicrosPerDay: 100}); err != nil {
				t.Fatal(err)
			}
			close(f.up.release)
			_, reply, _ := drain(t, f.provider, caller, a.Operation)
			if reply.Outcome != wire.ReplyOutcomeCompleted || (reply.Cost != nil) != (raw == "0") {
				t.Fatal(reply)
			}
			if reply.Cost != nil && reply.Cost.Micros != 0 {
				t.Fatal(reply.Cost)
			}
			_, entries, err := c.ProgramBudgets("openrouter", caller.Account)
			if err != nil || len(entries) != 1 || entries[0].CostUnreported != (raw != "0") {
				t.Fatal(entries, err)
			}
			if raw != "0" {
				c, err = OpenCeilings(c.path, nil, nil)
				if err != nil || c.budgetRefusal("openrouter", caller) != "cost:unreported" {
					t.Fatal("missing durable refusal", err)
				}
				_, tokens, money, requests := c.ProgramSpend("openrouter", caller)
				if tokens == 0 || money != 0 || requests != 1 {
					t.Fatal("lost observed usage", tokens, money, requests)
				}
			}
		})
	}
}

func TestProgramBudgetConditionalDurabilityAndExplicitNoLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ceilings.json")
	c, err := OpenCeilings(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := Subject{Account: "account", Program: "program-a"}
	b := Subject{Account: "account", Program: "program-b"}
	rev, entries, err := c.ProgramBudgets("key", a.Account)
	if err != nil || len(entries) != 0 {
		t.Fatal(rev, entries, err)
	}
	next, applied, err := c.SetProgramBudget(rev, "key", a, &Ceiling{RequestsPerDay: 1})
	if err != nil || !applied || next == rev {
		t.Fatal(next, applied, err)
	}
	if _, applied, err := c.SetProgramBudget(rev, "key", a, nil); err != nil || applied {
		t.Fatal(applied, err)
	}
	if err := c.addModalitiesForSubject("key", a, 10, 3, 2, 4, 5); err != nil {
		t.Fatal(err)
	}
	if reason := c.budgetRefusal("key", a); reason == "" {
		t.Fatal("program request budget ignored")
	}
	if reason := c.budgetRefusal("key", b); reason != "" {
		t.Fatal("program budget crossed native subject")
	}
	c, err = OpenCeilings(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, entries, err := c.ProgramBudgets("key", a.Account)
	if err != nil || current != next || len(entries) != 1 || entries[0].Requests != 1 || entries[0].Images != 2 || entries[0].Budget == nil {
		t.Fatal(current, entries, err)
	}
	next, applied, err = c.SetProgramBudget(current, "key", a, &Ceiling{})
	if err != nil || !applied {
		t.Fatal(next, applied, err)
	}
	if reason := c.budgetRefusal("key", a); reason != "" {
		t.Fatal("explicit No limit still limits")
	}
	_, entries, err = c.ProgramBudgets("key", a.Account)
	if err != nil || entries[0].Budget == nil || entries[0].Requests != 1 {
		t.Fatal(entries, err)
	}
	c, err = OpenCeilings(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err = c.ProgramBudgets("key", a.Account)
	if err != nil || entries[0].Budget == nil || *entries[0].Budget != (Ceiling{}) {
		t.Fatal("explicit No limit was lost across reopen", entries, err)
	}
	_, applied, err = c.SetProgramBudget(next, "key", a, nil)
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	c, err = OpenCeilings(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, entries, err = c.ProgramBudgets("key", a.Account)
	if err != nil || len(entries) != 1 || entries[0].Budget != nil || entries[0].Requests != 1 {
		t.Fatal(entries, err)
	}
}

func TestProgramBudgetMigratesExistingCountsInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ceilings.json")
	if err := os.WriteFile(path, []byte(`{"key":{"day":"2026-11-04","tokens":17,"micros":8,"requests":2}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCeilings(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := c.counts["key"]
	rev, _, _ := c.ProgramBudgets("key", "account")
	if _, ok, err := c.SetProgramBudget(rev, "key", Subject{Account: "account", Program: "program"}, &Ceiling{TokensPerDay: 100}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	reopened, err := OpenCeilings(path, nil, nil)
	if err != nil || reopened.counts["key"] != before {
		t.Fatal(reopened, err)
	}
}

func TestProgramBudgetPersistenceFailureDoesNotChangeChoice(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject := Subject{Account: "account", Program: "program"}
	rev, _, _ := c.ProgramBudgets("key", subject.Account)
	// A file parent blocks the existing state owner's write.
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	c.path = filepath.Join(parent, "counts.json")
	if _, ok, err := c.SetProgramBudget(rev, "key", subject, &Ceiling{RequestsPerDay: 1}); err == nil || ok {
		t.Fatal(ok, err)
	}
	current, entries, err := c.ProgramBudgets("key", subject.Account)
	if err != nil || current != rev || len(entries) != 0 {
		t.Fatal(current, entries, err)
	}
}

func TestProgramBudgetFinalAdmissionFailurePreservesRevision(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject := Subject{Account: "account", Program: "program"}
	rev, _, _ := c.ProgramBudgets("key", subject.Account)
	denied := errors.New("authority changed")
	_, applied, err := c.SetProgramBudget(rev, "key", subject, &Ceiling{RequestsPerDay: 1}, func() error { return denied })
	if !errors.Is(err, denied) || applied {
		t.Fatal(applied, err)
	}
	current, entries, err := c.ProgramBudgets("key", subject.Account)
	if err != nil || current != rev || len(entries) != 0 {
		t.Fatal(current, entries, err)
	}
}

func TestProgramBudgetConfigurationRemainsReadableAtItsBound(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	revision, _, _ := c.ProgramBudgets("key", "account")
	for i := 0; i < 256; i++ {
		var applied bool
		revision, applied, err = c.SetProgramBudget(revision, "key", Subject{Account: "account", Program: fmt.Sprintf("program-%03d", i)}, &Ceiling{})
		if err != nil || !applied {
			t.Fatal(i, applied, err)
		}
	}
	if _, applied, err := c.SetProgramBudget(revision, "key", Subject{Account: "account", Program: "one-too-many"}, &Ceiling{}); err == nil || applied {
		t.Fatal(applied, err)
	}
	current, entries, err := c.ProgramBudgets("key", "account")
	if err != nil || current != revision || len(entries) != 256 {
		t.Fatal(current, len(entries), err)
	}
}

func TestProgramBudgetAdmissionFailureDoesNotExposeConflictRevision(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("authority changed while waiting")
	revision, applied, err := c.SetProgramBudget("stale", "key", Subject{Account: "account", Program: "program"}, &Ceiling{}, func() error { return denied })
	if !errors.Is(err, denied) || applied || revision != "" {
		t.Fatal(revision, applied, err)
	}
}
