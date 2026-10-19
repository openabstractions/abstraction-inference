package inference

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// fixtureSeq numbers the pipes and sockets this package's fixtures listen on.
var fixtureSeq atomic.Uint64

// fixtureSuffix is a name part no other fixture of any running test process
// shares: this process's id and its own count. Go's wall clock on Windows
// steps every timer tick (0.5 ms to 15.6 ms), so a name built from
// time.Now().UnixNano() repeats for two fixtures made within one tick, and the
// second listen finds the first's pipe and answers listen.ErrTaken.
func fixtureSuffix() string {
	return fmt.Sprintf("%d-%d", os.Getpid(), fixtureSeq.Add(1))
}

// Two fixtures made back to back, as TestNativePreloadReachesOnlyAProvider
// ThatDeclaresIt makes them, never share a name, however coarse the clock.
func TestFixtureNamesDifferWithinOneClockTick(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		name := fixtureSuffix()
		if seen[name] {
			t.Fatalf("fixture name %s repeats", name)
		}
		seen[name] = true
	}
}
