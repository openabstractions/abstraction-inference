package inference

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	rights "github.com/openabstractions/abstraction-rights/go/abstraction/rights/api"
	router "github.com/openabstractions/abstraction-router/go"
	watch "github.com/openabstractions/abstraction-watch/go"
)

type chat3RunKey struct{}
type chat3Run struct {
	request   wire.RequestV3
	history   *chat3HistoryEntry
	events    []wire.ChatEvent // operation.mu owns the retained window
	result    chat3UpstreamResult
	body      []byte
	admission *wire.ChatAdmission
	relayEnd  *wire.ChatResult
	// reasoning and dropped are what this provider's own upstream attempt
	// reports in the terminal: the reasoning the answer ran with and the
	// request extensions the model does not take (chat3_reasoning.go).
	reasoning *wire.ChatReasoningApplied
	dropped   []string
	relayed   bool // terminal came verbatim from the serving runtime
	terminal  *wire.ChatResult
}

// BindChatV3LogicalOwner binds the runtime's Job owner or a designated leaf's
// declared provider name through trusted composition. Rebinding is refused.
func (p *Provider) BindChatV3LogicalOwner(owner string) error { return p.chat3History.bindOwner(owner) }

// GetChatHistoryWindow reads identity metadata for a proven caller without work.
func (p *Provider) GetChatHistoryWindow(subject Subject) wire.ChatHistoryRead {
	if subject.Account == "" || subject.Program == "" {
		return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeForbidden}
	}
	if !p.chat3History.bound() {
		return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeUnavailable}
	}
	window := p.chat3History.window()
	return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeAvailable, Window: &window}
}

// StartV3 shares routing, authorization, admission, standing and resource owners
// with existing chat. Identity reservation precedes that path and is never retry.
func (p *Provider) StartV3(ctx context.Context, subject Subject, request wire.RequestV3) wire.ChatAdmission {
	refuse := func(outcome wire.ChatStartOutcome, reason string) wire.ChatAdmission {
		p.record(Record{ChatVersion: 3, Account: subject.Account, Program: subject.Program, Outcome: outcome.String(), Reason: reason})
		return wire.ChatAdmission{Outcome: outcome, Reason: reason}
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.ChatStartOutcomeForbidden, "caller")
	}
	if !p.chat3History.bound() {
		return refuse(wire.ChatStartOutcomeUnavailable, "history:owner_unbound")
	}
	if _, err := wire.ToJSON(request); err != nil {
		return refuse(wire.ChatStartOutcomeInvalid, "request")
	}
	if reason := validateChat3Request(request); reason != "" {
		return refuse(wire.ChatStartOutcomeInvalid, reason)
	}
	if chat3HasGuarantee(request, wire.ChatGuaranteeFreeCapacity) && !p.supportsFreeCapacity() {
		relay := false
		for _, host := range p.cfg.Router.Hosts() {
			if host.Wire == router.WireNative || host.Wire == router.WireRemote {
				relay = true
				break
			}
		}
		if !relay {
			return refuse(wire.ChatStartOutcomeUnsupported, "guarantee:free-capacity")
		}
	}
	if _, reason := receivedChatCacheBinding(ctx, subject, request.Content.Extensions, request.Conversation, chat3NoRetention(request.Content.Guarantees)); reason != "" {
		return refuse(wire.ChatStartOutcomeInvalid, reason)
	}
	legacy, reason := chat3Legacy(request)
	if reason != "" {
		return refuse(wire.ChatStartOutcomeUnsupported, reason)
	}
	encoded, err := wire.ToJSON(request)
	if err != nil {
		return refuse(wire.ChatStartOutcomeInvalid, "request")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return refuse(wire.ChatStartOutcomeInvalid, "request")
	}
	delete(fields, "identity")
	body, err := json.Marshal(fields)
	if err != nil {
		return refuse(wire.ChatStartOutcomeInvalid, "request")
	}
	e, outcome := p.chat3History.lookup(subject, request.Identity, body, p.cfg.Now())
	if outcome != wire.ChatStartOutcomeAccepted {
		return refuse(outcome, "history:"+outcome.String())
	}
	if e != nil {
		select {
		case <-e.done:
			return e.admission
		case <-ctx.Done():
			return refuse(wire.ChatStartOutcomeUnavailable, "history:wait")
		}
	}
	state := &chat3Run{request: request, body: body}
	ctx = context.WithValue(ctx, chat3RunKey{}, state)
	var old wire.AdmissionV2
	if request.Target.SelectionPolicy != "" && chat3HasGuarantee(request, wire.ChatGuaranteeLocalOnly) {
		old = p.StartV2WithGuarantees(ctx, subject, legacy, []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly})
	} else {
		old = p.StartV2(ctx, subject, legacy)
	}
	if state.admission != nil {
		return *state.admission
	}
	outcome, ok := wire.ParseChatStartOutcome(old.Outcome.String())
	if !ok {
		outcome = wire.ChatStartOutcomeUnavailable
	}
	if old.Outcome == wire.StartOutcomeUnsupportedFeature {
		outcome = wire.ChatStartOutcomeUnsupported
	}
	a := wire.ChatAdmission{Outcome: outcome, Reason: strings.Replace(old.Reason, "ceiling:", "budget:", 1),
		Request: old.Operation, Server: old.Host, Model: old.Model, RetentionMs: old.RetentionMs,
		IdleMs: old.IdleMs, RetainedDeltas: old.RetainedDeltas, PreferredModel: old.PreferredModel, SelectionReason: old.SelectionReason}
	if a.Outcome == wire.ChatStartOutcomeNotPermitted {
		a.Refusal = chat3Authority(a.Reason)
	}
	if state.history != nil {
		if a.Outcome == wire.ChatStartOutcomeAccepted {
			p.chat3History.complete(state.history, a, p.cfg.Now())
		} else {
			p.chat3History.abort(state.history, a)
		}
	}
	return a
}

// commitChat3 arbitrates concurrent fresh preflights at the operation commit.
// A duplicate recovers the original admission and can never start another run.
func (p *Provider) commitChat3(ctx context.Context, subject Subject, state *chat3Run) bool {
	e, owner, outcome, retry, err := p.chat3History.reserve(subject, state.request.Identity, state.body, p.cfg.Now())
	if err != nil {
		p.report(err)
		outcome = wire.ChatStartOutcomeUnavailable
	}
	if owner {
		state.history = e
		p.chat3History.begin(e)
		return true
	}
	a := wire.ChatAdmission{Outcome: outcome, Reason: "history:" + outcome.String(), RetryAfterMs: chat3RetryHint(outcome, retry)}
	if e != nil {
		select {
		case <-e.done:
			a = e.admission
		case <-ctx.Done():
			a = wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnavailable, Reason: "history:wait"}
		}
	}
	state.admission = &a
	return false
}

func chat3Authority(reason string) *rights.RefusalDetail {
	if !strings.HasPrefix(reason, "rights:") {
		return nil
	}
	decision, ok := rights.ParseRefusalDecision(strings.TrimPrefix(reason, "rights:"))
	if !ok {
		return nil
	}
	return &rights.RefusalDetail{Source: rights.RefusalSourceRights, Decision: decision, Ask: rights.RefusalAskNone}
}

func chat3Legacy(request wire.RequestV3) (wire.RequestV2, string) {
	if request.ModelRef != "" {
		return wire.RequestV2{}, "feature:model_ref"
	}
	c := request.Content
	out := wire.RequestV2{Target: wire.Target{Model: request.Target.Model, SelectionPolicy: request.Target.SelectionPolicy},
		Content: wire.RequestContent{Extensions: c.Extensions, RequiredExtensions: c.RequiredExtensions, Credential: c.Credential}}
	for _, guarantee := range c.Guarantees {
		if guarantee == wire.ChatGuaranteeFreeCapacity {
			continue
		}
		if request.Target.SelectionPolicy != "" {
			continue
		}
		if guarantee == wire.ChatGuaranteeNoRetention {
			return out, "guarantee:no-retention"
		}
		out.Content.Guarantees = append(out.Content.Guarantees, wire.RequestGuarantee(guarantee))
	}
	for _, message := range c.Messages {
		m := wire.Message{Role: message.Role}
		for _, part := range message.Parts {
			m.Parts = append(m.Parts, wire.Part{Kind: part.Kind, Text: part.Text, Digest: part.Digest,
				MediaType: part.MediaType, CallID: part.CallID, Name: part.Name, Arguments: part.Arguments})
		}
		out.Content.Messages = append(out.Content.Messages, m)
	}
	for _, tool := range c.Tools {
		out.Content.Tools = append(out.Content.Tools, wire.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	if c.Options != nil {
		o := c.Options
		out.Content.Options = &wire.Options{MaxOutput: o.MaxOutput, Temperature: o.Temperature, Stop: o.Stop, JSONSchema: o.JSONSchema}
	}
	return out, ""
}

func validateChat3Request(request wire.RequestV3) string {
	guarantees := map[wire.ChatGuarantee]bool{}
	for _, guarantee := range request.Content.Guarantees {
		if guarantees[guarantee] {
			return "guarantees:duplicate"
		}
		guarantees[guarantee] = true
	}
	if request.Target.SelectionPolicy != "" {
		g := request.Content.Guarantees
		if request.Content.Credential != "" || len(g) > 2 {
			return "selection:override"
		}
		seen := map[wire.ChatGuarantee]bool{}
		for _, guarantee := range g {
			if seen[guarantee] || guarantee != wire.ChatGuaranteeLocalOnly && guarantee != wire.ChatGuaranteeFreeCapacity {
				return "selection:override"
			}
			seen[guarantee] = true
		}
	}
	for _, message := range request.Content.Messages {
		if len(message.Parts)+len(message.Reasoning) > 64 {
			return "messages:parts"
		}
		if len(message.Reasoning) != 0 && message.Role != wire.RoleAssistant {
			return "reasoning:role"
		}
		blocks := map[int64]bool{}
		for _, replay := range message.Reasoning {
			if blocks[replay.Block] {
				return "reasoning:block"
			}
			blocks[replay.Block] = true
		}
	}
	if options := request.Content.Options; options != nil && options.ToolChoice != nil {
		choice := options.ToolChoice
		if choice.Kind == wire.ChatToolChoiceKindNamed {
			found := false
			for _, tool := range request.Content.Tools {
				found = found || tool.Name == choice.Name
			}
			if !found {
				return "tool_choice:name"
			}
		} else if choice.Name != "" {
			return "tool_choice:name"
		}
	}
	return ""
}

func chat3Supported(host *router.Host, request wire.RequestV3) string {
	if host.Wire != router.WireOpenAICompatible && host.Wire != router.WireAnthropicMessages && host.Wire != "" && host.Wire != router.WireNative && host.Wire != router.WireRemote {
		return "wire:chat3"
	}
	// These controls require a router-owned per-model declaration. No current
	// declaration attests them; refusing is safer than approximating a requirement.
	for _, tool := range request.Content.Tools {
		if tool.Strict != nil && *tool.Strict {
			return "feature:strict-tools"
		}
	}
	if o := request.Content.Options; o != nil {
		if o.ToolChoice != nil && (o.ToolChoice.Kind == wire.ChatToolChoiceKindRequired || o.ToolChoice.Kind == wire.ChatToolChoiceKindNamed) {
			return "feature:tool-choice"
		}
		if o.ParallelTools != nil && *o.ParallelTools {
			return "feature:parallel-tools"
		}
	}
	return ""
}

func (op *operation) appendChat3Locked(event wire.ChatEvent) {
	event.Sequence = op.next
	op.next++
	op.chat3.events = append(op.chat3.events, event)
	if over := len(op.chat3.events) - op.capacity; over > 0 {
		op.chat3.events = append([]wire.ChatEvent(nil), op.chat3.events[over:]...)
		op.base += int64(over)
	}
	op.observe.Advance()
}

func (op *operation) emitChat3(event wire.ChatEvent, now time.Time) error {
	// Sequence's largest valid value keeps the preflight valid at every cursor.
	event.Sequence = 1<<63 - 1
	if _, err := wire.ToJSON(event); err != nil {
		return err
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.ended {
		return errors.New("chat request ended")
	}
	if event.Part != nil && (event.Part.Text != "" || event.Part.Arguments != "" || event.Part.Name != "") {
		op.timing.token(now)
		op.generation.progress(now, true, false)
	}
	op.appendChat3Locked(event)
	return nil
}

func (op *operation) endChat3Locked(end wire.Reply, now time.Time) {
	result := wire.ChatResult{Outcome: wire.ChatResultOutcomeFailed, Cause: end.Reason, Usage: op.chat3.result.Usage, Cost: op.chat3.result.Cost, Timing: op.timing.chatTiming()}
	if end.Outcome == wire.ReplyOutcomeUnavailable {
		result.RetryAfterMs = op.chat3.result.RetryAfterMs
	}
	if end.Outcome == wire.ReplyOutcomeCompleted {
		result.Outcome = wire.ChatResultOutcomeCompleted
		finish := op.chat3.result.Finish
		result.FinishReason = &finish
		if op.chat3.result.StopSequence != nil {
			result.StopSequence = *op.chat3.result.StopSequence
		}
	} else if end.Outcome == wire.ReplyOutcomeCancelled {
		result.Outcome = wire.ChatResultOutcomeCancelled
	}
	result.ServedBy = &wire.ServedBy{Place: op.placement, Server: op.host.Name, Machine: op.host.Domain}
	result.ReasoningApplied, result.DroppedExtensions = op.chat3.reasoning, op.chat3.dropped
	if op.chat3.relayEnd != nil && ((end.Outcome == wire.ReplyOutcomeCompleted && op.chat3.relayEnd.Outcome == wire.ChatResultOutcomeCompleted) || (end.Outcome == wire.ReplyOutcomeCancelled && op.chat3.relayEnd.Outcome == wire.ChatResultOutcomeCancelled) || (end.Outcome == wire.ReplyOutcomeUnavailable && op.chat3.relayEnd.Outcome == wire.ChatResultOutcomeFailed)) {
		result = *op.chat3.relayEnd
		op.chat3.relayed = true
	}
	op.chat3.terminal = &result
	op.appendChat3Locked(wire.ChatEvent{Kind: wire.ChatEventKindEnd, End: &result})
	op.ended, op.endAt, op.outcome = true, now, end.Outcome
}

func (p *Provider) runChat3(ctx context.Context, op *operation, req wire.Request, images map[string][]byte, headers map[string]string) wire.Reply {
	if op.host.Wire == router.WireNative || op.host.Wire == router.WireRemote {
		return p.relayChat3(ctx, op, req, images)
	}
	ctx = withChat3Replay(ctx, op.chat3.request.Content.Messages, func(audit chat3ReplayAudit) {
		p.record(Record{ChatVersion: 3, Operation: op.id, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name, Placement: op.placement, Outcome: "seal_omitted", Reason: string(audit.Reason)})
	})
	seals := map[int64]*chat3SealBuilder{}
	kinds := map[int64]wire.ChatPartKind{}
	// Apply the existing INF-Q8 content classifier at the shared terminal
	// owner, after either wire decoder. Seals never count as answer text.
	reasoning, text, called := false, false, false
	part := func(part wire.ChatPart) error {
		switch part.Kind {
		case wire.ChatPartKindReasoning, wire.ChatPartKindReasoningSummary:
			reasoning = true
		case wire.ChatPartKindText, wire.ChatPartKindModelRefusal:
			text = text || part.Text != ""
		case wire.ChatPartKindToolCall:
			called = true
		}
		kinds[part.Block] = part.Kind
		// Split token payloads at UTF-8 boundaries. Identity metadata occurs once.
		for len(part.Text) > 16384 || len(part.Arguments) > 16384 {
			chunk := part
			if len(part.Text) > 16384 {
				cut := 16384
				for !utf8.RuneStart(part.Text[cut]) {
					cut--
				}
				chunk.Text, part.Text = part.Text[:cut], part.Text[cut:]
				chunk.Arguments = ""
			} else {
				cut := 16384
				for !utf8.RuneStart(part.Arguments[cut]) {
					cut--
				}
				chunk.Arguments, part.Arguments = part.Arguments[:cut], part.Arguments[cut:]
				chunk.Text = ""
			}
			if err := op.emitChat3(wire.ChatEvent{Kind: wire.ChatEventKindPart, Part: &chunk}, p.cfg.Now()); err != nil {
				return err
			}
			part.CallID, part.Name = "", ""
		}
		return op.emitChat3(wire.ChatEvent{Kind: wire.ChatEventKindPart, Part: &part}, p.cfg.Now())
	}
	sink := chat3Sink{Now: p.cfg.Now, Part: part, Usage: func(usage wire.ChatUsage) error {
		return op.emitChat3(wire.ChatEvent{Kind: wire.ChatEventKindUsage, Usage: &usage}, p.cfg.Now())
	},
		Token:  func() { (&stream{op: op, now: p.cfg.Now}).token() },
		Engine: func(t *engineTimings) { (&stream{op: op}).engine(t) },
		Seal: func(block int64, origin, field string, value []byte) error {
			if seals[block] == nil {
				seals[block] = &chat3SealBuilder{}
			}
			seals[block].append(origin, field, value)
			return nil
		},
		BlockEnd: func(block int64) error {
			var omitted *wire.SealOmission
			if builder := seals[block]; builder != nil {
				seal, why := builder.finish()
				omitted = why
				if seal != nil {
					kind := kinds[block]
					if !kind.Known() {
						kind = wire.ChatPartKindReasoning
					}
					if err := part(wire.ChatPart{Block: block, Kind: kind, Seal: seal}); err != nil {
						return err
					}
				}
				delete(seals, block)
			}
			if omitted != nil {
				p.record(Record{ChatVersion: 3, Operation: op.id, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name, Placement: op.placement, Outcome: "seal_omitted", Reason: string(*omitted)})
			}
			return op.emitChat3(wire.ChatEvent{Kind: wire.ChatEventKindBlockEnd, BlockEnd: &wire.ChatBlockEnd{Block: block, SealOmitted: omitted}}, p.cfg.Now())
		}}
	// The model's own extensions and reasoning controls shape the body; an
	// extension it does not take is dropped and reported.
	caps := p.modelCapabilities(op.host, op.model)
	var dropped []string
	req.Extensions, dropped = chat3SupportedExtensions(op.host, caps, req.Extensions)
	plan := planChat3Reasoning(op.host, op.model, caps, op.chat3.request.Content.Options, req.Extensions)
	op.mu.Lock()
	op.chat3.reasoning, op.chat3.dropped = plan.applied, dropped
	op.mu.Unlock()
	attempt := withChat3Reasoning(ctx, plan)
	var result chat3UpstreamResult
	if op.host.Wire == router.WireAnthropicMessages {
		result = runChat3Anthropic(attempt, p.cfg.HTTP, op.host, op.model, req, op.chat3.request.Content.Options, op.chat3.request.Content.Tools, images, headers, sink)
	} else {
		result = runChat3OpenAI(attempt, p.cfg.HTTP, op.host, op.model, req, op.chat3.request.Content.Options, op.chat3.request.Content.Tools, images, headers, sink)
		if again, ok := withoutReasoning(op.host, req); ok && !chat3NamesReasoning(op.chat3.request) && result.Err == nil && result.Cause == "" && reasoning && !text && !called {
			result = p.retryChat3WithoutReasoning(ctx, op, again, images, headers, sink, kinds, result)
		}
	}
	op.chat3.result = result
	end := wire.Reply{Outcome: wire.ReplyOutcomeCompleted, StopReason: wire.StopReasonEnd, Cost: result.Cost}
	if result.Usage.Input != nil {
		end.Usage.Input = *result.Usage.Input
	}
	if result.Usage.Output != nil {
		end.Usage.Output = *result.Usage.Output
	}
	if result.Err == nil && result.Cause == "" && reasoning && !text && !called {
		only := failed(wire.ReplyOutcomeRefused, ReasonReasoningOnly)
		only.StopReason, only.Usage, only.Cost = end.StopReason, end.Usage, end.Cost
		return only
	}
	if result.Err != nil || result.Cause != "" {
		end.Outcome = wire.ReplyOutcomeUnavailable
		end.Reason = result.Cause
		if end.Reason == "" {
			end.Reason = "upstream:stream"
		}
		end.StopReason = wire.StopReasonNoStop
	}
	return end
}

// retryChat3WithoutReasoning asks the same server once more, without
// reasoning, for a reply whose only output was reasoning (INF-Q8). The
// first attempt's reasoning blocks stay delivered and ended; the second
// attempt's blocks follow them under new numbers, and its usage events and
// result count both attempts. The second attempt's result, failed or not,
// is the result: only a second failure ends the reply failed, under its
// own cause (architect ruling 2026-11-12 §3).
func (p *Provider) retryChat3WithoutReasoning(ctx context.Context, op *operation, req wire.Request, images map[string][]byte, headers map[string]string,
	sink chat3Sink, kinds map[int64]wire.ChatPartKind, first chat3UpstreamResult) chat3UpstreamResult {
	base := int64(0)
	for block := range kinds {
		base = max(base, block+1)
	}
	shifted := sink
	shifted.Part = func(part wire.ChatPart) error {
		part.Block += base
		return sink.Part(part)
	}
	shifted.BlockEnd = func(block int64) error { return sink.BlockEnd(block + base) }
	shifted.Seal = func(block int64, origin, field string, value []byte) error {
		return sink.Seal(block+base, origin, field, value)
	}
	shifted.Usage = func(usage wire.ChatUsage) error { return sink.Usage(addChatUsage(first.Usage, usage)) }
	// The second attempt asks for no reasoning through its extension alone;
	// the first attempt's planned fields stay off it.
	second := runChat3OpenAI(withChat3Reasoning(ctx, chat3Reasoning{}), p.cfg.HTTP, op.host, op.model, req, op.chat3.request.Content.Options, op.chat3.request.Content.Tools, images, headers, shifted)
	none := wire.ChatReasoningEffortNone
	op.mu.Lock()
	op.chat3.reasoning = &wire.ChatReasoningApplied{Effort: &none, Support: wire.ChatReasoningSupportApplied}
	op.mu.Unlock()
	second.Usage, second.Cost = addChatUsage(first.Usage, second.Usage), addCost(first.Cost, second.Cost)
	return second
}

func (p *Provider) ObserveV3(ctx context.Context, subject Subject, id string, cursor, maxEvents, maxBytes, waitMs int64) (page wire.ChatPage) {
	refuse := func(outcome wire.PageOutcome) wire.ChatPage {
		return wire.ChatPage{Outcome: outcome, Events: []wire.ChatEvent{}, Next: cursor}
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.PageOutcomeForbidden)
	}
	if cursor < 0 || maxEvents < 1 || maxEvents > 256 || maxBytes < 1 || maxBytes > 65536 || waitMs < 0 || waitMs > 30000 || id == "" || len(id) > 128 {
		return refuse(wire.PageOutcomeInvalid)
	}
	op := p.visible(subject, id)
	if op == nil || op.chat3 == nil {
		return refuse(wire.PageOutcomeUnknown)
	}
	op.observeBegin()
	defer func() { op.observeEnd(p.cfg.Now(), ctx.Err() == nil && !page.AtEnd) }()
	out := refuse(wire.PageOutcomePage)
	status, err := op.observe.Wait(ctx, time.Duration(waitMs)*time.Millisecond, watch.Probe{Read: func() (bool, error) {
		op.mu.Lock()
		defer op.mu.Unlock()
		out = refuse(wire.PageOutcomePage)
		if cursor < op.base {
			out.Outcome = wire.PageOutcomeGap
			out.Next = op.base
			return true, nil
		}
		if cursor > op.next {
			out.Outcome = wire.PageOutcomeInvalid
			return true, nil
		}
		used := int64(0)
		for i := cursor - op.base; i < int64(len(op.chat3.events)) && int64(len(out.Events)) < maxEvents; i++ {
			event := op.chat3.events[i]
			raw, err := wire.ToJSON(event)
			if err != nil {
				return false, err
			}
			if len(out.Events) > 0 && used+int64(len(raw)) > maxBytes {
				break
			}
			used += int64(len(raw))
			out.Events = append(out.Events, event)
			out.Next = event.Sequence + 1
		}
		out.AtEnd = op.ended && out.Next == op.next
		return cursor < op.next || op.ended, nil
	}, Recheck: func() error {
		if p.visible(subject, id) != op {
			return errors.New("request no longer visible")
		}
		return nil
	}})
	if ctx.Err() != nil {
		return refuse(wire.PageOutcomePage)
	}
	if err != nil || status == watch.Busy {
		return refuse(wire.PageOutcomeUnavailable)
	}
	if _, err := wire.ToJSON(out); err != nil {
		return refuse(wire.PageOutcomeUnavailable)
	}
	return out
}

func (p *Provider) CancelV3(subject Subject, id string) wire.ChatCancellation {
	if op := p.visible(subject, id); op != nil && op.chat3 == nil {
		return wire.ChatCancellation{Outcome: wire.CancelOutcomeUnknown}
	}
	old := p.cancelProfile(subject, id, router.ProfileChat)
	out := wire.ChatCancellation{Outcome: old.Outcome}
	if old.ReplyOutcome != nil {
		value := wire.ChatResultOutcomeFailed
		if *old.ReplyOutcome == wire.ReplyOutcomeCompleted {
			value = wire.ChatResultOutcomeCompleted
		} else if *old.ReplyOutcome == wire.ReplyOutcomeCancelled {
			value = wire.ChatResultOutcomeCancelled
		}
		out.ResultOutcome = &value
	}
	return out
}
