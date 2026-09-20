package inference

import (
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"testing"
)

func TestReplyFromStartRejectsUnknown(t *testing.T) {
	if got := replyFromStart(wire.StartOutcome(99)); got != 0 {
		t.Fatalf("unknown start outcome mapped to %v", got)
	}
}
