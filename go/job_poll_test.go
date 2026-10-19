package inference

import (
	"testing"
	"time"
)

// A running upstream that reports steps under way is read every second, so
// each step reaches the job's progress; queued or silent work keeps the
// 5 s poll. The live walkthrough of 2026-10-02 (D10) saw a video job's 5 s
// poll skip a step of about 5.8 s. A shorter configured poll stays shorter.
func TestInferenceJobPollsFasterWhileStepsAreUnderWay(t *testing.T) {
	e := &JobExecution{pollInterval: defaultInferenceJobPollInterval}
	for _, c := range []struct {
		name     string
		progress durableJobProgress
		want     time.Duration
	}{
		{"generating", durableJobProgress{Done: 16, Total: 100}, time.Second},
		{"generating, no step yet", durableJobProgress{Done: 0, Total: 100}, time.Second},
		{"queued", durableJobProgress{Queued: true, Position: 1}, 5 * time.Second},
		{"no report", durableJobProgress{}, 5 * time.Second},
	} {
		if got := e.pollWait(c.progress); got != c.want {
			t.Fatalf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	short := &JobExecution{pollInterval: 5 * time.Millisecond}
	if got := short.pollWait(durableJobProgress{Done: 1, Total: 3}); got != 5*time.Millisecond {
		t.Fatalf("a shorter configured poll became %v", got)
	}
	if got := (&JobExecution{}).pollWait(durableJobProgress{}); got != defaultInferenceJobPollInterval {
		t.Fatalf("an unset poll waits %v", got)
	}
}
