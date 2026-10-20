package inference

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChatUseCodecAndRequestIdentity(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	request := chat3HostedRequest(t, client)
	admission, err := client.Start(request)
	if err != nil || admission.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatal(admission, err)
	}
	use := wire.ChatUseTitle
	request.Use = &use
	changed, err := client.Start(request)
	if err != nil || changed.Outcome != wire.ChatStartOutcomeConflict {
		t.Fatal("request use was omitted from canonical body equality", changed, err)
	}
	raw := wire.EncodeRequestV3(&request)
	decoded, err := wire.DecodeRequestV3(raw)
	if err != nil || decoded.Use == nil || *decoded.Use != use {
		t.Fatal("request use did not round trip", err)
	}
	unknown := bytes.Replace(raw, []byte(`"title"`), []byte(`"unknown"`), 1)
	if bytes.Equal(raw, unknown) {
		t.Fatal("unknown-use control did not change the wire")
	}
	if _, err := wire.DecodeRequestV3(unknown); err == nil || !strings.Contains(err.Error(), "bad_enum") {
		t.Fatal("unknown request use was accepted", err)
	}
}

func TestChatUseReachesExistingExecutionWithoutChangingScope(t *testing.T) {
	for _, name := range []string{"ordinary", "title"} {
		t.Run(name, func(t *testing.T) {
			present := name == "title"
			seen := make(chan bool, 1)
			f := setup(t, router.WireOpenAICompatible, func(config *Config) {
				config.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
					use, exists := ExecutionUse(ctx)
					scope, affinity, _ := ExecutionCacheScope(ctx)
					want, _ := chatCacheScope(caller, "same-conversation")
					seen <- exists == present && (!exists || use == wire.ChatUseTitle) && affinity && scope == want
					return ExecutionPreparation{Reason: "fixture:stop"}
				}
			})
			request := chat3RequestFixture()
			request.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
			request.Target.Model, request.Content.Credential = "anthropic/claude-sonnet-5", "openrouter"
			request.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}
			request.Conversation = "same-conversation"
			if present {
				use := wire.ChatUseTitle
				request.Use = &use
			}
			admission := f.provider.StartV3(WithRoute(t.Context(), RouteNative, "program"), caller, request)
			if admission.Outcome != wire.ChatStartOutcomeAccepted {
				t.Fatal(admission)
			}
			select {
			case correct := <-seen:
				if !correct {
					t.Fatal("execution use or caller-derived scope changed")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("execution owner was not called")
			}
			if len(f.up.posts()) != 0 {
				t.Fatal("fixture refusal reached upstream")
			}
		})
	}
}

func TestChatUseSurvivesNativeAndRemoteRelay(t *testing.T) {
	for _, kind := range []string{router.WireNative, router.WireRemote} {
		f := setup(t, router.WireOpenAICompatible, nil)
		op := chat3RelayOperation(t, f.provider)
		op.host.Wire = kind
		use := wire.ChatUseTitle
		op.chat3.request.Use = &use
		relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnavailable}}
		f.provider.runChat3Relay(t.Context(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
		if len(relay.starts) != 1 || relay.starts[0].Use == nil || *relay.starts[0].Use != use || relay.starts[0].Target.Model != "model" {
			t.Fatal("relay lost request purpose or target replacement", relay.starts)
		}
	}
}
