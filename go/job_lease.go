package inference

import (
	"context"
	"sync"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

type inferenceLease struct {
	store  job.Store
	id     string
	epoch  int64
	ttl    time.Duration
	cancel context.CancelFunc
	done   chan struct{}
	exited chan struct{}
	once   sync.Once
	mu     sync.Mutex
}

func holdInferenceLease(parent context.Context, store job.Store, record *job.Record, ttl time.Duration) (context.Context, *inferenceLease) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	lease := &inferenceLease{store: store, id: record.ID, epoch: record.Lease.Epoch, ttl: ttl, cancel: cancel, done: make(chan struct{}), exited: make(chan struct{})}
	go lease.hold()
	return ctx, lease
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
		case <-ticker.C:
			//unchecked: renew already calls l.cancel() internally on failure; this background goroutine has no caller left to report the error to
			_ = l.renew()
		}
	}
}

func (l *inferenceLease) renew() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.store.Renew(l.id, l.epoch, l.ttl)
	if err != nil {
		// Any failed renewal leaves continued authority unproved. Cancel now;
		// waiting for a later tick can run external work past the last TTL.
		l.cancel()
	}
	return err
}

func (l *inferenceLease) stop() {
	l.cancel()
	l.once.Do(func() { close(l.done) })
	<-l.exited
}
