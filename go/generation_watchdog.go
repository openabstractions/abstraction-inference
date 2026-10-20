package inference

import (
	"context"
	"time"
)

// defaultGenerationStall reuses ModelHost DefaultReady's existing stall budget
// (Architect Codex 268). It is neither a call deadline nor an HTTP timeout.
const defaultGenerationStall = 180 * time.Second

// generationWatchdog is one operation-owned logical timer. The provider's
// existing watchdog ticks it on the injected Now clock; operation.mu owns it.
type generationWatchdog struct {
	stall                          time.Duration
	deadline                       time.Time
	prefillProcessed, prefillTotal int64
	output                         bool
}

func (g *generationWatchdog) arm(now time.Time, stall time.Duration) {
	if stall <= 0 {
		stall = defaultGenerationStall
	}
	g.stall, g.deadline = stall, now.Add(stall)
	g.prefillProcessed, g.prefillTotal, g.output = 0, 0, false
}

func (g *generationWatchdog) progress(now time.Time, token, declared bool) {
	if !g.deadline.IsZero() && (token || declared) {
		if token {
			g.output = true
		}
		g.deadline = now.Add(g.stall)
	}
}

func (g *generationWatchdog) prefill(now time.Time, processed, total int64) bool {
	if g.deadline.IsZero() || g.output || processed <= g.prefillProcessed || processed <= 0 || total <= 0 || processed > total || g.prefillTotal != 0 && total != g.prefillTotal {
		return false
	}
	g.prefillProcessed, g.prefillTotal = processed, total
	g.deadline = now.Add(g.stall)
	return true
}

func (g *generationWatchdog) expired(now time.Time) bool {
	return !g.deadline.IsZero() && !now.Before(g.deadline)
}

type generationSendKey struct{}
type executionProgressKey struct{}
type executionPrefillKey struct{}

// ReportExecutionPrefill renews an active local operation only for increasing
// uncached-token counters validated by its engine producer. It emits no public progress phase.
// Missing, relayed, finalized and already-generating operations return false.
func ReportExecutionPrefill(ctx context.Context, processed, total int64) bool {
	if report, ok := ctx.Value(executionPrefillKey{}).(func(int64, int64) bool); ok {
		return report(processed, total)
	}
	return false
}

// ReportExecutionProgress is called only by an execution producer when its
// source-owned progress parser confirms a monotonic phase advance. Display
// labels received through a relay never call this callback.
func ReportExecutionProgress(ctx context.Context, phase string) {
	if report, ok := ctx.Value(executionProgressKey{}).(func(string)); ok {
		report(phase)
	}
}

// generationSent is called at the actual outgoing request boundary. Other
// modality requests carry no hook and keep their existing behaviour.
func generationSent(ctx context.Context) {
	if sent, ok := ctx.Value(generationSendKey{}).(func()); ok {
		sent()
	}
}

func (op *operation) generationExpired(now time.Time) bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return !op.ended && !op.finishing && op.stopReason == "" && op.generation.expired(now)
}

func (op *operation) stopStalled(now time.Time) bool {
	op.mu.Lock()
	if op.ended || op.finishing || op.stopReason != "" || !op.generation.expired(now) {
		op.mu.Unlock()
		return false
	}
	op.stopReason = "stalled"
	cancel := op.cancel
	op.mu.Unlock()
	cancel()
	return true
}
