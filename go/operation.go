package inference

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	modelid "github.com/openabstractions/abstraction-model/identity"
	router "github.com/openabstractions/abstraction-router/go"
	watch "github.com/openabstractions/abstraction-watch/go"
)

func familyOf(model string) string { return modelid.Family(model) }

// operation is one admitted request: its retained deltas, its observers and
// the cancellation of its upstream request.
type operation struct {
	chat3      *chat3Run
	cache      chatCacheBinding
	live       *liveSession
	id         string
	subject    Subject
	host       *router.Host
	placement  wire.ServedPlace
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
	// selection is the named policy's choice its end record carries.
	selection selectionNote
	// remoteValues retains receiving authorization values across the ingress
	// call's lifetime. Standing supplies its own current cancellation/deadline.
	remoteValues context.Context
	// inputs are the content digests the request names as input, held until
	// the request ends (INF-V4). Set before the operation is admitted.
	inputs []string
	// job is the durable job operation that owns this operation, named by a
	// trusted runtime's JobExtension; it selects Provider.limits. Set before
	// the operation is admitted.
	job string

	mu          sync.Mutex
	deltas      []wire.Delta // the retained window; deltas[0] has sequence base
	base        int64
	next        int64
	observe     *watch.Gate
	ended       bool
	finishing   bool
	endAt       time.Time
	outcome     wire.ReplyOutcome
	observing   int
	lastObserve time.Time
	// resumable is true when the last Observe returned its page before the
	// end to a caller still connected: that caller holds the handle and may
	// resume after its own pause (Provider.limits).
	resumable  bool
	stopReason string
	generation generationWatchdog
	timing     requestClock // operation.mu owns all timing facts
	cancel     context.CancelFunc
	finished   chan struct{}
}

type operationStandingContext struct {
	context.Context
	values context.Context
}

func (c operationStandingContext) Value(key any) any { return c.values.Value(key) }

func (op *operation) standingContext(ctx context.Context) context.Context {
	if op.remoteValues == nil {
		return ctx
	}
	return operationStandingContext{Context: ctx, values: op.remoteValues}
}

func newOperation(id string, subject Subject, host *router.Host, model, credential string, capacity int, started time.Time) *operation {
	return &operation{id: id, subject: subject, host: host, placement: servedPlace(host), model: model, credential: credential, started: started,
		profile: router.ProfileChat, capacity: capacity, observe: watch.NewGate(watch.DefaultWaiters), lastObserve: started, finished: make(chan struct{})}
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

// observeEnd ends one Observe at now. resumable says it returned its page
// before the end to a caller still connected; an Observe its caller left
// mid-wait, or one that read the end, is not.
func (op *operation) observeEnd(now time.Time, resumable bool) {
	op.mu.Lock()
	op.observing--
	op.lastObserve = now
	op.resumable = resumable
	op.mu.Unlock()
}

func (op *operation) isResumable() bool {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.resumable
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
	if op.chat3 != nil {
		if d.End != nil {
			op.endChat3Locked(*d.End, now)
		}
		return
	}
	if d.Part != nil && (d.Part.Text != "" || d.Part.Arguments != "" || d.Part.Name != "") {
		op.timing.token(now)
		op.generation.progress(now, true, false)
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
	op.observe.Advance()
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
	return op.pageChecked(ctx, cursor, maxDeltas, maxBytes, wait, func() error { return nil })
}

func (op *operation) pageChecked(ctx context.Context, cursor, maxDeltas, maxBytes int64, wait time.Duration, recheck func() error) wire.DeltaPage {
	out := wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
	outcome, err := op.observe.Wait(ctx, wait, watch.Probe{
		Read: func() (bool, error) {
			op.mu.Lock()
			defer op.mu.Unlock()
			if cursor < op.base {
				out = wire.DeltaPage{Outcome: wire.PageOutcomeGap, Deltas: []wire.Delta{}, Next: op.base}
				return true, nil
			}
			if cursor > op.next {
				out = wire.DeltaPage{Outcome: wire.PageOutcomeInvalid, Deltas: []wire.Delta{}, Next: cursor}
				return true, nil
			}
			out = wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
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
			return cursor < op.next || op.ended, nil
		}, Recheck: recheck,
	})
	if ctx.Err() != nil {
		return wire.DeltaPage{Outcome: wire.PageOutcomePage, Deltas: []wire.Delta{}, Next: cursor}
	}
	if err != nil || outcome == watch.Busy {
		return wire.DeltaPage{Outcome: wire.PageOutcomeUnavailable, Deltas: []wire.Delta{}, Next: cursor}
	}
	return out
}

// run performs the upstream request and ends the operation with its reply.
func (p *Provider) run(ctx context.Context, op *operation, adapter adapter, req wire.Request, images map[string][]byte, headers map[string]string) {
	defer p.runs.Done()
	defer close(op.finished)
	defer op.cancel()
	op.mu.Lock()
	op.timing.begin(op.started, p.cfg.Now())
	op.mu.Unlock()
	s := &stream{op: op, now: p.cfg.Now}
	ctx = context.WithValue(ctx, executionCacheScopeKey{}, op.cache)
	ctx = context.WithValue(ctx, executionObservationKey{}, func(value ExecutionObservation) *int64 {
		op.mu.Lock()
		defer op.mu.Unlock()
		return op.timing.observeExecution(value)
	})
	if op.chat3 != nil {
		if use := op.chat3.request.Use; use != nil {
			ctx = context.WithValue(ctx, executionUseKey{}, *use)
		}
		ctx = context.WithValue(ctx, freeCapacityKey{}, chat3HasGuarantee(op.chat3.request, wire.ChatGuaranteeFreeCapacity))
	}
	ctx = context.WithValue(ctx, generationSendKey{}, func() {
		if wireAdapters[op.host.Wire].relay {
			return
		}
		op.mu.Lock()
		defer op.mu.Unlock()
		op.timing.send(p.cfg.Now())
		if op.generation.deadline.IsZero() {
			op.generation.arm(p.cfg.Now(), wireAdapters[op.host.Wire].stall)
		}
	})
	if !wireAdapters[op.host.Wire].relay {
		ctx = context.WithValue(ctx, executionPrefillKey{}, func(processed, total int64) bool {
			op.mu.Lock()
			defer op.mu.Unlock()
			if op.ended || op.finishing || op.stopReason != "" {
				return false
			}
			return op.generation.prefill(p.cfg.Now(), processed, total)
		})
		ctx = context.WithValue(ctx, executionProgressKey{}, func(phase string) {
			if phase == "" {
				return
			}
			now := p.cfg.Now()
			if op.chat3 != nil {
				progress := wire.ChatProgress{Phase: wire.ChatProgressPhase(phase)}
				if err := op.emitChat3(wire.ChatEvent{Kind: wire.ChatEventKindProgress, Progress: &progress}, now); err != nil {
					p.report(err)
					return
				}
			}
			op.mu.Lock()
			op.generation.progress(now, false, true)
			op.mu.Unlock()
		})
	}
	var end wire.Reply
	reason, ready := "", true
	if p.cfg.WaitExecution != nil {
		prepared := p.cfg.WaitExecution(WithAdmissionProfile(ctx, "chat"), op.host, op.model)
		reason, ready = prepared.Reason, prepared.Ready
		op.mu.Lock()
		op.timing.prepare(prepared.Timing)
		op.mu.Unlock()
	}
	if ready {
		if op.chat3 != nil {
			end = p.runChat3(ctx, op, req, images, headers)
		} else {
			end = adapter.run(ctx, p.cfg.HTTP, op.host, op.model, req, images, headers, s)
		}
	} else {
		end = wire.Reply{Outcome: wire.ReplyOutcomeUnavailable, Reason: reason, StopReason: wire.StopReasonNoStop}
	}
	// Decode ends at the adapter boundary. Credential accounting and terminal
	// publication below do not become model generation time.
	op.mu.Lock()
	op.timing.finish(p.cfg.Now())
	op.mu.Unlock()
	clear(headers)
	uncertain := remoteExecutionUncertain(op, end.Outcome, end.Reason)
	if op.chat3 != nil && end.Outcome == wire.ReplyOutcomeUnavailable && end.Reason == "remote:uncertain" {
		uncertain = true
	}
	if reason := op.stopped(); reason != "" && !uncertain {
		end.Outcome, end.Reason, end.StopReason = wire.ReplyOutcomeCancelled, reason, wire.StopReasonNoStop
		if reason == "stalled" {
			end.Outcome = wire.ReplyOutcomeUnavailable
		}
	} else if p.ctx.Err() != nil && !uncertain {
		end.Outcome, end.Reason, end.StopReason = wire.ReplyOutcomeCancelled, "caller", wire.StopReasonNoStop
	} else if end.Outcome == wire.ReplyOutcomeRefused {
		end.Reason = p.withObservedWindow(end.Reason, op.host.Name, op.model)
	}
	end.Host, end.Model = op.host.Name, op.model
	end.Message = wire.Message{Role: wire.RoleAssistant, Parts: []wire.Part{}}
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		var micros int64
		if end.Cost != nil {
			micros = end.Cost.Micros
		}
		err := p.cfg.Ceilings.addModalitiesForSubject(op.credential, op.subject, addObservedCount(end.Usage.Input, end.Usage.Output), micros, 0, 0, 0, end.Cost != nil)
		if err != nil {
			p.report(err)
		}
	}
	now := p.cfg.Now()
	op.mu.Lock()
	if op.chat3 != nil {
		usage := op.chat3.result.Usage
		if op.timing.facts.ReusedTokens == nil {
			op.timing.facts.ReusedTokens = count(usage.CacheRead)
		}
		if op.timing.facts.PrefillTokens == nil && usage.Input != nil && usage.CacheRead != nil && *usage.Input >= *usage.CacheRead && *usage.CacheRead >= 0 {
			value := *usage.Input - *usage.CacheRead
			op.timing.facts.PrefillTokens = &value
		}
	}
	timing := op.timing.facts
	op.mu.Unlock()
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, End: &end}, now)
	if op.chat3 != nil {
		p.chat3History.finish(op.chat3.history, end.Outcome == wire.ReplyOutcomeUnavailable && !uncertain, now)
	}
	r := Record{Operation: op.id, Route: op.route.name, Rung: op.route.rung, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name,
		Model: op.model, Placement: op.placement, Family: familyOf(op.model), Credential: op.credential, Outcome: end.Outcome.String(),
		Reason: end.Reason, TokensIn: end.Usage.Input, TokensOut: end.Usage.Output, WallMS: now.Sub(op.started).Milliseconds(),
		Domain: op.route.domain, Claim: op.route.claim, Timing: &timing}
	if op.chat3 != nil {
		r.ChatVersion = 3
		op.mu.Lock()
		if terminal := op.chat3.terminal; terminal != nil {
			r.DelegatedTiming = op.delegatedTimingLocked()
			r.Outcome, r.Reason = terminal.Outcome.String(), terminal.Cause
			if terminal.FinishReason != nil {
				r.FinishReason = string(*terminal.FinishReason)
			}
			{
				u := terminal.Usage
				for _, field := range []**int64{&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.Reasoning} {
					if *field != nil {
						value := **field
						*field = &value
					}
				}
				r.ChatUsage = &u
			}
		}
		op.mu.Unlock()
	}
	op.selection.apply(&r)
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

func (s *stream) token() {
	if s == nil || s.op == nil {
		return
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	s.op.mu.Lock()
	s.op.timing.token(now)
	s.op.generation.progress(now, true, false)
	s.op.mu.Unlock()
}

func (s *stream) engine(t *engineTimings) {
	if s == nil || s.op == nil {
		return
	}
	s.op.mu.Lock()
	s.op.timing.engine(t)
	s.op.mu.Unlock()
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
