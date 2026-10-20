package inference

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestChatCacheScopeForwardingRequiresRecheckedDesignatedNativeRuntime(t *testing.T) {
	key, _ := chatCacheScope(Subject{Account: "account", Program: "actual-app"}, "conversation")
	values := map[string]string{CacheScopeExtension: key, ClaimExtension: "audit-only"}
	native := WithRoute(context.Background(), RouteNative, "program")
	trusted := WithDesignatedRuntimeRecheck(native, func() error { return nil })
	for _, who := range []Subject{{}, {Account: "account"}, {Program: "runtime"}} {
		if got, reason := receivedChatCacheBinding(trusted, who, values, "conversation", false); reason == "" || got.scope != "" {
			t.Fatal("forwarded scope admitted without a bound native caller")
		}
	}
	for _, ctx := range []context.Context{context.Background(), native, WithDesignatedRuntime(native), WithDesignatedRuntimeRecheck(WithRemote(context.Background(), "other", "remote"), func() error { return nil }), WithDesignatedRuntimeRecheck(native, func() error { return errors.New("runtime process replaced") })} {
		if got, reason := receivedChatCacheBinding(ctx, Subject{Account: "account", Program: "runtime"}, values, "conversation", false); reason == "" || got.scope != "" {
			t.Fatalf("untrusted forwarding admitted: %+v %s", got, reason)
		}
	}
	got, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, values, "conversation", false)
	if reason != "" || got.scope != key || !got.affinity || !got.persist {
		t.Fatalf("trusted leaf rehashed or lost admitted scope: %+v %s", got, reason)
	}
	for _, value := range []string{"", strings.TrimPrefix(key, "sha256:"), strings.ToUpper(key), "../state"} {
		if _, reason := receivedChatCacheBinding(trusted, caller, map[string]string{CacheScopeExtension: value}, "conversation", false); reason == "" {
			t.Fatalf("invalid forwarded digest admitted: %q", value)
		}
	}
	replaced := false
	checked := WithDesignatedRuntimeRecheck(native, func() error {
		if replaced {
			return errors.New("runtime replaced")
		}
		return nil
	})
	if _, reason := receivedChatCacheBinding(checked, caller, values, "conversation", false); reason != "" {
		t.Fatal(reason)
	}
	replaced = true
	if _, reason := receivedChatCacheBinding(checked, caller, values, "conversation", false); reason == "" {
		t.Fatal("copied context trusted a replaced runtime")
	}
}

func TestChatCacheForwardingPreservesAppPartitionAndNoRetention(t *testing.T) {
	native := WithRoute(context.Background(), RouteNative, "program")
	trusted := WithDesignatedRuntimeRecheck(native, func() error { return nil })
	for _, app := range []string{"app-a", "app-b"} {
		who := Subject{Account: "account", Program: app}
		binding, reason := receivedChatCacheBinding(native, who, nil, "same-label", false)
		want, _ := chatCacheScope(who, "same-label")
		if reason != "" || binding.scope != want {
			t.Fatal("direct native app did not derive its actual peer partition")
		}
		forwarded := forwardedChatCacheExtensions(map[string]string{CacheScopeExtension: "impostor", "vendor/control": "1"}, binding)
		leaf, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, forwarded, "same-label", true)
		if reason != "" || leaf.scope != want || leaf.persist || !leaf.affinity || forwarded["vendor/control"] != "1" {
			t.Fatalf("forwarding crossed caller scope or enabled retention: %+v %s", leaf, reason)
		}
	}
	missing, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, map[string]string{ClaimExtension: "claimed-app"}, "conversation", false)
	if reason != "" || missing.scope != "" || missing.affinity || missing.persist {
		t.Fatal("audit claim or runtime program became an app cache scope")
	}
	key, _ := chatCacheScope(caller, "")
	absent, reason := receivedChatCacheBinding(trusted, caller, map[string]string{CacheScopeExtension: key}, "", false)
	if reason != "" || absent.scope != key || absent.affinity {
		t.Fatal("absent conversation gained chat affinity")
	}
}

func TestChatCacheScopeIsBoundToAttestedCallerAndConversation(t *testing.T) {
	type input struct{ account, program, conversation string }
	inputs := []input{
		{"account-a", "app-a", "private-conversation-label"},
		{"account-b", "app-a", "private-conversation-label"},
		{"account-a", "app-b", "private-conversation-label"},
		{"account-a", "app-a", "another-conversation"},
		{"ab", "c", "d"}, {"a", "bc", "d"},
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		who := Subject{Account: input.account, Program: input.program}
		key, affinity := chatCacheScope(who, input.conversation)
		if !affinity || !strings.HasPrefix(key, "sha256:") || len(key) != 71 {
			t.Fatalf("scope = %q affinity=%v", key, affinity)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(key, "sha256:")); err != nil || key != strings.ToLower(key) {
			t.Fatalf("scope does not use CONV-T3: %q", key)
		}
		if seen[key] {
			t.Fatalf("different caller/conversation tuples share %s", key)
		}
		seen[key] = true
		if again, _ := chatCacheScope(who, input.conversation); again != key {
			t.Fatal("scope changed for the same tuple")
		}
		if strings.Contains(key, "private-conversation-label") {
			t.Fatal("raw conversation label escaped into the cache key")
		}
	}
}

func TestChatCacheScopeWithoutConversationKeepsOnlyCallerPartition(t *testing.T) {
	a, affinity := chatCacheScope(Subject{Account: "account", Program: "app-a"}, "")
	b, otherAffinity := chatCacheScope(Subject{Account: "account", Program: "app-b"}, "")
	if a == "" || b == "" || a == b || affinity || otherAffinity {
		t.Fatalf("absent labels lost caller partition or selected affinity: %q/%v %q/%v", a, affinity, b, otherAffinity)
	}
	for _, who := range []Subject{{}, {Account: "account"}, {Program: "app-a"}} {
		if key, affinity := chatCacheScope(who, "conversation"); key != "" || affinity {
			t.Fatal("unattested caller received a cache scope")
		}
	}
}

func TestChatCacheProgramPartitionUsesOnlyAttestedIngress(t *testing.T) {
	native := WithRoute(t.Context(), RouteNative, "program")
	trusted := WithDesignatedRuntimeRecheck(native, func() error { return nil })
	seen := map[string]bool{}
	for _, program := range []string{"app-a", "app-b"} {
		who := Subject{Account: "account", Program: program}
		binding, reason := receivedChatCacheBinding(native, who, map[string]string{ClaimExtension: "same audit claim"}, "same-prefix", false)
		partition, _ := chatCacheScope(who, "")
		if reason != "" || binding.partition != partition || seen[partition] {
			t.Fatal(binding, reason)
		}
		seen[partition] = true
		forwarded := forwardedChatCacheExtensions(map[string]string{CachePartitionExtension: "forged"}, binding)
		leaf, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, forwarded, "same-prefix", false)
		if reason != "" || leaf.partition != partition || leaf.scope != binding.scope {
			t.Fatal(leaf, reason)
		}
		ctx := context.WithValue(native, executionCacheScopeKey{}, leaf)
		if ExecutionCachePartition(ctx) != partition {
			t.Fatal("execution lost attested partition")
		}
		remote, _ := forwardedRemoteExtensions(forwarded, nil, program)
		if _, present := remote[CachePartitionExtension]; present {
			t.Fatal("remote partition crossed as authority")
		}
		paired, reason := receivedChatCacheBinding(WithRemote(t.Context(), "peer", "tls-peer"), Subject{Account: "receiving-account", Program: "attested-peer"}, remote, "same-prefix", false)
		want, _ := chatCacheScope(Subject{Account: "receiving-account", Program: "attested-peer"}, "")
		if reason != "" || paired.partition != want {
			t.Fatal(paired, reason)
		}
		legacy, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, map[string]string{CacheScopeExtension: binding.scope}, "same-prefix", false)
		if reason != "" || legacy.partition != "" {
			t.Fatal("legacy forwarding invented a program partition", legacy, reason)
		}
		if _, reason := receivedChatCacheBinding(trusted, Subject{Account: "account", Program: "runtime"}, map[string]string{CacheScopeExtension: binding.scope, CachePartitionExtension: "bad"}, "same-prefix", false); reason == "" {
			t.Fatal("trusted forwarding admitted malformed partition")
		}
		for _, values := range []map[string]string{{CachePartitionExtension: partition}, {CacheScopeExtension: binding.scope, CachePartitionExtension: "bad"}, {CacheScopeExtension: binding.scope, CachePartitionExtension: partition}} {
			if _, reason := receivedChatCacheBinding(native, who, values, "same-prefix", false); reason == "" {
				t.Fatal("forged partition accepted")
			}
		}
	}
	if ExecutionCachePartition(context.Background()) != "" {
		t.Fatal("missing operation invented a partition")
	}
}
