package inference

import (
	"context"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3ReasoningOnlyTerminalUsesQ8AndPreservesUsage(t *testing.T) {
	for _, kind := range []string{router.WireOpenAICompatible, router.WireAnthropicMessages} {
		for _, mode := range []string{"reasoning_only", "mixed", "empty", "failed"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				f := setup(t, kind, nil)
				f.up.chunks = nil
				f.up.reasoning = nil
				if mode != "empty" {
					f.up.reasoning = []string{"private reasoning"}
				}
				if mode == "mixed" {
					f.up.chunks = []string{"answer"}
				}
				if mode == "failed" {
					f.up.streamDrop = true
					if kind == router.WireAnthropicMessages {
						f.up.chatStatus = 503
						f.up.chatBody = `{}`
					}
				}
				f.up.input, f.up.output = 22, 128
				if kind == router.WireOpenAICompatible {
					f.up.streamExtension = `{"usage":{"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":128}}}`
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, ctx}})
				req := chat3HostedRequest(t, client)
				if kind == router.WireAnthropicMessages {
					req.Target.Model = "claude-sonnet-5"
					req.Content.Credential = "anthropic"
				}
				a, err := client.Start(req)
				if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
					t.Fatalf("admission %v %v", a.Outcome, err)
				}
				var end *wire.ChatResult
				var cursor int64
				visible, reasoning := 0, 0
				for end == nil && ctx.Err() == nil {
					page, err := client.Observe(a.Request, cursor, 256, 65536, 100)
					if err != nil || page.Outcome != wire.PageOutcomePage {
						t.Fatalf("observe %v %v", page.Outcome, err)
					}
					for _, event := range page.Events {
						if event.End != nil {
							end = event.End
						}
						if event.Part != nil {
							if event.Part.Kind == wire.ChatPartKindText {
								visible += len(event.Part.Text)
							}
							if event.Part.Kind == wire.ChatPartKindReasoning {
								reasoning += len(event.Part.Text)
							}
						}
					}
					cursor = page.Next
				}
				if end == nil {
					t.Fatal("terminal missing")
				}
				if mode == "reasoning_only" {
					if end.Outcome != wire.ChatResultOutcomeFailed || end.Cause != ReasonReasoningOnly || visible != 0 || reasoning == 0 {
						t.Fatalf("Q8 terminal outcome=%s cause=%s visible=%d reasoning=%d", end.Outcome, end.Cause, visible, reasoning)
					}
					if end.Usage.Input == nil || *end.Usage.Input != 22 || end.Usage.Output == nil || *end.Usage.Output != 128 {
						t.Fatal("reasoning-only usage lost")
					}
					if kind == router.WireOpenAICompatible && (end.Usage.CacheRead == nil || *end.Usage.CacheRead != 0 || end.Usage.Reasoning == nil || *end.Usage.Reasoning != 128) {
						t.Fatal("optional zero/cache/reasoning usage lost")
					}
				} else if mode == "failed" {
					if end.Outcome != wire.ChatResultOutcomeFailed || end.Cause == ReasonReasoningOnly {
						t.Fatalf("upstream failure overwritten: %s/%s", end.Outcome, end.Cause)
					}
				} else if end.Outcome != wire.ChatResultOutcomeCompleted || end.Cause != "" {
					t.Fatalf("completion changed: %s/%s", end.Outcome, end.Cause)
				}
				if len(f.up.posts()) != 1 {
					t.Fatal("terminal handling submitted extra inference")
				}
			})
		}
	}
}
