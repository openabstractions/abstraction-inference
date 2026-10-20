package inference

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// resumeFixture is a provider with production idle bounds on a fake clock
// and an upstream that holds its reply after the first chunk, as a model
// still loading holds a cold request.
func resumeFixture(t *testing.T) (*fixture, func(time.Duration)) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Unix(1_000_000, 0).UnixNano())
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.Idle = 10 * time.Second
		c.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	})
	f.up.release = make(chan struct{})
	advance := func(d time.Duration) {
		clock.Add(int64(d))
		// Let the real watchdog tick (every 100 ms) on the new time.
		time.Sleep(400 * time.Millisecond)
	}
	return f, advance
}

// agentWaitCap is the MCP tool's bounded follow: one Observe that returns its
// page to a caller still connected, after which the tool answers "working"
// with the operation's handle.
func agentWaitCap(t *testing.T, p *Provider, id string) int64 {
	t.Helper()
	cursor := int64(0)
	for {
		page := p.Observe(context.Background(), caller, id, cursor, 256, 65536, 200)
		if page.Outcome != wire.PageOutcomePage || page.AtEnd {
			t.Fatalf("wait-cap page %+v", page)
		}
		if len(page.Deltas) == 0 {
			return cursor
		}
		cursor = page.Next
	}
}

func (f *fixture) ended(id string) bool {
	f.provider.mu.Lock()
	op := f.provider.ops[id]
	f.provider.mu.Unlock()
	return op != nil && op.isEnded()
}

// An agent's tool answers "working" after its wait cap and the agent resumes
// after its own turn. Idle (10 s) is shorter than that turn; the operation
// waits ResumeIdle for the resume and stays readable as long after it ends.
func TestAgentResumeAfterItsTurnCompletes(t *testing.T) {
	f, advance := resumeFixture(t)
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted || a.IdleMs != 10_000 {
		t.Fatalf("admission %+v", a)
	}
	cursor := agentWaitCap(t, f.provider, a.Operation)

	// The agent's turn: no Observe for 15 s.
	advance(15 * time.Second)
	if f.ended(a.Operation) {
		_, reply, _ := drain(t, f.provider, caller, a.Operation)
		t.Fatalf("operation ended while its caller held the handle: %+v", reply)
	}

	// The model finishes while the agent is still away, and the agent comes
	// back after the provider's one-minute retention.
	close(f.up.release)
	deadline := time.Now().Add(5 * time.Second)
	for !f.ended(a.Operation) {
		if time.Now().After(deadline) {
			t.Fatal("upstream reply never ended the operation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	advance(2 * time.Minute)

	var text string
	var reply wire.Reply
	for {
		page := f.provider.Observe(context.Background(), caller, a.Operation, cursor, 256, 65536, 1000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("resume page %+v", page)
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindPart && d.Part != nil {
				text += d.Part.Text
			}
			if d.Kind == wire.DeltaKindEnd && d.End != nil {
				reply = *d.End
			}
		}
		cursor = page.Next
		if page.AtEnd {
			break
		}
	}
	if reply.Outcome != wire.ReplyOutcomeCompleted || (text != "lo!" && text != "Hello!") {
		t.Fatalf("resumed reply %+v text %q", reply, text)
	}
}

// A handle nobody resumes still ends: past ResumeIdle the operation is
// cancelled with reason idle and its upstream request closed.
func TestAbandonedHandleEndsIdleAfterTheGrace(t *testing.T) {
	f, advance := resumeFixture(t)
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	agentWaitCap(t, f.provider, a.Operation)

	advance(2 * time.Minute)
	if f.ended(a.Operation) {
		t.Fatal("operation ended inside the resume grace")
	}
	advance(DefaultResumeIdle - 2*time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for !f.ended(a.Operation) {
		if time.Now().After(deadline) {
			t.Fatal("an abandoned handle outlived the resume grace")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeCancelled || reply.Reason != "idle" {
		t.Fatalf("reply %+v", reply)
	}
}

// A caller that leaves in the middle of an Observe has gone: Idle applies.
func TestCallerLeavingMidObserveEndsAfterIdle(t *testing.T) {
	f, advance := resumeFixture(t)
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	cursor := agentWaitCap(t, f.provider, a.Operation)
	gone, leave := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); leave() }()
	f.provider.Observe(gone, caller, a.Operation, cursor, 256, 65536, 5000)

	advance(10 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for !f.ended(a.Operation) {
		if time.Now().After(deadline) {
			t.Fatal("a departed caller's operation outlived Idle")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, reply, _ := drain(t, f.provider, caller, a.Operation)
	if reply.Outcome != wire.ReplyOutcomeCancelled || reply.Reason != "idle" {
		t.Fatalf("reply %+v", reply)
	}
}
