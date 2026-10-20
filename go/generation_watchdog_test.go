package inference

import (
	"testing"
	"time"

	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

func TestGenerationWatchdogActualTokensKeepLongWorkLive(t *testing.T) {
	now := time.Unix(0, 0)
	var g generationWatchdog
	g.arm(now, defaultGenerationStall)
	for range 100 {
		now = now.Add(179 * time.Second)
		if g.expired(now) {
			t.Fatal("progressing generation stalled")
		}
		g.progress(now, true, false)
	}
}

func TestGenerationWatchdogIgnoresKeepaliveUsageAndCallerPresence(t *testing.T) {
	now := time.Unix(0, 0)
	var g generationWatchdog
	g.arm(now, defaultGenerationStall)
	for range 180 {
		now = now.Add(time.Second)
		g.progress(now, false, false)
	}
	if !g.expired(now) {
		t.Fatal("non-token activity reset the stall timer")
	}
}

func TestGenerationWatchdogDeclaredProgressAndLongerAdapterBound(t *testing.T) {
	now := time.Unix(0, 0)
	var g generationWatchdog
	g.arm(now, 240*time.Second)
	if g.expired(now.Add(180 * time.Second)) {
		t.Fatal("longer adapter declaration ignored")
	}
	g.progress(now.Add(239*time.Second), false, true)
	if g.expired(now.Add(478 * time.Second)) {
		t.Fatal("declared provider progress ignored")
	}
	if !g.expired(now.Add(479 * time.Second)) {
		t.Fatal("declared progress granted an unbounded lifetime")
	}
}

func TestGenerationWatchdogEveryDeclaredWireHasOneProducerOrRelay(t *testing.T) {
	for _, kind := range routerwire.WireKinds {
		d, ok := wireAdapters[kind]
		if !ok || !d.relay && d.stall <= 0 || d.relay && d.stall != 0 {
			t.Fatalf("wire %q has no bounded adapter declaration", kind)
		}
	}
}

func TestGenerationWatchdogRechecksProgressBeforeStopping(t *testing.T) {
	now := time.Unix(0, 0)
	stopped := false
	op := &operation{cancel: func() { stopped = true }}
	op.generation.arm(now, defaultGenerationStall)
	now = now.Add(defaultGenerationStall)
	if !op.generationExpired(now) {
		t.Fatal("stalled candidate missing")
	}
	op.generation.progress(now, true, false)
	if op.stopStalled(now) || stopped {
		t.Fatal("fresh token lost a race with watchdog selection")
	}
	if !op.stopStalled(now.Add(defaultGenerationStall)) || !stopped || op.stopped() != "stalled" {
		t.Fatal("real stall did not cancel its upstream")
	}
}
