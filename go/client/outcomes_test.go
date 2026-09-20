package client

import (
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"testing"
)

func TestAdmissionRefusalsPreserveWireMeaning(t *testing.T) {
	for _, admission := range wire.StartOutcomeValues() {
		if admission == wire.StartOutcomeAccepted {
			continue
		}
		reply := replyFromAdmission(admission)
		if !reply.Known() || reply.String() != admission.String() {
			t.Errorf("admission %s became reply %s", admission, reply)
		}
	}
	for _, invalid := range []wire.StartOutcome{0, wire.StartOutcomeAccepted, wire.StartOutcome(999)} {
		if replyFromAdmission(invalid).Known() {
			t.Errorf("invalid refusal %v became a valid reply", invalid)
		}
	}
}
