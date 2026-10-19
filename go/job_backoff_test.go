package inference

import (
	"testing"
	"time"
)

// TestUncertainResendBacksOff: a resend after upstream:uncertain waits 1 s,
// doubling to a 60 s cap, and any answer resets it.
func TestUncertainResendBacksOff(t *testing.T) {
	now := time.Unix(1000, 0)
	e := &JobExecution{clock: func() time.Time { return now }}
	if !e.resendDue("job") {
		t.Fatal("a job with no uncertain attempt waited")
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for _, step := range want {
		e.noteUncertain("job")
		now = now.Add(step*time.Second - time.Millisecond)
		if e.resendDue("job") {
			t.Fatalf("resend due before %d s", step)
		}
		now = now.Add(time.Millisecond)
		if !e.resendDue("job") {
			t.Fatalf("resend not due after %d s", step)
		}
	}
	e.noteUncertain("job")
	e.noteAnswered("job")
	if !e.resendDue("job") {
		t.Fatal("an answer did not reset the backoff")
	}
	e.noteUncertain("job")
	now = now.Add(time.Second)
	if !e.resendDue("job") {
		t.Fatal("the backoff after a reset did not start again at 1 s")
	}
}
