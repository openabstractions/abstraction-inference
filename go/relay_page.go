package inference

import (
	"context"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"math"
	"time"
)

// validateRelayChatPage checks continuity before any provider data reaches the
// outer operation. A lost retained prefix remains an explicit refusal.
func validateRelayChatPage(page wire.DeltaPage, cursor int64) bool {
	if page.Outcome != wire.PageOutcomePage || cursor < 0 || len(page.Deltas) > 256 || cursor > math.MaxInt64-int64(len(page.Deltas)) || page.Next != cursor+int64(len(page.Deltas)) {
		return false
	}
	var used int64
	ended := false
	for i, d := range page.Deltas {
		if d.Sequence != cursor+int64(i) || ended {
			return false
		}
		used += deltaBytes(d)
		if i > 0 && used > 65536 {
			return false
		}
		switch d.Kind {
		case wire.DeltaKindPart:
			if d.Part == nil || d.Index < 0 {
				return false
			}
		case wire.DeltaKindUsage:
			if d.Usage == nil {
				return false
			}
		case wire.DeltaKindEnd:
			if d.End == nil {
				return false
			}
			ended = true
		default:
			return false
		}
	}
	// A sequential relay always consumes the terminal event itself. at_end
	// without that event means continuity was lost and cannot be reconstructed.
	return page.AtEnd == ended
}

// idleRelay prevents a provider returning immediate empty pages from spinning.
// The caller's operation deadline and cancellation still bound the wait.
func idleRelay(ctx context.Context) bool {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
