package inference

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func chat3Answer(t *testing.T, f *fixture, r wire.RequestV3) (string, *wire.ChatResult) {
	t.Helper()
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	a, err := client.Start(r)
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start: %+v %v", a, err)
	}
	var answer strings.Builder
	var cursor int64
	for steps := 0; steps < 200; steps++ {
		page, err := client.Observe(a.Request, cursor, 256, 65536, 1000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v %v", page, err)
		}
		for _, event := range page.Events {
			if event.Part != nil && event.Part.Kind == wire.ChatPartKindText {
				answer.WriteString(event.Part.Text)
			}
			if event.End != nil {
				return answer.String(), event.End
			}
		}
		cursor = page.Next
	}
	t.Fatal("no terminal")
	return "", nil
}

// Architect ruling 2026-11-12, reasoning effort §2: a host whose catalogue
// declares no reasoning control gets no reasoning parameter; the answer
// still arrives, and reasoning_applied reads not_supported for the effort and
// the budget. An extension the model does not take is dropped and named.
func TestChat3ReasoningOnAnUnsupportedHostAnswersAndSaysNotSupported(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	r := chat3HostedRequest(t, client)
	budget := int64(2048)
	r.Content.Options = &wire.ChatOptions{Reasoning: &wire.ChatReasoning{Effort: wire.ChatReasoningEffortHigh, Budget: &budget}}
	r.Content.Extensions = map[string]string{"llama.cpp/enable_thinking": "false"}
	answer, end := chat3Answer(t, f, r)
	if end.Outcome != wire.ChatResultOutcomeCompleted || answer != "Hello!" {
		t.Fatalf("answer %q end %+v", answer, end)
	}
	a := end.ReasoningApplied
	if a == nil || a.Effort == nil || *a.Effort != wire.ChatReasoningEffortHigh || a.Support != wire.ChatReasoningSupportNotSupported ||
		a.BudgetSupport == nil || *a.BudgetSupport != wire.ChatReasoningSupportNotSupported {
		t.Fatalf("reasoning_applied %+v", a)
	}
	if len(end.DroppedExtensions) != 1 || end.DroppedExtensions[0] != "llama.cpp/enable_thinking" {
		t.Fatalf("dropped %v", end.DroppedExtensions)
	}
	posts := f.up.posts()
	if len(posts) != 1 {
		t.Fatalf("posts %d", len(posts))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(posts[0].body), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"reasoning_effort", "reasoning_budget_tokens", "chat_template_kwargs", "thinking"} {
		if _, sent := body[field]; sent {
			t.Fatalf("sent %s: %s", field, posts[0].body)
		}
	}
}

// The DeepSeek mapping from its API docs, read 2026-11-12
// (api-docs.deepseek.com/api/create-chat-completion): reasoning_effort takes
// none, low, high and max, reads medium as high, defaults to high, and has no
// reasoning budget. The hosted catalogue's deepseek-flash entry is the one
// the router lists; the body is the one the provider posts.
func TestChat3ReasoningMapsOntoDeepSeeksReasoningEffort(t *testing.T) {
	host := router.NewHosted("deepseek", "https://api.deepseek.com/v1", router.WireOpenAICompatible, "deepseek")
	caps := host.CapabilitiesFor("deepseek-flash")
	if caps == nil || caps.Reasoning == nil || len(caps.Reasoning.Efforts) != 4 || caps.Reasoning.Budget ||
		caps.Reasoning.DefaultEffort == nil || *caps.Reasoning.DefaultEffort != "high" || strings.Join(caps.Input, "+") != "text+image" {
		t.Fatalf("deepseek-flash capabilities %+v", caps)
	}
	if pro := host.CapabilitiesFor("deepseek-v4-pro"); pro == nil || strings.Join(pro.Input, "+") != "text" {
		t.Fatalf("deepseek-v4-pro capabilities %+v", pro)
	}
	budget := int64(1000)
	for _, tc := range []struct {
		effort  wire.ChatReasoningEffort
		sent    string
		support wire.ChatReasoningSupport
	}{
		{wire.ChatReasoningEffortNone, "none", wire.ChatReasoningSupportApplied},
		{wire.ChatReasoningEffortLow, "low", wire.ChatReasoningSupportApplied},
		{wire.ChatReasoningEffortMedium, "high", wire.ChatReasoningSupportApproximated},
		{wire.ChatReasoningEffortHigh, "high", wire.ChatReasoningSupportApplied},
	} {
		options := &wire.ChatOptions{MaxOutput: 512, Reasoning: &wire.ChatReasoning{Effort: tc.effort, Budget: &budget}}
		plan := planChat3Reasoning(host, "deepseek-flash", caps, options, nil)
		body := chat3OpenAIBody("deepseek-flash", wire.Request{Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hi"}}}}}, options, nil, nil)
		chat3ApplyReasoning(withChat3Reasoning(context.Background(), plan), body)
		if body["reasoning_effort"] != tc.sent || body["thinking"] != nil || body["reasoning_budget_tokens"] != nil {
			t.Fatalf("%s: body %v", tc.effort, body)
		}
		a := plan.applied
		if a == nil || a.Support != tc.support || a.BudgetSupport == nil || *a.BudgetSupport != wire.ChatReasoningSupportNotSupported {
			t.Fatalf("%s: applied %+v", tc.effort, a)
		}
	}
	// Nothing named: DeepSeek's own default, with no parameter sent.
	plan := planChat3Reasoning(host, "deepseek-flash", caps, &wire.ChatOptions{}, nil)
	if plan.applied != nil || len(plan.fields) != 0 {
		t.Fatalf("default plan %+v", plan)
	}
	// DeepSeek's own words travel as the native extension, which wins.
	plan = planChat3Reasoning(host, "deepseek-flash", caps, &wire.ChatOptions{Reasoning: &wire.ChatReasoning{Effort: wire.ChatReasoningEffortLow}},
		map[string]string{reasoningEffortExtension: "max"})
	body := chat3OpenAIBody("deepseek-flash", wire.Request{Extensions: map[string]string{reasoningEffortExtension: "max"}}, nil, nil, nil)
	chat3ApplyReasoning(withChat3Reasoning(context.Background(), plan), body)
	if body["reasoning_effort"] != "max" || plan.applied == nil || plan.applied.Support != wire.ChatReasoningSupportOverridden {
		t.Fatalf("native word: body %v applied %+v", body, plan.applied)
	}
}

// An Anthropic Messages model whose catalogue table names thinking budgets
// takes the effort's budget, a named budget wins, none disables thinking,
// and a limit too small for the smallest budget reads approximated.
func TestChat3ReasoningMapsOntoAnthropicThinkingBudgets(t *testing.T) {
	host := router.NewHosted("anthropic", "https://api.anthropic.com", router.WireAnthropicMessages, "anthropic")
	host.DeclareModel("claude", nil, &router.ReasoningControl{Kind: router.ReasoningThinkingBudget, Budgets: map[string]int64{"low": 2048, "medium": 8192, "high": 16384}})
	plan := planChat3Reasoning(host, "claude", nil, &wire.ChatOptions{MaxOutput: 32000, Reasoning: &wire.ChatReasoning{Effort: wire.ChatReasoningEffortMedium}}, nil)
	if thinking, _ := plan.fields["thinking"].(map[string]any); thinking["budget_tokens"] != int64(8192) || plan.applied.Support != wire.ChatReasoningSupportApplied {
		t.Fatalf("medium %+v", plan)
	}
	plan = planChat3Reasoning(host, "claude", nil, &wire.ChatOptions{Reasoning: &wire.ChatReasoning{Effort: wire.ChatReasoningEffortNone}}, nil)
	if thinking, _ := plan.fields["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Fatalf("none %+v", plan)
	}
	plan = planChat3Reasoning(host, "claude", nil, &wire.ChatOptions{MaxOutput: 600, Reasoning: &wire.ChatReasoning{Effort: wire.ChatReasoningEffortHigh}}, nil)
	if thinking, _ := plan.fields["thinking"].(map[string]any); thinking["type"] != "disabled" || plan.applied.Support != wire.ChatReasoningSupportApproximated {
		t.Fatalf("small limit %+v", plan)
	}
}
