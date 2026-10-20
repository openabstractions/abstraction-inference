package inference

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3ProgramAccountingKeepsCredentialAndProvenSubjectTotals(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "counts.json")
	c, err := OpenCeilings(path, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	a, b := caller, caller
	b.Program = "another-program"
	if err := c.SetProgramLimit("credential", a, Ceiling{RequestsPerDay: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddForSubject("credential", a, 7, 3, 12); err != nil {
		t.Fatal(err)
	}
	if unit, over := c.ExceededProgram("credential", a); !over || unit != "requests" {
		t.Fatalf("limit %s %v", unit, over)
	}
	if _, over := c.ExceededProgram("credential", b); over {
		t.Fatal("one program consumed another's limit")
	}
	if err := c.AddForSubject("credential", b, 2, 4, 5); err != nil {
		t.Fatal(err)
	}
	_, tokens, micros, requests, _, _, _ := c.SpendUnits("credential")
	if tokens != 16 || micros != 17 || requests != 2 {
		t.Fatalf("credential totals %d %d %d", tokens, micros, requests)
	}
	_, tokens, micros, requests = c.ProgramSpend("credential", a)
	if tokens != 10 || micros != 12 || requests != 1 {
		t.Fatalf("program totals %d %d %d", tokens, micros, requests)
	}
	reopened, err := OpenCeilings(path, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, tokens, _, requests = reopened.ProgramSpend("credential", a)
	if tokens != 10 || requests != 1 {
		t.Fatal("program counters lost across reopen")
	}
	_, tokens, _, _ = reopened.ProgramSpend("different-credential", a)
	if tokens != 0 {
		t.Fatal("program subtotal crossed credentials")
	}
	now = now.Add(24 * time.Hour)
	_, tokens, _, requests = reopened.ProgramSpend("credential", a)
	if tokens != 0 || requests != 0 {
		t.Fatal("program counters did not roll over with credential day")
	}
	if err := reopened.AddForSubject("credential", a, math.MaxInt64, math.MaxInt64, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if err := reopened.AddForSubject("credential", a, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
	_, tokens, micros, _ = reopened.ProgramSpend("credential", a)
	if tokens != math.MaxInt64 || micros != math.MaxInt64 {
		t.Fatal("observed counters overflowed")
	}
}

func TestChat3UnboundOwnerRefusesBeforeAdmissionAndBindsOnce(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.ChatLogicalOwner = "" })
	posts, uses := len(f.up.posts()), f.applier.count()
	if window := f.provider.GetChatHistoryWindow(caller); window.Outcome != wire.ChatHistoryOutcomeUnavailable || window.Window != nil {
		t.Fatalf("unbound history %+v", window)
	}
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
	r.Target.Model, r.Content.Credential = "anthropic/claude-sonnet-5", "openrouter"
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}
	if a := f.provider.StartV3(context.Background(), caller, r); a.Outcome != wire.ChatStartOutcomeUnavailable || a.Reason != "history:owner_unbound" {
		t.Fatalf("unbound start %+v", a)
	}
	if len(f.provider.chat3History.open.entries) != 0 || len(f.up.posts()) != posts || f.applier.count() != uses {
		t.Fatal("unbound chat3 produced an admission or effect")
	}
	if err := f.provider.BindChatV3LogicalOwner(""); err == nil {
		t.Fatal("blank logical owner bound")
	}
	if err := f.provider.BindChatV3LogicalOwner("persistent-job-owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.provider.BindChatV3LogicalOwner("persistent-job-owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.provider.BindChatV3LogicalOwner("replacement-owner"); err == nil {
		t.Fatal("logical owner changed")
	}
	window := f.provider.GetChatHistoryWindow(caller)
	if window.Window == nil || window.Window.LogicalOwner != "persistent-job-owner" || window.Window.HistoryEpoch != r.Identity.HistoryEpoch {
		t.Fatalf("bound window %+v", window)
	}
	if a := f.provider.StartV3(context.Background(), caller, r); a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("bound start %+v", a)
	}
	if err := f.provider.BindChatV3LogicalOwner("replacement-owner"); err == nil {
		t.Fatal("accepted history changed owner")
	}
}

func TestChat3UnboundOwnerLeavesLegacyAdmissionUnchanged(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.ChatLogicalOwner = "" })
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("legacy admission %+v", a)
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeCompleted || len(f.up.posts()) != 1 {
		t.Fatalf("legacy completion %+v", reply)
	}
}

func TestChat3ProgramBudgetRefusesBeforeCredentialAndUpstream(t *testing.T) {
	c, err := OpenCeilings(filepath.Join(t.TempDir(), "counts.json"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetProgramLimit("openrouter", caller, Ceiling{RequestsPerDay: 1}); err != nil {
		t.Fatal(err)
	}
	f := setup(t, router.WireOpenAICompatible, func(config *Config) { config.Ceilings = c })
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	a, err := client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("first admission %+v %v", a, err)
	}
	f.waitLogged(t, 1)
	posts, uses := len(f.up.posts()), f.applier.count()
	r.Identity.Key += "-next"
	a, err = client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeBudgetExceeded || a.Reason != "budget:requests:"+caller.Program {
		t.Fatalf("budget admission %+v %v", a, err)
	}
	if len(f.up.posts()) != posts || f.applier.count() != uses {
		t.Fatal("spent program budget applied credential or sent upstream")
	}
}

// The fixture's subject stands for the receiving boundary's attested subject.
// Real client frames and dispatcher codecs surround the real provider/HTTP path.
type chat3BoundFixture struct {
	p       *Provider
	subject Subject
	ctx     context.Context
}

func (f chat3BoundFixture) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	return f.p.GetChatHistoryWindow(f.subject), nil
}
func (f chat3BoundFixture) Start(r wire.RequestV3) (wire.ChatAdmission, error) {
	return f.p.StartV3(f.ctx, f.subject, r), nil
}
func (f chat3BoundFixture) Observe(id string, c, n, b, w int64) (wire.ChatPage, error) {
	return f.p.ObserveV3(f.ctx, f.subject, id, c, n, b, w), nil
}
func (f chat3BoundFixture) Cancel(id string) (wire.ChatCancellation, error) {
	return f.p.CancelV3(f.subject, id), nil
}

func chat3HostedRequest(t *testing.T, client *wire.ChatV3Client) wire.RequestV3 {
	t.Helper()
	window, err := client.GetHistoryWindow()
	if err != nil || window.Window == nil {
		t.Fatalf("history: %+v %v", window, err)
	}
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = window.Window.HistoryEpoch
	r.Target.Model = "anthropic/claude-sonnet-5"
	r.Content.Credential = "openrouter"
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}
	return r
}

func TestChat3GeneratedClientRunsOneAdmissionThroughReasoningAndAnswer(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.up.reasoning = []string{"thinking", " separately"}
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	request := chat3HostedRequest(t, client)
	a, err := client.Start(request)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start: %+v %v", a, err)
	}
	duplicate, err := client.Start(request)
	if err != nil || duplicate.Request != a.Request {
		t.Fatalf("replay: %+v %v", duplicate, err)
	}
	conflict := request
	conflict.Content.Messages = append([]wire.ChatMessage(nil), request.Content.Messages...)
	conflict.Content.Messages[0].Parts = []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "other"}}
	if changed, err := client.Start(conflict); err != nil || changed.Outcome != wire.ChatStartOutcomeConflict {
		t.Fatalf("different body: %+v %v", changed, err)
	}
	var answer, reasoning strings.Builder
	var end *wire.ChatResult
	var cursor int64
	for steps := 0; steps < 100 && end == nil; steps++ {
		page, err := client.Observe(a.Request, cursor, 256, 65536, 1000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v %v", page, err)
		}
		for _, event := range page.Events {
			if event.Sequence != cursor {
				t.Fatalf("sequence %d want %d", event.Sequence, cursor)
			}
			cursor++
			if event.Part != nil {
				switch event.Part.Kind {
				case wire.ChatPartKindText:
					answer.WriteString(event.Part.Text)
				case wire.ChatPartKindReasoning:
					reasoning.WriteString(event.Part.Text)
				}
			}
			if event.End != nil {
				end = event.End
			}
		}
		if page.Next != cursor {
			t.Fatalf("cursor %d want %d", page.Next, cursor)
		}
	}
	if end == nil || end.Outcome != wire.ChatResultOutcomeCompleted || answer.String() != "Hello!" || reasoning.String() != "thinking separately" {
		t.Fatalf("answer %q reasoning %q end %+v", answer.String(), reasoning.String(), end)
	}
	if end.ServedBy == nil || end.ServedBy.Place != wire.ServedPlaceHosted || end.ServedBy.Server != "openrouter" {
		t.Fatalf("serving fact %+v", end.ServedBy)
	}
	if end.Usage.Input == nil || *end.Usage.Input != 7 || end.Usage.Output == nil || *end.Usage.Output != 3 {
		t.Fatalf("usage %+v", end.Usage)
	}
	if len(f.up.posts()) != 1 {
		t.Fatalf("generated %d upstream calls", len(f.up.posts()))
	}
	foreign := caller
	foreign.Program = "other"
	if page := f.provider.ObserveV3(context.Background(), foreign, a.Request, 0, 256, 65536, 0); page.Outcome != wire.PageOutcomeUnknown {
		t.Fatal("foreign subject observed request")
	}
	if page := f.provider.Observe(context.Background(), caller, a.Request, 0, 256, 65536, 0); page.Outcome != wire.PageOutcomeUnknown {
		t.Fatal("legacy receiver observed chat3 events")
	}
	if cancel := f.provider.Cancel(caller, a.Request); cancel.Outcome != wire.CancelOutcomeUnknown {
		t.Fatal("legacy receiver cancelled chat3 request")
	}
	records := f.waitLogged(t, 1)
	if records[len(records)-1].TokensOut != 3 || records[len(records)-1].Program != caller.Program {
		t.Fatalf("accounting %+v", records)
	}
}

func TestChat3UnsupportedRequirementRefusesBeforeUpstream(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	r.Content.Options = &wire.ChatOptions{ToolChoice: &wire.ChatToolChoice{Kind: wire.ChatToolChoiceKindRequired}}
	a, err := client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeUnsupported || len(f.up.posts()) != 0 {
		t.Fatalf("unsupported: %+v %v posts %d", a, err, len(f.up.posts()))
	}
}

func TestChat3SealOmissionRecordsKeepAdmittedPlacement(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	f.up.reasoning = []string{strings.Repeat("r", chat3SealBytes+1)}
	records := collectRecords(f.provider)
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	request := chat3HostedRequest(t, client)
	request.Content.Messages = append([]wire.ChatMessage{{Role: wire.RoleAssistant, Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "previous answer"}}, Reasoning: []wire.ChatReplay{{Block: 0, Seal: wire.ReasoningSeal{Origin: router.WireAnthropicMessages, Bytes: []byte(`{"type":"thinking","signature":"s"}`)}}}}}, request.Content.Messages...)
	a, err := client.Start(request)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatal(a, err)
	}
	var cursor int64
	ended := false
	for steps := 0; steps < 100 && !ended; steps++ {
		page, err := client.Observe(a.Request, cursor, 256, 65536, 1000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatal(page.Outcome, err)
		}
		cursor = page.Next
		for _, event := range page.Events {
			if event.End != nil {
				ended = true
			}
		}
	}
	if !ended {
		t.Fatal("chat did not end")
	}
	omitted := 0
	for _, record := range records.wait(t, 3) {
		if record.Host != "openrouter" || record.Placement != wire.ServedPlaceHosted {
			t.Fatal(record)
		}
		if record.Outcome == "seal_omitted" {
			omitted++
		}
	}
	if omitted != 2 {
		t.Fatal("both replay and received-seal omissions must retain placement", omitted)
	}
}

func TestChat3HistoryRetainsRunningWorkBeyondMinimumHorizon(t *testing.T) {
	h, now, subject := chat3HistoryFixture(t)
	identity := chat3RequestFixture().Identity
	identity.HistoryEpoch = h.window().HistoryEpoch
	e, owner, _, _, _ := h.reserve(subject, identity, []byte("body"), now)
	if !owner {
		t.Fatal("not admission owner")
	}
	h.begin(e)
	h.complete(e, wire.ChatAdmission{Outcome: wire.ChatStartOutcomeAccepted, Request: "one"}, now)
	got, owner, outcome, _, _ := h.reserve(subject, identity, []byte("body"), now.Add(10*time.Minute))
	if owner || outcome != wire.ChatStartOutcomeAccepted || got != e {
		t.Fatal("long accepted work lost replay identity")
	}
	h.finish(e, false, now.Add(10*time.Minute))
	_, owner, outcome, _, _ = h.reserve(subject, identity, []byte("body"), now.Add(10*time.Minute+30*time.Second))
	if owner || outcome != wire.ChatStartOutcomeAccepted {
		t.Fatal("terminal retention missing")
	}
	_, owner, outcome, _, _ = h.reserve(subject, identity, []byte("body"), now.Add(11*time.Minute))
	if owner || outcome != wire.ChatStartOutcomeUnknown {
		t.Fatal("expired work admitted")
	}
}

func TestChat3WaitDeadlinePreservesAcceptedLoadingWorkAndHandle(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return ExecutionPreparation{Ready: true}
			case <-ctx.Done():
				return ExecutionPreparation{Reason: "cancel"}
			}
		}
	})
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	a, err := client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start: %+v %v", a, err)
	}
	<-entered
	wait, stop := context.WithCancel(context.Background())
	stop()
	page := f.provider.ObserveV3(wait, caller, a.Request, 0, 256, 65536, 1000)
	if page.Next != 0 || len(page.Events) != 0 || page.AtEnd {
		t.Fatalf("ended wait changed cursor: %+v", page)
	}
	if op := f.provider.visible(caller, a.Request); op == nil || op.stopped() != "" {
		t.Fatal("wait expiry cancelled accepted work")
	}
	if duplicate, err := client.Start(r); err != nil || duplicate.Request != a.Request {
		t.Fatalf("lost handle: %+v %v", duplicate, err)
	}
	close(release)
	var cursor int64
	for i := 0; i < 100; i++ {
		page, err := client.Observe(a.Request, cursor, 256, 65536, 1000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("resume: %+v %v", page, err)
		}
		cursor = page.Next
		if page.AtEnd {
			if len(f.up.posts()) != 1 {
				t.Fatal("resumed through extra inference")
			}
			return
		}
	}
	t.Fatal("accepted operation did not complete after load")
}

func TestChat3ExplicitCancelStopsAcceptedLoadingBeforeUpstream(t *testing.T) {
	entered := make(chan struct{})
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
			close(entered)
			<-ctx.Done()
			return ExecutionPreparation{Reason: "cancel"}
		}
	})
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	a, err := client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start: %+v %v", a, err)
	}
	<-entered
	cancel, err := client.Cancel(a.Request)
	if err != nil || cancel.Outcome != wire.CancelOutcomeCancelled || cancel.ResultOutcome == nil || *cancel.ResultOutcome != wire.ChatResultOutcomeCancelled {
		t.Fatalf("cancel: %+v %v", cancel, err)
	}
	page, err := client.Observe(a.Request, 0, 256, 65536, 0)
	if err != nil || !page.AtEnd || len(page.Events) != 1 || page.Events[0].End == nil || page.Events[0].End.Outcome != wire.ChatResultOutcomeCancelled {
		t.Fatalf("terminal: %+v %v", page, err)
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("cancelled load sent upstream")
	}
}

// JOB-A16: a preaccept authority refusal consumes no identity. The same
// persisted key may be enforced again after an explicit permission answer.
func TestChat3PreacceptAuthorityRefusalDoesNotConsumeIdentity(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	f.policy.mu.Lock()
	f.policy.rules[caller.Program+" "+ActionComplete+" host:openrouter"] = "not_granted"
	f.policy.mu.Unlock()
	denied, err := client.Start(r)
	if err != nil || denied.Outcome != wire.ChatStartOutcomeNotPermitted || denied.Refusal == nil || len(f.up.posts()) != 0 {
		t.Fatalf("denial: %+v %v", denied, err)
	}
	if len(f.provider.chat3History.open.entries) != 0 {
		t.Fatal("authority preflight consumed identity")
	}
	f.policy.mu.Lock()
	f.policy.rules[caller.Program+" "+ActionComplete+" host:openrouter"] = "permitted"
	f.policy.mu.Unlock()
	allowed, err := client.Start(r)
	if err != nil || allowed.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("same identity after answer: %+v %v", allowed, err)
	}
	f.policy.mu.Lock()
	asked := len(f.policy.asked)
	f.policy.rules[caller.Program+" "+ActionComplete+" host:openrouter"] = "not_granted"
	f.policy.mu.Unlock()
	replayed, err := client.Start(r)
	if err != nil || replayed.Request != allowed.Request {
		t.Fatalf("accepted identity changed: %+v %v", replayed, err)
	}
	f.policy.mu.Lock()
	after := len(f.policy.asked)
	f.policy.mu.Unlock()
	if after != asked {
		t.Fatal("accepted duplicate repeated authority preflight")
	}
}

func TestChat3ConcurrentPreflightsCommitOneAcceptedOperation(t *testing.T) {
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	var checks atomic.Int32
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.Decide = func(context.Context, Subject, string, string) (string, error) {
			if checks.Add(1) <= 2 {
				arrived <- struct{}{}
				<-release
			}
			return "permitted", nil
		}
	})
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	answers := make(chan wire.ChatAdmission, 2)
	for range 2 {
		go func() { answers <- f.provider.StartV3(context.Background(), caller, r) }()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("fresh preflights did not reach the barrier")
		}
	}
	close(release)
	a, b := <-answers, <-answers
	if a.Outcome != wire.ChatStartOutcomeAccepted || b.Outcome != wire.ChatStartOutcomeAccepted || a.Request != b.Request {
		t.Fatalf("CAS admissions: %+v %+v", a, b)
	}
	<-f.provider.visible(caller, a.Request).finished
	if len(f.up.posts()) != 1 {
		t.Fatalf("concurrent duplicates sent %d upstream calls", len(f.up.posts()))
	}
}

func TestChat3PolicyLocalOnlyRefusesHostedCandidateBeforeEveryEffect(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("hosted survey or dispatch") }))
	defer cloud.Close()
	local := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("privacy refusal fell back to local") }))
	defer local.Close()
	routes := router.New(router.NewHosted("cloud", cloud.URL, router.WireOpenAICompatible, "cloudcred"), router.OpenAICompatible("local", local.URL, "/v1", []string{"local-model"}))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) {
		t.Error("survey credential")
		return nil, nil
	})
	p, err := New(Config{ChatLogicalOwner: "fixture-job-owner", Router: routes, ResolveSelectionV2: selectionPolicy, Decide: func(context.Context, Subject, string, string) (string, error) {
		t.Error("authority admission")
		return "permitted", nil
	}, Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
		t.Error("credential application")
		return nil, "applied"
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = p.chat3History.window().HistoryEpoch
	r.Target = wire.ChatTarget{SelectionPolicy: "offline"}
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly}
	a := p.StartV3(context.Background(), caller, r)
	if a.Outcome != wire.ChatStartOutcomeNoHost || a.Reason != "selection:placement" || a.Request != "" {
		t.Fatalf("privacy refusal %+v", a)
	}
}

func TestChat3PolicyLocalOnlyRejectsConflictsAndDuplicateRestrictions(t *testing.T) {
	p, err := New(Config{ChatLogicalOwner: "fixture-job-owner", Router: router.New(), ResolveSelectionV2: selectionPolicy, Decide: func(context.Context, Subject, string, string) (string, error) {
		t.Error("authority admission")
		return "permitted", nil
	}, Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
		t.Error("credential application")
		return nil, "applied"
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, tc := range []struct {
		guarantees []wire.ChatGuarantee
		credential string
		remote     bool
	}{
		{[]wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly, wire.ChatGuaranteeLocalOnly}, "", false},
		{[]wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}, "", false},
		{[]wire.ChatGuarantee{wire.ChatGuaranteeNoRetention}, "", false},
		{[]wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly}, "credential", false},
		{[]wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly}, "", true},
	} {
		r := chat3RequestFixture()
		r.Identity.HistoryEpoch = p.chat3History.window().HistoryEpoch
		r.Target = wire.ChatTarget{SelectionPolicy: "offline"}
		r.Content.Guarantees = tc.guarantees
		r.Content.Credential = tc.credential
		ctx := context.Background()
		if tc.remote {
			ctx = WithExecutionPlacement(ctx, ExecutionRemote)
		}
		if a := p.StartV3(ctx, caller, r); a.Outcome != wire.ChatStartOutcomeInvalid || a.Request != "" {
			t.Fatalf("conflict %+v: %+v", tc, a)
		}
	}
}

func TestChat3PolicyLocalOnlyRunsAnAllowedLocalPolicy(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"local-model"}]}`)
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion"]}`)
		case "/v1/chat/completions":
			posts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"local\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	p, err := New(Config{ChatLogicalOwner: "fixture-job-owner", Router: router.New(router.Ollama(server.URL)), ResolveSelectionV2: func(context.Context, Subject, string) (wire.SelectionPolicyV2, bool, error) {
		return wire.SelectionPolicyV2{Name: "offline", Candidates: []wire.SelectionCandidateV2{{Model: "local-model", Placement: wire.CandidatePlacementLocal}}}, true, nil
	}, Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil }, Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
		t.Error("local credential")
		return nil, "applied"
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r := chat3RequestFixture()
	r.Identity.HistoryEpoch = p.chat3History.window().HistoryEpoch
	r.Target = wire.ChatTarget{SelectionPolicy: "offline"}
	r.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeLocalOnly}
	a := p.StartV3(context.Background(), caller, r)
	if a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("local admission %+v", a)
	}
	<-p.visible(caller, a.Request).finished
	if posts.Load() != 1 {
		t.Fatalf("local posts %d", posts.Load())
	}
}

func TestChat3OriginActualTokenGapEndsFailedWithoutExtraInference(t *testing.T) {
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) { cfg.Now = now; cfg.Idle = time.Hour })
	f.up.release = make(chan struct{})
	f.up.chunks = []string{"first"}
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	a, err := client.Start(chat3HostedRequest(t, client))
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start %+v %v", a, err)
	}
	page, err := client.Observe(a.Request, 0, 256, 65536, 1000)
	if err != nil || len(page.Events) == 0 {
		t.Fatalf("first token %+v %v", page, err)
	}
	mu.Lock()
	clock = clock.Add(180 * time.Second)
	mu.Unlock()
	op := f.provider.visible(caller, a.Request)
	select {
	case <-op.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("actual producer did not stop stalled source")
	}
	page, err = client.Observe(a.Request, page.Next, 256, 65536, 0)
	if err != nil || !page.AtEnd {
		t.Fatalf("terminal %+v %v", page, err)
	}
	end := page.Events[len(page.Events)-1].End
	if end == nil || end.Outcome != wire.ChatResultOutcomeFailed || end.Cause != "stalled" || len(f.up.posts()) != 1 {
		t.Fatalf("origin terminal %+v posts %d", end, len(f.up.posts()))
	}
}

func TestChat3OriginReportsActualLoadPhaseBeginningsWithoutCounts(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
			for _, phase := range []string{"metadata", "tensors", "context", "graph"} {
				ReportExecutionProgress(ctx, phase)
			}
			return ExecutionPreparation{Ready: true}
		}
	})
	f.up.reasoning = []string{"thinking"}
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	a, err := client.Start(chat3HostedRequest(t, client))
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start %+v %v", a, err)
	}
	<-f.provider.visible(caller, a.Request).finished
	page, err := client.Observe(a.Request, 0, 256, 65536, 0)
	if err != nil || !page.AtEnd {
		t.Fatalf("events %+v %v", page, err)
	}
	var phases []string
	for _, event := range page.Events {
		if p := event.Progress; p != nil {
			phases = append(phases, string(p.Phase))
			if p.Done != nil || p.Total != nil || p.Unit != "" {
				t.Fatal("begun phase fabricated completed work")
			}
		}
	}
	if strings.Join(phases, ",") != "metadata,tensors,context,graph" || len(f.up.posts()) != 1 {
		t.Fatalf("phases %v posts %d", phases, len(f.up.posts()))
	}
}
