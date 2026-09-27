package inference

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	modelid "github.com/openabstractions/abstraction-model/identity"
	router "github.com/openabstractions/abstraction-router/go"
)

func familyOf(model string) string { return modelid.Family(model) }

// operation is one admitted request: its retained deltas, its observers and
// the cancellation of its upstream request.
type operation struct {
	live       *liveSession
	id         string
	subject    Subject
	host       *router.Host
	bindingID  string
	model      string
	credential string
	profile    string
	started    time.Time
	capacity   int
	route      route
	// remote is an operation delegated to a remote runtime, which counts its
	// own ceilings.
	remote bool

	mu          sync.Mutex
	deltas      []wire.Delta // the retained window; deltas[0] has sequence base
	base        int64
	next        int64
	changed     chan struct{}
	ended       bool
	finishing   bool
	endAt       time.Time
	outcome     wire.ReplyOutcome
	observing   int
	lastObserve time.Time
	stopReason  string
	cancel      context.CancelFunc
	finished    chan struct{}
}

func newOperation(id string, subject Subject, host *router.Host, model, credential string, capacity int, started time.Time) *operation {
	return &operation{id: id, subject: subject, host: host, model: model, credential: credential, started: started,
		profile: router.ProfileChat, capacity: capacity, changed: make(chan struct{}), lastObserve: started, finished: make(chan struct{})}
}

func (op *operation) isEnded() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.ended
}

func (op *operation) endedAt() (bool, time.Time) {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.ended, op.endAt
}

func (op *operation) endedOutcome() (wire.ReplyOutcome, bool) {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.outcome, op.ended
}

func (op *operation) idleSince(now time.Time) time.Duration {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.observing > 0 {
		return 0
	}
	return now.Sub(op.lastObserve)
}

func (op *operation) observeBegin() {
	op.mu.Lock()
	op.observing++
	op.mu.Unlock()
}

func (op *operation) observeEnd(now time.Time) {
	op.mu.Lock()
	op.observing--
	op.lastObserve = now
	op.mu.Unlock()
}

// stop cancels the upstream request with reason, once. It reports whether this
// call stopped an unended operation.
func (op *operation) stop(reason string) bool {
	op.mu.Lock()
	if op.ended || op.finishing || op.stopReason != "" {
		op.mu.Unlock()
		return false
	}
	op.stopReason = reason
	cancel := op.cancel
	op.mu.Unlock()
	cancel()
	return true
}

// stopIdle cancels only if the operation is still idle at the cancellation
// point. The watchdog selects candidates without holding the operation lock;
// an Observe may begin between that selection and this call.
func (op *operation) stopIdle(now time.Time, idle time.Duration) bool {
	op.mu.Lock()
	if op.ended || op.finishing || op.stopReason != "" || op.observing > 0 || now.Sub(op.lastObserve) < idle {
		op.mu.Unlock()
		return false
	}
	op.stopReason = "idle"
	cancel := op.cancel
	op.mu.Unlock()
	cancel()
	return true
}

func (op *operation) stopped() string {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.stopReason
}

// emit appends one delta, numbering it, and wakes observers. The end delta
// marks the operation ended.
func (op *operation) emit(d wire.Delta, now time.Time) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.ended {
		return
	}
	d.Sequence = op.next
	op.next++
	op.deltas = append(op.deltas, d)
	if over := len(op.deltas) - op.capacity; over > 0 {
		op.deltas = append([]wire.Delta(nil), op.deltas[over:]...)
		op.base += int64(over)
	}
	if d.Kind == wire.DeltaKindEnd {
		op.ended, op.endAt = true, now
		switch {
		case d.End != nil:
			op.outcome = d.End.Outcome
		case d.LiveEnd != nil:
			op.outcome = d.LiveEnd.Outcome
		case d.ImageEnd != nil:
			op.outcome = d.ImageEnd.Outcome
		case d.TranscriptionEnd != nil:
			op.outcome = d.TranscriptionEnd.Outcome
		case d.SpeechEnd != nil:
			op.outcome = d.SpeechEnd.Outcome
		default:
			op.outcome = wire.ReplyOutcomeUnavailable
		}
	}
	close(op.changed)
	op.changed = make(chan struct{})
}

// deltaBytes is a delta's compact JSON size, the measure max_bytes bounds.
func deltaBytes(d wire.Delta) int64 {
	raw, err := json.Marshal(d)
	if err != nil {
		return 1 << 16
	}
	return int64(len(raw))
}

// page reads from cursor, waiting once up to wait for a first delta.
func (op *operation) page(ctx context.Context, cursor, maxDeltas, maxBytes int64, wait time.Duration) wire.DeltaPage {
	waited := false
	for {
		op.mu.Lock()
		if cursor < op.base {
			next := op.base
			op.mu.Unlock()
			return wire.DeltaPage{Outcome: wire.PageOutcomeGap, Deltas: []wire.Delta{}, Next: next}
		}
		if cursor > op.next {
			op.mu.Unlock()
			return wire.DeltaPage{Outcome: wire.PageOutcomeInvalid, Deltas: []wire.Delta{}, Next: cursor}
		}
		if cursor < op.next || op.ended || waited || wait <= 0 {
			out := wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
			var used int64
			for i := cursor - op.base; i < int64(len(op.deltas)) && int64(len(out.Deltas)) < maxDeltas; i++ {
				d := op.deltas[i]
				size := deltaBytes(d)
				if len(out.Deltas) > 0 && used+size > maxBytes {
					break
				}
				used += size
				out.Deltas = append(out.Deltas, d)
				out.Next = d.Sequence + 1
				if d.Kind == wire.DeltaKindEnd {
					out.AtEnd = true
				}
			}
			if op.ended && out.Next == op.next {
				out.AtEnd = true
			}
			op.mu.Unlock()
			return out
		}
		changed := op.changed
		op.mu.Unlock()
		waited = true
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if ctx.Err() != nil {
			return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
		}
	}
}

// run performs the upstream request and ends the operation with its reply.
func (p *Provider) run(ctx context.Context, op *operation, adapter adapter, req wire.Request, images map[string][]byte, headers map[string]string) {
	defer p.runs.Done()
	defer close(op.finished)
	defer op.cancel()
	s := &stream{op: op, now: p.cfg.Now}
	end := adapter.run(ctx, p.cfg.HTTP, op.host, op.model, req, images, headers, s)
	clear(headers)
	if reason := op.stopped(); reason != "" {
		end.Outcome, end.Reason, end.StopReason = wire.ReplyOutcomeCancelled, reason, wire.StopReasonNoStop
	} else if p.ctx.Err() != nil {
		end.Outcome, end.Reason, end.StopReason = wire.ReplyOutcomeCancelled, "caller", wire.StopReasonNoStop
	}
	end.Host, end.Model = op.host.Name, op.model
	end.Message = wire.Message{Role: wire.RoleAssistant, Parts: []wire.Part{}}
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		var micros int64
		if end.Cost != nil {
			micros = end.Cost.Micros
		}
		if err := p.cfg.Ceilings.Add(op.credential, end.Usage.Input+end.Usage.Output, micros); err != nil {
			p.report(err)
		}
	}
	now := p.cfg.Now()
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, End: &end}, now)
	r := Record{Operation: op.id, Route: op.route.name, Rung: op.route.rung, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name,
		Model: op.model, Family: familyOf(op.model), Credential: op.credential, Outcome: end.Outcome.String(),
		Reason: end.Reason, TokensIn: end.Usage.Input, TokensOut: end.Usage.Output, WallMS: now.Sub(op.started).Milliseconds(),
		Domain: op.route.domain, Claim: op.route.claim}
	if op.remote {
		r.Domain = op.host.Domain
	}
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		r.Ceiling = p.cfg.Ceilings.State(op.credential)
	}
	p.record(r)
}

// stream turns adapter events into numbered deltas with stable part indexes.
type stream struct {
	op  *operation
	now func() time.Time
}

func (s *stream) part(index int64, part wire.Part) {
	s.op.emit(wire.Delta{Kind: wire.DeltaKindPart, Index: index, Part: &part}, s.now())
}

func (s *stream) usage(u wire.Usage) {
	s.op.emit(wire.Delta{Kind: wire.DeltaKindUsage, Usage: &u}, s.now())
}

// beginCompletion orders cancellation before publication of a generated result.
// Once publication starts, Cancel waits for its outcome; storage I/O runs without
// holding the operation mutex. The worker must always emit a terminal delta.
func (op *operation) beginCompletion() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.ended || op.stopReason != "" || op.finishing {
		return false
	}
	op.finishing = true
	return true
}
