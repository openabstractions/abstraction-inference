package inference

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

// replyFromStart preserves the shared refusal word between admission and
// terminal replies. It is called only for a refused admission.
func replyFromStart(outcome wire.StartOutcome) wire.ReplyOutcome {
	reply, ok := wire.ParseReplyOutcome(outcome.String())
	if !ok {
		return 0
	}
	return reply
}
