package inference

import (
	"context"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestSlowAdmissionStartsIdleBudgetAtRegistration(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	var elapsed atomic.Int64
	now := func() time.Time { return started.Add(time.Duration(elapsed.Load())) }
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.Idle = 100 * time.Millisecond
		c.Now = now
		c.Admit = func(context.Context, *router.Host, string) (string, bool) {
			elapsed.Store(int64(200 * time.Millisecond))
			return "", true
		}
	})
	f.up.hold = true
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted || a.IdleMs != 100 {
		t.Fatalf("admission: %+v", a)
	}
	// Hold registration's lock while moving the fake clock, so the real
	// watchdog cannot race this deterministic inspection of its idle rule.
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	op := f.provider.ops[a.Operation]
	if op == nil || !op.started.Equal(started) {
		t.Fatal("accepted operation lost its original wall-time start")
	}
	if idle := op.idleSince(now()); idle != 0 {
		t.Fatalf("newly accepted operation is already idle for %s", idle)
	}
	elapsed.Store(int64(299 * time.Millisecond))
	if op.stopIdle(now(), f.provider.cfg.Idle) {
		t.Fatal("operation cancelled before its full admitted idle budget")
	}
	op.observeBegin()
	elapsed.Store(int64(500 * time.Millisecond))
	if op.stopIdle(now(), f.provider.cfg.Idle) {
		t.Fatal("in-flight Observe was treated as idle")
	}
	op.observeEnd(now())
	elapsed.Store(int64(599 * time.Millisecond))
	if op.stopIdle(now(), f.provider.cfg.Idle) {
		t.Fatal("idle budget did not restart at Observe completion")
	}
	elapsed.Store(int64(600 * time.Millisecond))
	if !op.stopIdle(now(), f.provider.cfg.Idle) {
		t.Fatal("unobserved operation survived its full idle budget")
	}
}

func TestProfilePreflightRefusalsNeverInvokeAdmit(t *testing.T) {
	t.Run("chat rights", func(t *testing.T) {
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.Decide = func(context.Context, Subject, string, string) (string, error) { return "denied", nil }
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		if got := f.provider.Start(context.Background(), caller, hostedRequest("openrouter")); got.Outcome != wire.StartOutcomeNotPermitted || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("chat unsupported", func(t *testing.T) {
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		req := hostedRequest("openrouter")
		req.RequiredExtensions = []string{"unknown/feature"}
		req.Extensions = map[string]string{"unknown/feature": "x"}
		if got := f.provider.Start(context.Background(), caller, req); got.Outcome != wire.StartOutcomeUnsupportedFeature || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("chat credential", func(t *testing.T) {
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.Apply = func(context.Context, Subject, string, string, string) (map[string]string, string) {
				return nil, "denied"
			}
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		if got := f.provider.Start(context.Background(), caller, hostedRequest("openrouter")); got.Outcome != wire.StartOutcomeNotPermitted || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("chat budget", func(t *testing.T) {
		ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"openrouter": {RequestsPerDay: 1}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := ceilings.Add("openrouter", 0, 0); err != nil {
			t.Fatal(err)
		}
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.Ceilings = ceilings
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		if got := f.provider.Start(context.Background(), caller, hostedRequest("openrouter")); got.Outcome != wire.StartOutcomeBudgetExceeded || got.Reason != "ceiling:requests:openrouter" || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("embed rights", func(t *testing.T) {
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.Decide = func(context.Context, Subject, string, string) (string, error) { return "denied", nil }
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		if got := f.provider.Embed(context.Background(), caller, embedRequest("hello")); got.Outcome != wire.EmbedOutcomeNotPermitted || calls != 0 {
			t.Fatalf("embedding %+v, hook calls %d", got, calls)
		}
	})
	t.Run("transcription input", func(t *testing.T) {
		calls := 0
		f, audio := transcriptionFixture(t, func(c *Config) {
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		req := transcriptionRequest(audio)
		req.AudioDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if got := f.provider.StartTranscription(context.Background(), caller, req); got.Outcome != wire.StartOutcomeInvalid || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("speech output", func(t *testing.T) {
		calls := 0
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.PrepareContentWrite = func(context.Context, Subject, string, string, int64) (ContentCommitter, ContentOutcome) {
				return nil, ContentForbidden
			}
			c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		})
		if got := f.provider.StartSpeech(context.Background(), caller, speechRequest("hello")); got.Outcome != wire.StartOutcomeForbidden || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("image output", func(t *testing.T) {
		calls := 0
		f := newImageFixture(t, func(http.ResponseWriter, *http.Request, []byte) {})
		f.store.deny = true
		f.provider.cfg.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		if got := f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1)); got.Outcome != wire.StartOutcomeForbidden || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("image input", func(t *testing.T) {
		calls := 0
		f := newImageFixture(t, func(http.ResponseWriter, *http.Request, []byte) {})
		f.provider.cfg.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) { return nil, ContentUnknown }
		f.provider.cfg.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		req := imageRequestFixture(wire.ImageModeEdit, 1)
		req.ImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		req.ImageMediaType = "image/png"
		if got := f.provider.StartImage(context.Background(), caller, req); got.Outcome != wire.StartOutcomeInvalid || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
	t.Run("live rights", func(t *testing.T) {
		calls := 0
		f := newLiveFixture(t, true)
		f.p.cfg.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
		if got := f.p.StartLive(context.Background(), caller, liveRequest()); got.Outcome != wire.StartOutcomeNotPermitted || calls != 0 {
			t.Fatalf("admission %+v, hook calls %d", got, calls)
		}
	})
}

func TestChatAdmissionKeepsModelActivityUntilOperationRegistered(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.Admit = func(_ context.Context, _ *router.Host, model string) (string, bool) {
			close(entered)
			<-release
			return "", true
		}
	})
	admitted := make(chan wire.Admission, 1)
	go func() { admitted <- f.provider.Start(context.Background(), caller, hostedRequest("openrouter")) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("admit hook did not start")
	}
	if !f.provider.ActiveForModel("anthropic/claude-sonnet-5") {
		t.Fatal("pending model activity disappeared during hook")
	}
	close(release)
	a := <-admitted
	if a.Outcome != wire.StartOutcomeAccepted || !f.provider.ActiveForModel("anthropic/claude-sonnet-5") {
		t.Fatalf("admission %+v lost model activity", a)
	}
	if result := f.provider.Cancel(caller, a.Operation); result.Outcome != wire.CancelOutcomeCancelled && result.Outcome != wire.CancelOutcomeEnded {
		t.Fatalf("cancel %+v", result)
	}
}

func TestImageAdmissionRetainsBoundGenerationAfterCancellation(t *testing.T) {
	f := newImageFixture(t, func(http.ResponseWriter, *http.Request, []byte) {})
	host := f.provider.cfg.Router.Hosts()[0]
	host.BindingID = "image-generation-one"
	f.provider.cfg.Router.SetHosts(host)
	ctx := WithExecutionBinding(context.Background(), ExecutionBinding{Host: host.Name, BindingID: host.BindingID})
	f.provider.cfg.Admit = func(context.Context, *router.Host, string) (string, bool) { return "", true }
	a := f.provider.StartImage(ctx, caller, imageRequestFixture(wire.ImageModeGenerate, 1))
	if a.Outcome != wire.StartOutcomeAccepted || !f.provider.HasBindingOperations(host.BindingID) {
		t.Fatalf("admission %+v did not retain bound generation", a)
	}
	_ = f.provider.CancelImage(caller, a.Operation)
	if !f.provider.HasBindingOperations(host.BindingID) {
		t.Fatal("cancel discarded retained generation before operation expiry")
	}
}

func TestEveryProfileHonorsProviderAdmissionRefusal(t *testing.T) {
	// The hook also receives the request's profile (INF-A5), so it can refuse
	// a model its server declares for another profile only (ROUTE-D9).
	check := func(t *testing.T, p *Provider, profile string, call func() bool) {
		t.Helper()
		calls := 0
		seen := ""
		p.cfg.Admit = func(ctx context.Context, _ *router.Host, _ string) (string, bool) {
			calls++
			seen = AdmissionProfile(ctx)
			return "engine:none", false
		}
		handled := call()
		if !handled || calls != 1 || seen != profile {
			t.Fatalf("provider refusal handled=%v, hook calls=%d, profile %q, want %q", handled, calls, seen, profile)
		}
		p.mu.Lock()
		registered := len(p.ops)
		p.mu.Unlock()
		if registered != 0 {
			t.Fatal("refused admission registered an operation")
		}
	}
	t.Run("chat", func(t *testing.T) {
		f := setup(t, router.WireOpenAICompatible, nil)
		check(t, f.provider, router.ProfileChat, func() bool {
			return f.provider.Start(context.Background(), caller, hostedRequest("openrouter")).Outcome == wire.StartOutcomeUnavailable
		})
	})
	t.Run("embed", func(t *testing.T) {
		f := setup(t, router.WireOpenAICompatible, nil)
		check(t, f.provider, router.ProfileEmbed, func() bool {
			return f.provider.Embed(context.Background(), caller, embedRequest("hello")).Outcome == wire.EmbedOutcomeUnavailable
		})
	})
	t.Run("transcription", func(t *testing.T) {
		f, audio := transcriptionFixture(t, nil)
		check(t, f.provider, router.ProfileTranscription, func() bool {
			return f.provider.StartTranscription(context.Background(), caller, transcriptionRequest(audio)).Outcome == wire.StartOutcomeUnavailable
		})
	})
	t.Run("speech", func(t *testing.T) {
		store := &speechStore{}
		f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.PrepareContentWrite = store.prepare })
		check(t, f.provider, router.ProfileSpeech, func() bool {
			return f.provider.StartSpeech(context.Background(), caller, speechRequest("hello")).Outcome == wire.StartOutcomeUnavailable
		})
	})
	t.Run("image", func(t *testing.T) {
		f := newImageFixture(t, func(http.ResponseWriter, *http.Request, []byte) {})
		check(t, f.provider, router.ProfileImage, func() bool {
			return f.provider.StartImage(context.Background(), caller, imageRequestFixture(wire.ImageModeGenerate, 1)).Outcome == wire.StartOutcomeUnavailable
		})
	})
	t.Run("live", func(t *testing.T) {
		f := newLiveFixture(t, false)
		check(t, f.p, router.ProfileLive, func() bool {
			return f.p.StartLive(context.Background(), caller, liveRequest()).Outcome == wire.StartOutcomeUnavailable
		})
	})
}

func TestCancelledPreflightDoesNotDispatchOrInvokeAdmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.Apply = func(context.Context, Subject, string, string, string) (map[string]string, string) {
			cancel()
			return map[string]string{"Authorization": "Bearer fixture"}, "applied"
		}
		c.Admit = func(context.Context, *router.Host, string) (string, bool) { calls++; return "", true }
	})
	a := f.provider.Start(ctx, caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeUnavailable || a.Operation != "" || calls != 0 || len(f.up.posts()) != 0 {
		t.Fatalf("admission %+v, hook calls=%d, posts=%d", a, calls, len(f.up.posts()))
	}
}

func TestCancelledDuringAdmitDoesNotRegisterOrDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.Admit = func(context.Context, *router.Host, string) (string, bool) { cancel(); return "", true }
	})
	a := f.provider.Start(ctx, caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeUnavailable || a.Operation != "" || len(f.up.posts()) != 0 {
		t.Fatalf("admission %+v, posts=%d", a, len(f.up.posts()))
	}
}

func TestCancelledWhileRegistrationLockHeldRefusesAtomically(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	p := f.provider
	host := p.cfg.Router.Hosts()[0]
	activity, _ := p.reserveModelActivity("anthropic/claude-sonnet-5", &caller)
	if activity == nil {
		t.Fatal("model reservation unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op := newOperation("blocked-registration", caller, host, "anthropic/claude-sonnet-5", "", p.cfg.RetainedDeltas, p.cfg.Now())
	p.mu.Lock()
	result := make(chan wire.StartOutcome, 1)
	go func() {
		outcome, _ := p.admitOperationWithActivity(ctx, op, activity)
		result <- outcome
	}()
	cancel()
	p.mu.Unlock()
	if outcome := <-result; outcome != wire.StartOutcomeUnavailable {
		t.Fatalf("cancelled registration outcome %s", outcome)
	}
	p.releaseModelActivity(activity)
	p.mu.Lock()
	_, registered := p.ops[op.id]
	p.mu.Unlock()
	if p.ActiveForModel("anthropic/claude-sonnet-5") || registered || len(f.up.posts()) != 0 {
		t.Fatal("cancelled registration retained activity, operation or dispatched backend work")
	}
}

func TestPendingCapacityRefusesSecondCallBeforeAdmit(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		name := "hook refusal"
		if cancelFirst {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			f := setup(t, router.WireOpenAICompatible, func(c *Config) {
				c.MaxOperations = 1
				c.Admit = func(context.Context, *router.Host, string) (string, bool) {
					if calls.Add(1) == 1 {
						close(entered)
						<-release
						if cancelFirst {
							return "", true
						}
					}
					return "engine:none", false
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := make(chan wire.Admission, 1)
			go func() { first <- f.provider.Start(ctx, caller, hostedRequest("openrouter")) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first admission did not reach hook")
			}
			second := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
			if second.Outcome != wire.StartOutcomeExhausted || second.Reason != "capacity" || calls.Load() != 1 {
				t.Fatalf("second admission %+v, hook calls=%d", second, calls.Load())
			}
			if cancelFirst {
				cancel()
			}
			close(release)
			if a := <-first; a.Outcome != wire.StartOutcomeUnavailable {
				t.Fatalf("first admission %+v", a)
			}
			third := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
			if third.Outcome != wire.StartOutcomeUnavailable || third.Reason != "engine:none" || calls.Load() != 2 {
				t.Fatalf("third admission %+v, hook calls=%d", third, calls.Load())
			}
			if f.provider.ActiveForModel("anthropic/claude-sonnet-5") || len(f.up.posts()) != 0 {
				t.Fatal("refusals retained model activity or dispatched work")
			}
		})
	}
}
