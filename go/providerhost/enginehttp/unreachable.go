package enginehttp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"
)

// ErrEngineUnreachable is a connect to an engine's endpoint that failed for a
// reason waiting does not cure: a Windows AF_UNIX path in a packaged app's
// redirected folder reads "invalid argument" (WSAEINVAL) on every try, and
// access denied or a missing launch folder stay the same while the engine
// loads. A refused connection, a socket file not made yet, and a timeout are
// the engine not listening yet and are returned as they are (inference
// CONTRACT.md INF-EL2).
var ErrEngineUnreachable = errors.New("enginehttp: the engine's endpoint cannot be reached")

// UnreachableFor is how long a readiness probe's connects may fail with
// ErrEngineUnreachable without a break before readiness ends with that error. A
// starting engine that answers once in between restarts the count.
const UnreachableFor = 3 * time.Second

// An UnreachableRun is one readiness wait's run of ErrEngineUnreachable answers,
// the one rule every engine readiness loop applies (inference INF-EL2): the
// provider kit's engineproc.Wait and a provider's own loop alike.
type UnreachableRun struct{ since time.Time }

// Note records one probe answer's error. It returns an error wrapping
// ErrEngineUnreachable once every answer for UnreachableFor was ErrEngineUnreachable;
// any other answer, success included, ends the run.
func (r *UnreachableRun) Note(err error) error {
	if !errors.Is(err, ErrEngineUnreachable) {
		r.since = time.Time{}
		return nil
	}
	now := time.Now()
	if r.since.IsZero() {
		r.since = now
		return nil
	}
	if now.Sub(r.since) < UnreachableFor {
		return nil
	}
	return fmt.Errorf("every connect for %s failed: %w", UnreachableFor, err)
}

// dialFailure is err, a failed connect to an engine, wrapped in
// ErrEngineUnreachable unless it is the engine not listening yet.
func dialFailure(err error) error {
	if err == nil || notListeningYet(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrEngineUnreachable, err)
}

// notListeningYet reports a connect error an engine that is still starting
// gives: the connection refused, the socket file absent, a timeout, an
// interrupted or would-block connect, or the caller's own deadline.
func notListeningYet(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	// Only a system error outside the list below is unreachable; a failure
	// that carries none keeps the wait it always had.
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return true
	}
	return slices.Contains(notListeningErrnos, errno)
}
