package inference

import (
	"context"
	"encoding/json"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"math"
	"strings"
	"time"
)

// RequestTiming contains observations for one operation. An absent field means
// its owner supplied no fact. Durations use OA's monotonic clock; Engine keeps
// independently reported engine claims and never replaces those durations.
// It contains no request or response content.
type RequestTiming struct {
	QueueMS, LoadMS, RestoreMS, PrefillMS, DecodeMS *int64
	RelayMS                                         *int64
	Load, LoadSource, Restore                       string
	RestoreCause                                    string
	CacheEvictions                                  *int64
	LoadBytes, PrefillTokens, ReusedTokens          *int64
	Engine                                          EngineTiming
}

// ExecutionObservation is bounded provider-owned resident-cache evidence.
// It carries no scope, program, request content or engine timing claim.
type ExecutionObservation struct {
	Restore, Cause string
	Evicted        bool
}

type executionObservationKey struct{}

// ReportExecutionObservation reports through the current operation owner and
// returns a copy of its OA-clock prefill duration when observed. Missing or
// finalized operations return nil and accept no late facts.
func ReportExecutionObservation(ctx context.Context, observation ExecutionObservation) *int64 {
	if report, ok := ctx.Value(executionObservationKey{}).(func(ExecutionObservation) *int64); ok {
		return report(observation)
	}
	return nil
}

func (c *requestClock) observeExecution(value ExecutionObservation) *int64 {
	if c.finished {
		return nil
	}
	switch value.Restore {
	case "hit", "miss", "refused":
	default:
		return nil
	}
	switch value.Cause {
	case "resident", "not_resident", "unlabelled", "program_change", "missing_partition", "request_failed":
	default:
		return nil
	}
	c.facts.Restore, c.facts.RestoreCause = value.Restore, value.Cause
	if c.facts.CacheEvictions == nil || value.Evicted {
		n := int64(0)
		if value.Evicted {
			n = 1
		}
		c.facts.CacheEvictions = &n
	}
	return count(c.facts.PrefillMS)
}

type EngineTiming struct {
	PromptMS, DecodeMS *float64
}

// ExecutionPreparation is the private execution hook's result, introduced after
// the published Go baseline. Admission authority remains in Admit.
type ExecutionPreparation struct {
	Reason string
	Ready  bool
	Timing PreparationTiming
}

// PreparationTiming carries only facts owned by model loading/restoration.
// A producer with no cache-restore observation leaves both restore fields absent.
type PreparationTiming struct {
	LoadMS, RestoreMS, LoadBytes *int64
	Load, LoadSource, Restore    string
}

type requestClock struct {
	facts       RequestTiming
	sent, first time.Time
	relaySent   time.Time
	finished    bool
}

// DelegatedTiming is the serving runtime's report, attributed by its unchanged
// served_by value. A relaying runtime keeps these claims apart from its clock.
type DelegatedTiming struct {
	Timing   wire.ChatTiming
	ServedBy *wire.ServedBy
}

func (op *operation) delegatedTimingLocked() *DelegatedTiming {
	if op.chat3 == nil || !op.chat3.relayed || op.chat3.terminal == nil || op.chat3.terminal.Timing == nil {
		return nil
	}
	t := op.chat3.terminal
	d := &DelegatedTiming{Timing: copyChatTiming(*t.Timing)}
	if t.ServedBy != nil {
		v := *t.ServedBy
		d.ServedBy = &v
	}
	return d
}

func copyChatTiming(t wire.ChatTiming) wire.ChatTiming {
	t.QueueMs, t.LoadMs, t.RestoreMs = count(t.QueueMs), count(t.LoadMs), count(t.RestoreMs)
	t.PrefillMs, t.DecodeMs = count(t.PrefillMs), count(t.DecodeMs)
	t.PrefillTokens, t.ReusedTokens = count(t.PrefillTokens), count(t.ReusedTokens)
	if t.Load != nil {
		v := *t.Load
		t.Load = &v
	}
	if t.LoadSource != nil {
		v := *t.LoadSource
		t.LoadSource = &v
	}
	return t
}

func (c *requestClock) chatTiming() *wire.ChatTiming {
	f := c.facts
	t := wire.ChatTiming{QueueMs: f.QueueMS, LoadMs: f.LoadMS, RestoreMs: f.RestoreMS, PrefillMs: f.PrefillMS, DecodeMs: f.DecodeMS,
		PrefillTokens: f.PrefillTokens, ReusedTokens: f.ReusedTokens}
	if f.Load != "" {
		v := wire.ChatLoad(f.Load)
		t.Load = &v
	}
	if f.LoadSource != "" {
		v := wire.ChatLoadSource(f.LoadSource)
		t.LoadSource = &v
	}
	if t.QueueMs == nil && t.LoadMs == nil && t.RestoreMs == nil && t.PrefillMs == nil && t.DecodeMs == nil && t.PrefillTokens == nil && t.ReusedTokens == nil && t.Load == nil && t.LoadSource == nil {
		return nil
	}
	t = copyChatTiming(t)
	return &t
}

func (c *requestClock) relaySend(now time.Time) {
	if c.relaySent.IsZero() {
		c.relaySent = now
	}
}
func (c *requestClock) relayDelta(now time.Time) {
	if c.facts.RelayMS == nil {
		c.facts.RelayMS = observedMS(c.relaySent, now)
	}
}

func observedMS(start, end time.Time) *int64 {
	if start.IsZero() || end.Before(start) {
		return nil
	}
	v := end.Sub(start).Milliseconds()
	return &v
}

func count(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	v := *value
	return &v
}

func claim(value *float64) *float64 {
	if value == nil || *value < 0 || math.IsInf(*value, 0) || math.IsNaN(*value) {
		return nil
	}
	v := *value
	return &v
}

func (c *requestClock) begin(accepted, executing time.Time) {
	c.facts.QueueMS = observedMS(accepted, executing)
}
func (c *requestClock) prepare(p PreparationTiming) {
	c.facts.LoadMS, c.facts.RestoreMS, c.facts.LoadBytes = count(p.LoadMS), count(p.RestoreMS), count(p.LoadBytes)
	// These are trusted owner observations, rather than classifications inferred
	// from elapsed time. Unknown observations remain absent.
	switch p.Load {
	case "cold", "warm", "loaded":
		c.facts.Load = p.Load
	}
	switch p.LoadSource {
	case "disk", "adopted", "lent":
		c.facts.LoadSource = p.LoadSource
	}
	switch p.Restore {
	case "hit", "miss", "refused":
		c.facts.Restore = p.Restore
	}
}
func (c *requestClock) send(now time.Time) {
	if c.sent.IsZero() {
		c.sent = now
	}
}
func (c *requestClock) token(now time.Time) {
	if c.first.IsZero() && !c.sent.IsZero() && !now.Before(c.sent) {
		c.first = now
		c.facts.PrefillMS = observedMS(c.sent, now)
	}
}
func (c *requestClock) finish(now time.Time) {
	if c.finished {
		return
	}
	c.finished = true
	c.facts.DecodeMS = observedMS(c.first, now)
}
func (c *requestClock) engine(t *engineTimings) {
	if t == nil {
		return
	}
	if v := claim(t.PromptMS); v != nil {
		c.facts.Engine.PromptMS = v
	}
	if v := claim(t.DecodeMS); v != nil {
		c.facts.Engine.DecodeMS = v
	}
	if v := count(t.PromptN); v != nil {
		c.facts.PrefillTokens = v
	}
	if v := count(t.CacheN); v != nil {
		c.facts.ReusedTokens = v
	}
}

type engineTimings struct {
	PromptMS *float64 `json:"prompt_ms"`
	DecodeMS *float64 `json:"predicted_ms"`
	PromptN  *int64   `json:"prompt_n"`
	CacheN   *int64   `json:"cache_n"`
}

// Optional vendor diagnostics are interpreted independently after the primary
// completion has passed its normal JSON validation. A bad field supplies no
// claim; it cannot fail completion or erase another valid diagnostic field.
func parseEngineTimings(raw json.RawMessage) *engineTimings {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	duration := func(name string) *float64 {
		var value *float64
		if json.Unmarshal(fields[name], &value) != nil {
			return nil
		}
		value = claim(value)
		if value != nil && *value == 0 {
			// A nonzero mantissa that underflowed float64 is unrepresentable,
			// rather than a reported zero duration.
			mantissa := strings.TrimSpace(string(fields[name]))
			if end := strings.IndexAny(mantissa, "eE"); end >= 0 {
				mantissa = mantissa[:end]
			}
			if strings.ContainsAny(mantissa, "123456789") {
				return nil
			}
		}
		return value
	}
	tokens := func(name string) *int64 {
		var value *int64
		if json.Unmarshal(fields[name], &value) != nil {
			return nil
		}
		return count(value)
	}
	return &engineTimings{PromptMS: duration("prompt_ms"), DecodeMS: duration("predicted_ms"),
		PromptN: tokens("prompt_n"), CacheN: tokens("cache_n")}
}
