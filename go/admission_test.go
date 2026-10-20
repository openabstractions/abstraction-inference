package inference

import (
	"context"
	"errors"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdmissionRechecksAuthorityAfterTheProviderWait(t *testing.T) {
	for _, profile := range []string{"chat", "embed"} {
		for _, change := range []string{"complete", "credential", "unavailable", "caller"} {
			t.Run(profile+"/"+change, func(t *testing.T) {
				var changed atomic.Bool
				var checks atomic.Int32
				entered := make(chan string, 1)
				release := make(chan struct{})
				f := setup(t, router.WireOpenAICompatible, func(c *Config) {
					c.Standing = func(_ context.Context, subject Subject, host, credential string) (string, error) {
						checks.Add(1)
						if subject != caller || host != "openrouter" || credential != "openrouter" {
							t.Errorf("standing lost authority: %+v %s %s", subject, host, credential)
						}
						if changed.Load() {
							switch change {
							case "complete":
								return "rights:denied", nil
							case "credential":
								return "credential:not_permitted", nil
							case "unavailable":
								return "", errors.New("fixture policy unavailable")
							}
						}
						return "", nil
					}
					c.Admit = func(ctx context.Context, _ *router.Host, model string) (string, bool) {
						entered <- model
						select {
						case <-release:
						case <-ctx.Done():
						}
						return "", true
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				returned := make(chan string, 1)
				go func() {
					if profile == "embed" {
						v := f.provider.Embed(ctx, caller, embedRequest("inert"))
						returned <- v.Outcome.String() + " " + v.Reason
					} else {
						v := f.provider.Start(ctx, caller, hostedRequest("openrouter"))
						returned <- v.Outcome.String() + " " + v.Reason
					}
				}()
				var model string
				select {
				case model = <-entered:
				case <-time.After(time.Second):
					t.Fatal("provider admission hook not reached")
				}
				if !f.provider.ActiveForModel(model) {
					t.Error("blocked hook has no reservation")
				}
				f.provider.mu.Lock()
				pending := f.provider.pendingSubjects[caller]
				f.provider.mu.Unlock()
				if profile == "embed" && pending != 0 || profile == "chat" && pending != 1 {
					t.Errorf("reservation subject changed: %s pending=%d", profile, pending)
				}
				changed.Store(true)
				if change == "caller" {
					cancel()
				}
				close(release)
				want := "not_permitted rights:denied"
				switch change {
				case "credential":
					want = "not_permitted credential:not_permitted"
				case "unavailable":
					want = "unavailable rights:unavailable"
				case "caller":
					want = "unavailable caller"
				}
				select {
				case got := <-returned:
					if got != want {
						t.Errorf("post-hook authority: got %q want %q", got, want)
					}
				case <-time.After(time.Second):
					t.Fatal("admission did not finish")
				}
				if change != "caller" && checks.Load() < 2 {
					t.Error("authority not read after blocked hook")
				}
				if f.provider.ActiveForModel(model) {
					t.Error("refused admission retained model activity")
				}
				f.provider.mu.Lock()
				ops, models, subjects := len(f.provider.ops), len(f.provider.pendingModels), len(f.provider.pendingSubjects)
				f.provider.mu.Unlock()
				if ops != 0 || models != 0 || subjects != 0 {
					t.Errorf("refusal leaked state: ops=%d models=%d subjects=%d", ops, models, subjects)
				}
				if posts := len(f.up.posts()); posts != 0 {
					t.Errorf("refused admission sent %d upstream requests", posts)
				}
			})
		}
	}
}

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
		if r.Outcome != wire.StartOutcomeUnavailable || r.Reason != "provider:closed" {
			t.Fatalf("closed provider admitted: %+v", r)
		}
	}
	if result := f.provider.Embed(context.Background(), caller, embedRequest("hello")); result.Outcome != wire.EmbedOutcomeUnavailable || result.Reason != "provider:closed" {
		t.Fatalf("closed provider embedding: %+v", result)
	}
	if n := len(f.up.posts()); n != 0 {
		t.Fatalf("closed provider sent %d upstream requests", n)
	}
}
