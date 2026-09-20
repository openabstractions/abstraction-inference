package inference

import (
	"context"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"testing"
	"time"
)

// Both requests reach credential application after the early capacity check.
// Hold them there to make concurrent admission deterministic.
func TestConcurrentProfilesShareAtomicCapacity(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	f, audio := transcriptionFixture(t, func(c *Config) {
		c.MaxOperations = 1
		apply := c.Apply
		c.Apply = func(ctx context.Context, subject Subject, consumer, credential, target string) (map[string]string, string) {
			entered <- struct{}{}
			<-release
			return apply(ctx, subject, consumer, credential, target)
		}
	})
	f.up.hold = true
	f.up.transcriptionHold = true
	replies := make(chan wire.Admission, 2)
	go func() { replies <- f.provider.Start(context.Background(), caller, hostedRequest("openrouter")) }()
	go func() {
		replies <- f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio))
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("request did not reach admission barrier")
		}
	}
	close(release)
	accepted, exhausted := 0, 0
	for i := 0; i < 2; i++ {
		r := <-replies
		switch r.Outcome {
		case wire.StartOutcomeAccepted:
			accepted++
		case wire.StartOutcomeExhausted:
			exhausted++
		default:
			t.Fatalf("unexpected admission: %+v", r)
		}
	}
	if accepted != 1 || exhausted != 1 {
		t.Fatalf("accepted=%d exhausted=%d; want one of each", accepted, exhausted)
	}
}

func TestStartAfterCloseRefusesBeforeUpstream(t *testing.T) {
	f, audio := transcriptionFixture(t, nil)
	f.provider.Close()
	for _, r := range []wire.Admission{
		f.provider.Start(context.Background(), caller, hostedRequest("openrouter")),
		f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio)),
	} {
		if r.Outcome != wire.StartOutcomeUnavailable {
			t.Fatalf("closed provider admitted: %+v", r)
		}
	}
	if n := len(f.up.posts()); n != 0 {
		t.Fatalf("closed provider sent %d upstream requests", n)
	}
}
