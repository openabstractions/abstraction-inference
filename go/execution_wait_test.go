package inference

import (
	"context"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChatAdmissionExecutionWaitOwnsAcceptedLifetime(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait_deadline", true: "explicit_cancel"}[explicit], func(t *testing.T) {
			entered, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			f := setup(t, router.WireOpenAICompatible, func(c *Config) {
				c.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
					close(entered)
					select {
					case <-release:
						return ExecutionPreparation{Ready: true}
					case <-ctx.Done():
						close(stopped)
						return ExecutionPreparation{Reason: "caller"}
					}
				}
			})
			callerCtx, stopCaller := context.WithCancel(context.Background())
			a := f.provider.Start(callerCtx, caller, hostedRequest("openrouter"))
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("not accepted: %+v", a)
			}
			<-entered
			stopCaller()
			select {
			case <-stopped:
				t.Fatal("caller wait cancelled accepted work")
			default:
			}
			if !f.provider.ActiveForModel(a.Model) {
				t.Fatal("loading operation lost activity")
			}
			if explicit {
				f.provider.Cancel(caller, a.Operation)
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("explicit cancel did not stop execution wait")
				}
			} else {
				close(release)
			}
		})
	}
}

func TestChatAdmissionExecutionFailureIsNotAnAuthorityRefusal(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.WaitExecution = func(context.Context, *router.Host, string) ExecutionPreparation {
			return ExecutionPreparation{Reason: "not_permitted"}
		}
	})
	a := f.provider.Start(context.Background(), caller, hostedRequest("openrouter"))
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(a)
	}
	page := f.provider.Observe(context.Background(), caller, a.Operation, 0, 256, 65536, 1000)
	for _, delta := range page.Deltas {
		if delta.Kind == wire.DeltaKindEnd {
			if delta.End.Outcome != wire.ReplyOutcomeUnavailable || delta.End.Reason != "not_permitted" {
				t.Fatalf("terminal %+v", delta.End)
			}
			return
		}
	}
	t.Fatalf("no failed terminal projection: %+v", page)
}
