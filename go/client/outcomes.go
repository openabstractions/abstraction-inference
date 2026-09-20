package client

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

// Admission and reply enums share refusal words but have independent numeric tags.
func replyFromAdmission(outcome wire.StartOutcome) wire.ReplyOutcome {
	reply, _ := wire.ParseReplyOutcome(outcome.String())
	return reply
}
