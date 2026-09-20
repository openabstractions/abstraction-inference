package inference

import wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"

// admitOperation reserves capacity and registers work under the same lock
// Close uses to stop admission. Each successful reservation requires one
// runs.Done from its worker, including when shutdown has cancelled its context.
func (p *Provider) admitOperation(op *operation) wire.StartOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return wire.StartOutcomeUnavailable
	}
	active := 0
	for _, existing := range p.ops {
		if existing.subject == op.subject && !existing.isEnded() {
			active++
		}
	}
	if active >= p.cfg.MaxOperations {
		return wire.StartOutcomeExhausted
	}
	p.ops[op.id] = op
	p.runs.Add(1)
	return wire.StartOutcomeAccepted
}
