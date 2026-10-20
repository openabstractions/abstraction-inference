package inference

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChatCacheSpoofRefusesBeforeAcceptanceOrUpstream(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	uses, posts := f.applier.count(), len(f.up.posts())
	key, _ := chatCacheScope(caller, "conversation")
	request := chat3RequestFixture()
	request.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
	request.Conversation = "conversation"
	request.Content.Extensions = map[string]string{CacheScopeExtension: key}
	native := WithRoute(context.Background(), RouteNative, "program")
	checks := 0
	replaced := WithDesignatedRuntimeRecheck(native, func() error {
		checks++
		if checks > 1 {
			return errors.New("runtime replaced before acceptance")
		}
		return nil
	})
	for _, ctx := range []context.Context{native, WithDesignatedRuntime(native), WithDesignatedRuntimeRecheck(native, func() error { return errors.New("impostor runtime") }), replaced} {
		admission := f.provider.StartV3(ctx, caller, request)
		if admission.Outcome != wire.ChatStartOutcomeInvalid || admission.Reason != "extension:"+CacheScopeExtension || admission.Request != "" {
			t.Fatalf("untrusted scope admission: %+v", admission)
		}
	}
	legacy := hostedRequest("openrouter")
	legacy.Extensions = map[string]string{CacheScopeExtension: key}
	if admission := f.provider.Start(native, caller, legacy); admission.Outcome != wire.StartOutcomeInvalid || admission.Operation != "" {
		t.Fatalf("legacy spoof admission: %+v", admission)
	}
	if checks != 2 || len(f.provider.chat3History.open.entries) != 0 || len(f.up.posts()) != posts || f.applier.count() != uses {
		t.Fatalf("scope spoof effects: rechecks=%d histories=%d posts=%d/%d credential uses=%d/%d", checks, len(f.provider.chat3History.open.entries), len(f.up.posts()), posts, f.applier.count(), uses)
	}
}

func TestChatCachePartitionSpoofRefusesBeforeAcceptanceOrUpstream(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	uses, posts := f.applier.count(), len(f.up.posts())
	request := chat3RequestFixture()
	request.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
	request.Conversation = "conversation"
	partition, _ := chatCacheScope(caller, "")
	request.Content.Extensions = map[string]string{CachePartitionExtension: partition}
	for _, ctx := range []context.Context{WithRoute(t.Context(), RouteNative, "program"), WithRemote(t.Context(), "peer", "tls-peer")} {
		admission := f.provider.StartV3(ctx, caller, request)
		if admission.Outcome != wire.ChatStartOutcomeInvalid || admission.Request != "" {
			t.Fatalf("forged partition admission: %+v", admission)
		}
	}
	if len(f.provider.chat3History.open.entries) != 0 || len(f.up.posts()) != posts || f.applier.count() != uses {
		t.Fatal("forged partition produced acceptance, credential use or upstream work")
	}
}

func TestChatCacheScopeReachesExistingExecutionOwner(t *testing.T) {
	seen := make(chan chatCacheBinding, 1)
	f := setup(t, router.WireOpenAICompatible, func(config *Config) {
		config.WaitExecution = func(ctx context.Context, _ *router.Host, _ string) ExecutionPreparation {
			scope, affinity, persist := ExecutionCacheScope(ctx)
			seen <- chatCacheBinding{scope: scope, partition: ExecutionCachePartition(ctx), affinity: affinity, persist: persist}
			return ExecutionPreparation{Reason: "fixture:stop"}
		}
	})
	request := chat3RequestFixture()
	request.Identity.HistoryEpoch = f.provider.chat3History.window().HistoryEpoch
	request.Target.Model, request.Content.Credential = "anthropic/claude-sonnet-5", "openrouter"
	request.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeHostedAllowed}
	request.Conversation = "private-conversation"
	admission := f.provider.StartV3(WithRoute(context.Background(), RouteNative, "program"), caller, request)
	if admission.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("admission: %+v", admission)
	}
	select {
	case got := <-seen:
		want, _ := chatCacheScope(caller, request.Conversation)
		partition, _ := chatCacheScope(caller, "")
		if got.scope != want || got.partition != partition || !got.affinity || !got.persist || strings.Contains(got.scope, request.Conversation) {
			t.Fatalf("execution cache scope: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("existing execution owner received no cache scope")
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("fixture execution refusal reached upstream")
	}
}

func TestChatCacheNativeRelayKeepsDerivedScopeAndNoRetention(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	op := chat3RelayOperation(t, f.provider)
	op.host.Wire = router.WireNative
	op.chat3.request.Conversation = "conversation"
	op.chat3.request.Content.Guarantees = []wire.ChatGuarantee{wire.ChatGuaranteeNoRetention}
	key, _ := chatCacheScope(caller, "conversation")
	partition, _ := chatCacheScope(caller, "")
	op.cache = chatCacheBinding{scope: key, partition: partition, affinity: true, persist: false}
	relay := &chat3RelayFixture{admission: wire.ChatAdmission{Outcome: wire.ChatStartOutcomeUnavailable}}
	f.provider.runChat3Relay(context.Background(), op, wire.Request{}, func(context.Context) chat3RelayClient { return relay })
	if len(relay.starts) != 1 {
		t.Fatal("native relay did not make its one start")
	}
	forwarded := relay.starts[0]
	if forwarded.Content.Extensions[CacheScopeExtension] != key || forwarded.Content.Extensions[CachePartitionExtension] != partition || forwarded.Content.Extensions[ClaimExtension] != caller.Program || forwarded.Conversation != "conversation" || !chat3NoRetention(forwarded.Content.Guarantees) {
		t.Fatalf("native forwarding changed scope, audit attribution or retention restriction: %+v", forwarded)
	}
	leaf, reason := receivedChatCacheBinding(WithDesignatedRuntimeRecheck(WithRoute(context.Background(), RouteNative, "program"), func() error { return nil }), Subject{Account: caller.Account, Program: "runtime"}, forwarded.Content.Extensions, forwarded.Conversation, chat3NoRetention(forwarded.Content.Guarantees))
	if reason != "" || leaf.scope != key || leaf.partition != partition || leaf.persist {
		t.Fatalf("leaf scope or no-retention changed: %+v %s", leaf, reason)
	}
}

// A chat@2 call through the runtime reaches its native leaf in the caller's
// own partition, derived from the attested account and program. Claude's MCP
// connection and OpenCode never share one; Claude's resend keeps its own.
func TestChatCacheChat2NativeLeafKeepsCallerPartition(t *testing.T) {
	claude := Subject{Account: caller.Account, Program: `C:\Program Files\OpenAbstractions\tools\oa-claude-integration.exe`}
	opencode := Subject{Account: caller.Account, Program: `C:\Users\fixture\AppData\Local\OpenCode\opencode.exe`}
	leafPartition := func(subject Subject, extensions map[string]string) string {
		t.Helper()
		binding, reason := receivedChatCacheBinding(WithRoute(t.Context(), RouteNative, "program"), subject, nil, "", false)
		if reason != "" {
			t.Fatal(reason)
		}
		op := &operation{subject: subject, cache: binding}
		prompt := []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "the same 2,131-token prompt"}}}}
		forwarded := nativeChatRequest(wire.Request{Messages: prompt, Extensions: extensions}, "qwen", op)
		leaf, reason := receivedChatCacheBinding(WithDesignatedRuntimeRecheck(WithRoute(t.Context(), RouteNative, "program"), func() error { return nil }), Subject{Account: caller.Account, Program: "runtime"}, forwarded.Extensions, "", false)
		if reason != "" || leaf.partition == "" || leaf.affinity || forwarded.Extensions[ClaimExtension] != subject.Program {
			t.Fatalf("leaf binding %+v reason=%q extensions=%v", leaf, reason, forwarded.Extensions)
		}
		return leaf.partition
	}
	first, resend, other := leafPartition(claude, nil), leafPartition(claude, nil), leafPartition(opencode, nil)
	if first != resend {
		t.Fatal("a resend from the same program changed partition")
	}
	if first == other {
		t.Fatal("two programs share one cache partition")
	}
	// A partition the request names is replaced by the caller's own.
	if got := leafPartition(claude, map[string]string{CachePartitionExtension: other, CacheScopeExtension: other}); got != first {
		t.Fatal("a request-named partition reached the leaf")
	}
}
