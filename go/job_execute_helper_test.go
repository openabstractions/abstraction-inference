package inference

import (
	"context"
	"testing"

	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
)

// executeUntilCleanup runs p.Execute in the background and returns the
// function that stops it and waits for it to return. The test's cleanup
// calls it too, before the temporary folders the provider writes into are
// removed: an executor still writing its job store fails that removal.
func executeUntilCleanup(t *testing.T, p *acceptanceprovider.Provider) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.Execute(ctx)
	}()
	stop = func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return stop
}
