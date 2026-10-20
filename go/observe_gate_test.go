package inference

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	watch "github.com/openabstractions/abstraction-watch/go"
)

func TestDeltaObserveBoundsWaitersAndRechecksAfterWake(t *testing.T) {
	op := newOperation("wait", caller, nil, "model", "", 16, time.Now())
	op.observe = watch.NewGate(1)
	var withdrawn atomic.Bool
	done := make(chan wire.DeltaPage, 1)
	go func() {
		done <- op.pageChecked(t.Context(), 0, 1, 1024, 5*time.Second, func() error {
			if withdrawn.Load() {
				return errors.New("operation no longer visible")
			}
			return nil
		})
	}()
	deadline := time.Now().Add(time.Second)
	for op.observe.Waiting() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("waiter did not enter gate")
		}
		time.Sleep(time.Millisecond)
	}
	if page := op.page(t.Context(), 0, 1, 1024, time.Millisecond); page.Outcome != wire.PageOutcomeUnavailable {
		t.Fatalf("full gate: %+v", page)
	}
	withdrawn.Store(true)
	op.observe.Advance()
	select {
	case page := <-done:
		if page.Outcome != wire.PageOutcomeUnavailable || len(page.Deltas) != 0 {
			t.Fatalf("wake authority: %+v", page)
		}
	case <-time.After(time.Second):
		t.Fatal("advance did not wake observer")
	}
}
