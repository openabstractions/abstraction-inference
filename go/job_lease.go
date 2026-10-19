package inference

import (
	"context"
	"errors"
	"sync"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

// errLeaseLost reports a lease whose renewal did not succeed within its
// duration: another runtime may claim the job from then on (INF-G9).
var errLeaseLost = errors.New("inference job: the lease was not renewed within its duration")

// errLeaseStopped reports a renewal asked for after the lease was let go.
var errLeaseStopped = errors.New("inference job: the lease was let go")

// inferenceLease keeps one claimed job's lease and proves it within its
// duration. Every wait for the store ends by the lease's deadline: a store
// call that hangs is left to return in the background, and the job stops.
type inferenceLease struct {
	store  job.Store
	id     string
	epoch  int64
	ttl    time.Duration
	cancel context.CancelFunc
	done   chan struct{} // closed by stop
	exited chan struct{} // closed when the keeper goroutine returns
	// lost closes when the deadline passes without a renewal or a renewal
	// fails; the job's context is cancelled with it.
	lost chan struct{}

	mu       sync.Mutex
	stopped  bool
	isLost   bool
	pending  *leaseRenewal // the store call in flight, shared by its callers
	deadline *time.Timer
	gen      int // the deadline timer that may still expire the lease
}

// leaseRenewal is one store renewal; done closes when the store returned.
type leaseRenewal struct {
	done chan struct{}
	err  error
}

func holdInferenceLease(parent context.Context, store job.Store, record *job.Record, ttl time.Duration) (context.Context, *inferenceLease) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	lease := &inferenceLease{store: store, id: record.ID, epoch: record.Lease.Epoch, ttl: ttl, cancel: cancel,
		done: make(chan struct{}), exited: make(chan struct{}), lost: make(chan struct{})}
	until := ttl
	if expires := record.Lease.ExpiresAt; !expires.IsZero() {
		until = time.Until(expires.Time)
	}
	lease.mu.Lock()
	lease.arm(until)
	lease.mu.Unlock()
	go lease.hold()
	return ctx, lease
}

// arm sets the deadline to until from now. The caller holds mu.
func (l *inferenceLease) arm(until time.Duration) {
	l.gen++
	gen := l.gen
	if l.deadline != nil {
		l.deadline.Stop()
	}
	l.deadline = time.AfterFunc(until, func() { l.expire(gen) })
}

// expire loses the lease when timer gen is still the current deadline.
func (l *inferenceLease) expire(gen int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if gen == l.gen && !l.stopped {
		l.loseLocked()
	}
}

// loseLocked marks the lease lost and cancels the job. The caller holds mu.
func (l *inferenceLease) loseLocked() {
	if !l.isLost {
		l.isLost = true
		close(l.lost)
	}
	l.cancel()
}

func (l *inferenceLease) hold() {
	defer close(l.exited)
	every := l.ttl / 3
	if every < time.Millisecond {
		every = time.Millisecond
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-l.lost:
			return
		case <-ticker.C:
			//unchecked: a failed or late renewal already loses the lease and cancels the job; this background goroutine has no caller left to report the error to
			_ = l.renew()
		}
	}
}

// renew proves the lease again. It returns once the store answered, the
// deadline passed (errLeaseLost) or the lease was let go (errLeaseStopped),
// whichever comes first; concurrent callers share one store call.
func (l *inferenceLease) renew() error {
	l.mu.Lock()
	switch {
	case l.stopped:
		l.mu.Unlock()
		return errLeaseStopped
	case l.isLost:
		l.mu.Unlock()
		return errLeaseLost
	}
	call := l.pending
	if call == nil {
		call = &leaseRenewal{done: make(chan struct{})}
		l.pending = call
		go l.renewInStore(call)
	}
	l.mu.Unlock()
	select {
	case <-call.done:
		return call.err
	case <-l.lost:
		return errLeaseLost
	case <-l.done:
		return errLeaseStopped
	}
}

// renewInStore runs one store renewal, which may hang; it outlives the
// deadline until the store returns, and its answer then changes nothing.
func (l *inferenceLease) renewInStore(call *leaseRenewal) {
	asked := time.Now()
	renewed, err := l.store.Renew(l.id, l.epoch, l.ttl)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending == call {
		l.pending = nil
	}
	switch {
	case l.isLost:
		call.err = errLeaseLost
	case err != nil:
		// Any failed renewal leaves continued authority unproved. Cancel now;
		// waiting for a later tick can run external work past the last TTL.
		call.err = err
		if !l.stopped {
			l.loseLocked()
		}
	case !l.stopped:
		// The store extended the lease to at most ttl after it was asked,
		// and a recall can shorten it.
		until := l.ttl - time.Since(asked)
		if renewed != nil && !renewed.Lease.ExpiresAt.IsZero() {
			until = min(until, time.Until(renewed.Lease.ExpiresAt.Time))
		}
		l.arm(until)
	}
	close(call.done)
}

// stop lets the lease go: the job's context ends, the deadline stops and the
// keeper returns at once, even while a renewal hangs in the store.
func (l *inferenceLease) stop() {
	l.mu.Lock()
	if !l.stopped {
		l.stopped = true
		l.gen++
		l.deadline.Stop()
		close(l.done)
	}
	l.mu.Unlock()
	l.cancel()
	<-l.exited
}
