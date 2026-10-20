package inference

import (
	"context"
	"slices"
	"strconv"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// chat3Reasoning is one chat@3 request's reasoning as its model takes it
// (architect ruling 2026-11-12, reasoning effort): the body fields its wire
// gets, and what the answer reports in reasoning_applied. An unsupported
// control never refuses the call.
type chat3Reasoning struct {
	// fields are set on the upstream body, after the request's own.
	fields map[string]any
	// kwargs are merged into chat_template_kwargs (llama.cpp).
	kwargs  map[string]any
	applied *wire.ChatReasoningApplied
}

// reasoningExtensions are the native extensions that choose reasoning
// themselves. One a model supports wins over ChatOptions.reasoning, which
// is then reported overridden (ruling, native pass-through (b)).
var reasoningExtensions = []string{reasoningEffortExtension, router.ExtensionLlamaCppEnableThinking, router.ExtensionLlamaCppReasoningBudget}

// modelCapabilities are host's facts for model: the host's own declaration
// or the hosted catalogue entry, then what the router's latest survey states
// for that alias. Nil when neither states any.
func (p *Provider) modelCapabilities(host *router.Host, model string) *router.ModelCapabilities {
	declared := host.CapabilitiesFor(model)
	var surveyed *router.ModelCapabilities
	if p.cfg.Router != nil && host != nil {
		surveyed = p.cfg.Router.AliasCapabilities(host.Name, model)
	}
	if declared == nil {
		return surveyed
	}
	if surveyed != nil && len(declared.Extensions) == 0 {
		declared.Extensions = surveyed.Extensions
	}
	return declared
}

// wireExtensions are the extension keys a wire maps for every model.
func wireExtensions(host *router.Host) map[string]bool {
	known := map[string]bool{}
	// Anthropic Messages maps no extension key.
	if host != nil && (host.Wire == router.WireOpenAICompatible || host.Wire == "") {
		for key := range openAIExtensions {
			known[key] = true
		}
	}
	return known
}

// supportsExtension reports whether model on host takes key: its wire maps
// it for every model, or the model's capabilities name it.
func supportsExtension(host *router.Host, caps *router.ModelCapabilities, key string) bool {
	if wireExtensions(host)[key] {
		return true
	}
	return caps != nil && slices.Contains(caps.Extensions, key)
}

// requiredBeyondModel is required without the keys the model itself
// supports: what is left is judged by the wire's or the registration's own
// inventory, which refuses it naming the key.
func requiredBeyondModel(caps *router.ModelCapabilities, required []string) []string {
	if caps == nil || len(caps.Extensions) == 0 {
		return required
	}
	var out []string
	for _, key := range required {
		if !slices.Contains(caps.Extensions, key) {
			out = append(out, key)
		}
	}
	return out
}

// chat3SupportedExtensions keeps the request extensions model takes and
// names the others, which are dropped and reported (ruling, native
// pass-through (a)). A required key the model does not take was refused at
// admission. Reserved OA keys stay.
func chat3SupportedExtensions(host *router.Host, caps *router.ModelCapabilities, values map[string]string) (map[string]string, []string) {
	if len(values) == 0 {
		return values, nil
	}
	kept := make(map[string]string, len(values))
	var dropped []string
	for key, value := range values {
		if strings.HasPrefix(key, ReservedExtensionPrefix) || supportsExtension(host, caps, key) {
			kept[key] = value
			continue
		}
		dropped = append(dropped, key)
	}
	slices.Sort(dropped)
	return kept, dropped
}

// planChat3Reasoning maps the caller's reasoning choice onto model's wire on
// host (ruling §2):
//   - a llama.cpp engine behind modelhost gets reasoning_effort, and
//     reasoning_budget_tokens for a named budget; modelhost turns none into
//     thinking off and bounds every other effort (reserveAnswer);
//   - a host whose catalogue entry names reasoning_effort gets its own word
//     for the effort, approximated when that word differs;
//   - an Anthropic host gets a thinking budget from the catalogue table;
//   - any other model gets nothing, reported not_supported.
//
// With nothing named, a model served on this machine reasons at its bounded
// default, reported; a hosted model gets its provider's default and no
// parameter. A native reasoning extension the model takes wins.
func planChat3Reasoning(host *router.Host, model string, caps *router.ModelCapabilities, options *wire.ChatOptions, extensions map[string]string) chat3Reasoning {
	var asked *wire.ChatReasoning
	if options != nil {
		asked = options.Reasoning
	}
	var maxOutput int64
	if options != nil {
		maxOutput = options.MaxOutput
	}
	native := false
	for _, key := range reasoningExtensions {
		if _, set := extensions[key]; set && supportsExtension(host, caps, key) {
			native = true
		}
	}
	plan := chat3Reasoning{}
	control, known := host.ReasoningFor(model)
	if native {
		plan.nativeExtensions(control, extensions)
		if asked != nil {
			effort := asked.Effort
			plan.applied = &wire.ChatReasoningApplied{Effort: &effort, Support: wire.ChatReasoningSupportOverridden}
			if asked.Budget != nil {
				overridden := wire.ChatReasoningSupportOverridden
				plan.applied.BudgetSupport = &overridden
			}
		}
		return plan
	}
	if !known {
		if asked != nil {
			effort := asked.Effort
			plan.applied = &wire.ChatReasoningApplied{Effort: &effort, Support: wire.ChatReasoningSupportNotSupported}
			plan.budgetUnsupported(asked)
		}
		return plan
	}
	if asked == nil {
		// The model's own default: bounded on this machine and reported;
		// a hosted provider's default with no parameter sent.
		if control.Kind == router.ReasoningLlamaCpp && control.Default != "" {
			if effort, ok := wire.ParseChatReasoningEffort(control.Default); ok {
				plan.applied = &wire.ChatReasoningApplied{Effort: &effort, Support: wire.ChatReasoningSupportApplied}
			}
		}
		return plan
	}
	effort := asked.Effort
	word := effort.String()
	plan.applied = &wire.ChatReasoningApplied{Effort: &effort, Support: wire.ChatReasoningSupportApplied}
	switch control.Kind {
	case router.ReasoningLlamaCpp:
		plan.fields = map[string]any{"reasoning_effort": word}
		if asked.Budget != nil && effort != wire.ChatReasoningEffortNone {
			plan.fields["reasoning_budget_tokens"] = *asked.Budget
			budget, applied := *asked.Budget, wire.ChatReasoningSupportApplied
			plan.applied.Budget, plan.applied.BudgetSupport = &budget, &applied
		} else {
			plan.budgetUnsupported(asked)
		}
	case router.ReasoningEffortField:
		value := control.Values[word]
		if value == "" {
			plan.applied.Support = wire.ChatReasoningSupportNotSupported
			plan.budgetUnsupported(asked)
			return plan
		}
		plan.fields = map[string]any{"reasoning_effort": value}
		if value != word {
			plan.applied.Support = wire.ChatReasoningSupportApproximated
		}
		plan.budgetUnsupported(asked)
	case router.ReasoningThinkingBudget:
		if effort == wire.ChatReasoningEffortNone {
			plan.fields = map[string]any{"thinking": map[string]any{"type": "disabled"}}
			plan.budgetUnsupported(asked)
			return plan
		}
		budget, budgetSupport := control.Budgets[word], wire.ChatReasoningSupportApplied
		if asked.Budget != nil {
			budget = *asked.Budget
		}
		// Anthropic takes a budget of at least 1024 tokens below max_tokens;
		// a smaller limit gets the nearest budget that fits.
		if maxOutput > 0 && budget >= maxOutput {
			budget = maxOutput - 1
			budgetSupport = wire.ChatReasoningSupportApproximated
			plan.applied.Support = wire.ChatReasoningSupportApproximated
		}
		if budget < 1024 {
			plan.fields = map[string]any{"thinking": map[string]any{"type": "disabled"}}
			plan.applied.Support = wire.ChatReasoningSupportApproximated
			none := wire.ChatReasoningEffortNone
			plan.applied.Effort = &none
			plan.budgetUnsupported(asked)
			return plan
		}
		plan.fields = map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": budget}}
		plan.applied.Budget = &budget
		if asked.Budget != nil {
			plan.applied.BudgetSupport = &budgetSupport
		}
	default:
		plan.applied.Support = wire.ChatReasoningSupportNotSupported
		plan.budgetUnsupported(asked)
	}
	return plan
}

func (plan *chat3Reasoning) budgetUnsupported(asked *wire.ChatReasoning) {
	if asked != nil && asked.Budget != nil && plan.applied != nil {
		unsupported := wire.ChatReasoningSupportNotSupported
		plan.applied.BudgetSupport = &unsupported
	}
}

// nativeExtensions maps llama.cpp's own reasoning extensions onto its body:
// enable_thinking is chat_template_kwargs' switch and reasoning_budget is
// reasoning_budget_tokens. openai/reasoning_effort maps through the wire's
// own extension table.
func (plan *chat3Reasoning) nativeExtensions(control router.ReasoningControl, extensions map[string]string) {
	if value, set := extensions[router.ExtensionLlamaCppEnableThinking]; set {
		if on, err := strconv.ParseBool(value); err == nil {
			plan.kwargs = map[string]any{"enable_thinking": on}
		}
	}
	if value, set := extensions[router.ExtensionLlamaCppReasoningBudget]; set {
		if budget, err := strconv.ParseInt(value, 10, 64); err == nil && budget > 0 && budget <= 1<<20 {
			plan.fields = map[string]any{"reasoning_budget_tokens": budget}
		}
	}
}

// apply sets the plan's fields on an upstream body.
func (plan chat3Reasoning) apply(body map[string]any) {
	for key, value := range plan.fields {
		body[key] = value
	}
	if len(plan.kwargs) > 0 {
		kwargs, _ := body["chat_template_kwargs"].(map[string]any)
		if kwargs == nil {
			kwargs = map[string]any{}
		}
		for key, value := range plan.kwargs {
			kwargs[key] = value
		}
		body["chat_template_kwargs"] = kwargs
	}
}

type chat3ReasoningKey struct{}

// withChat3Reasoning carries one attempt's reasoning plan to the body
// builders.
func withChat3Reasoning(ctx context.Context, plan chat3Reasoning) context.Context {
	return context.WithValue(ctx, chat3ReasoningKey{}, plan)
}

// chat3ApplyReasoning sets the attempt's planned reasoning fields on body.
func chat3ApplyReasoning(ctx context.Context, body map[string]any) {
	if plan, ok := ctx.Value(chat3ReasoningKey{}).(chat3Reasoning); ok {
		plan.apply(body)
	}
}

// chat3NamesReasoning reports whether the caller chose reasoning itself, in
// ChatOptions or a native reasoning extension; such a request keeps its own
// choice and is never asked again without reasoning.
func chat3NamesReasoning(request wire.RequestV3) bool {
	if o := request.Content.Options; o != nil && o.Reasoning != nil {
		return true
	}
	for _, key := range reasoningExtensions {
		if _, set := request.Content.Extensions[key]; set {
			return true
		}
	}
	return false
}
