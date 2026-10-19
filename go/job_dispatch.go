package inference

import (
	"context"
	"fmt"
	"sync"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

const (
	// defaultLocalJobsPerHost is one: a local engine generates on one GPU,
	// and a second job there only slows the first.
	defaultLocalJobsPerHost = 1
	// defaultHostedJobsPerHost bounds the jobs this runtime keeps open at
	// one hosted service; the service queues and bills each one.
	defaultHostedJobsPerHost = 4
	// hostBusyWaiting is the waiting word of a claimed job queued behind
	// other jobs on its model server.
	hostBusyWaiting = "host:busy"
)

// SetHostConcurrency sets how many inference jobs this runtime executes at
// once on one local model server and on one hosted service. A value below
// one keeps the current limit.
func (e *JobExecution) SetHostConcurrency(local, hosted int) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if local > 0 {
		e.localJobs = local
	}
	if hosted > 0 {
		e.hostedJobs = hosted
	}
}

func (e *JobExecution) hostLimit(host *router.Host) int {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if host.Hosted {
		if e.hostedJobs > 0 {
			return e.hostedJobs
		}
		return defaultHostedJobsPerHost
	}
	if e.localJobs > 0 {
		return e.localJobs
	}
	return defaultLocalJobsPerHost
}

// hostQueue is one model server's executing jobs and the claimed jobs
// waiting for it, first claimed first.
type hostQueue struct {
	busy    int
	limit   int
	waiting []*hostTicket
}

// hostTicket is one job's place at a model server: ready closes when the job
// holds a slot there.
type hostTicket struct {
	e       *JobExecution
	key     string
	ready   chan struct{}
	granted bool
	left    bool
}

// enterHost queues for a slot at key. The ticket is ready at once when fewer
// than limit jobs execute there and nobody waits ahead of it.
func (e *JobExecution) enterHost(key string, limit int) *hostTicket {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	queue := e.hostQueue(key)
	queue.limit = limit
	ticket := &hostTicket{e: e, key: key, ready: make(chan struct{})}
	queue.waiting = append(queue.waiting, ticket)
	queue.grant()
	return ticket
}

// occupyHost counts a job against key without waiting: recovered work may
// already run upstream, and queueing it would only delay its cancellation
// and its result.
func (e *JobExecution) occupyHost(key string) *hostTicket {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	queue := e.hostQueue(key)
	queue.busy++
	ticket := &hostTicket{e: e, key: key, ready: make(chan struct{}), granted: true}
	close(ticket.ready)
	return ticket
}

func (e *JobExecution) hostQueue(key string) *hostQueue {
	if e.hosts == nil {
		e.hosts = map[string]*hostQueue{}
	}
	queue := e.hosts[key]
	if queue == nil {
		queue = &hostQueue{}
		e.hosts[key] = queue
	}
	return queue
}

// grant hands free slots to the longest-waiting tickets.
func (q *hostQueue) grant() {
	for len(q.waiting) > 0 && q.busy < max(q.limit, 1) {
		next := q.waiting[0]
		q.waiting = q.waiting[1:]
		q.busy++
		next.granted = true
		close(next.ready)
	}
}

// leave gives the ticket's slot to the next waiting job, or withdraws the
// ticket from the queue. Leaving twice is harmless.
func (t *hostTicket) leave() {
	t.e.slotsMu.Lock()
	defer t.e.slotsMu.Unlock()
	if t.left {
		return
	}
	t.left = true
	queue := t.e.hosts[t.key]
	if t.granted {
		queue.busy--
	} else {
		for i, waiting := range queue.waiting {
			if waiting == t {
				queue.waiting = append(queue.waiting[:i], queue.waiting[i+1:]...)
				break
			}
		}
	}
	queue.grant()
	if queue.busy == 0 && len(queue.waiting) == 0 {
		delete(t.e.hosts, t.key)
	}
}

func noLeave() {}

// planOnHost plans held work and holds a slot at the planned model server
// for the rest of the execution; the caller calls the returned leave when the
// execution ends. A fresh submission waits for its slot under the lease and
// is planned again once it holds one: rights, credentials and the chosen
// server are decided when the work is sent. cancelled reports that the
// caller asked to cancel the work while it waited unsent.
func (e *JobExecution) planOnHost(ctx context.Context, store job.Store, held *job.Record, work inferenceJobWork, checkpoint inferenceJobCheckpoint) (plan inferenceJobPlan, outcome acceptance.AcceptanceOutcome, reason string, leave func(), cancelled bool) {
	shown := false
	plan, outcome, reason = e.plan(WithPeer(ctx, e.submitter(held.ID)), work, checkpoint, held.ID)
	for {
		if outcome != acceptance.AcceptanceOutcomeAccepted {
			return plan, outcome, reason, noLeave, false
		}
		key := plan.host.Name
		if checkpoint.Phase != "" {
			return plan, outcome, reason, e.occupyHost(key).leave, false
		}
		ticket := e.enterHost(key, e.hostLimit(plan.host))
		select {
		case <-ticket.ready:
			return plan, outcome, reason, ticket.leave, false
		default:
		}
		clear(plan.headers)
		if !shown {
			shown = true
			waiting := checkpoint
			waiting.Waiting = hostBusyWaiting
			//unchecked: the waiting word is only shown; a failed write leaves the job queued, and lease loss ends the wait through ctx
			_, _ = store.Update(held.ID, held.Lease.Epoch, func(record *job.Record) error { return record.SetCheckpoint(waiting) })
		}
		if stop := e.awaitHost(ctx, store, held.ID, ticket); stop != "" {
			ticket.leave()
			if stop == "cancel" {
				return inferenceJobPlan{}, acceptance.AcceptanceOutcomeAccepted, "", noLeave, true
			}
			return inferenceJobPlan{}, acceptance.AcceptanceOutcomeUnavailable, "service:stopped", noLeave, false
		}
		plan, outcome, reason = e.plan(WithPeer(ctx, e.submitter(held.ID)), work, checkpoint, held.ID)
		if outcome != acceptance.AcceptanceOutcomeAccepted {
			ticket.leave()
			return plan, outcome, reason, noLeave, false
		}
		if plan.host.Name == key {
			return plan, outcome, reason, ticket.leave, false
		}
		// The work now goes to another server: queue there with this plan.
		ticket.leave()
	}
}

// awaitHost waits until ticket holds a slot. It returns "stop" when ctx ends
// (shutdown or a lost lease) and "cancel" when the job's caller asked to
// cancel it, which it checks every poll interval.
func (e *JobExecution) awaitHost(ctx context.Context, store job.Store, id string, ticket *hostTicket) string {
	every := e.pollInterval
	if every <= 0 {
		every = defaultInferenceJobPollInterval
	}
	check := time.NewTicker(every)
	defer check.Stop()
	for {
		select {
		case <-ticket.ready:
			return ""
		case <-ctx.Done():
			return "stop"
		case <-check.C:
			if current, err := store.Load(id); err == nil && current.Wants() == job.WantCancel {
				return "cancel"
			}
		}
	}
}

// dispatch claims each stranded inference job and executes it in its own
// goroutine under the claim's lease. A job this runtime already executes is
// skipped even after its lease is let go, so no job runs twice here; the
// store's lease keeps other runtimes off it. running counts the goroutines
// Serve waits for at shutdown.
func (e *JobExecution) dispatch(ctx context.Context, store job.Store, record *job.Record, running *sync.WaitGroup) {
	if !e.startExecuting(record.ID) {
		return
	}
	held, err := store.Claim(record.ID, "inference-service-"+job.NewID(), 30*time.Second)
	if err != nil {
		e.stopExecuting(record.ID)
		return
	}
	running.Add(1)
	go func() {
		defer running.Done()
		defer e.stopExecuting(record.ID)
		if err := e.execute(ctx, store, held); err != nil && e.onError != nil {
			e.onError(fmt.Errorf("inference job %s: %w", record.ID, err))
		}
	}()
}

func (e *JobExecution) startExecuting(id string) bool {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	if e.executing[id] {
		return false
	}
	if e.executing == nil {
		e.executing = map[string]bool{}
	}
	e.executing[id] = true
	return true
}

func (e *JobExecution) stopExecuting(id string) {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	delete(e.executing, id)
}
