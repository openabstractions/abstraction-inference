package inference

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	contentdigest "github.com/openabstractions/abstraction-storage/go/digest"
)

type chatCacheBinding struct {
	scope             string
	partition         string
	affinity, persist bool
}

type executionCacheScopeKey struct{}
type executionUseKey struct{}

// ExecutionUse reads the accepted chat@3 request's purpose. It carries no
// selection, permission, budget or cache-partition authority.
func ExecutionUse(ctx context.Context) (wire.ChatUse, bool) {
	use, present := ctx.Value(executionUseKey{}).(wire.ChatUse)
	return use, present
}

// ExecutionCacheScope reads the accepted request's private provider scope.
// A missing scope selects no affinity and permits no persisted cache entry.
func ExecutionCacheScope(ctx context.Context) (scope string, affinity, mayPersist bool) {
	binding, _ := ctx.Value(executionCacheScopeKey{}).(chatCacheBinding)
	return binding.scope, binding.affinity, binding.persist
}

// ExecutionCachePartition reads only the authenticated ingress's program
// partition. Missing evidence stays empty; display claims supply no partition.
func ExecutionCachePartition(ctx context.Context) string {
	binding, _ := ctx.Value(executionCacheScopeKey{}).(chatCacheBinding)
	return binding.partition
}

func chat3NoRetention(guarantees []wire.ChatGuarantee) bool {
	for _, guarantee := range guarantees {
		if guarantee == wire.ChatGuaranteeNoRetention {
			return true
		}
	}
	return false
}

// receivedChatCacheBinding takes scope only from the rechecked designated
// native runtime. Every other native caller derives from its actual subject.
// A designated runtime without this private extension supplies no app scope.
func receivedChatCacheBinding(ctx context.Context, subject Subject, values map[string]string, conversation string, noRetention bool) (chatCacheBinding, string) {
	if subject.Account == "" || subject.Program == "" {
		return chatCacheBinding{}, "caller"
	}
	scope, present := values[CacheScopeExtension]
	partition, partitionPresent := values[CachePartitionExtension]
	if present || partitionPresent {
		if !cacheScopeForwardTrusted(ctx) || !contentdigest.Valid(scope) {
			return chatCacheBinding{}, "extension:" + CacheScopeExtension
		}
		if partitionPresent && !contentdigest.Valid(partition) {
			return chatCacheBinding{}, "extension:" + CachePartitionExtension
		}
		return chatCacheBinding{scope: scope, partition: partition, affinity: conversation != "", persist: !noRetention}, ""
	}
	if designated, _ := ctx.Value(designatedRuntimeKey{}).(bool); designated {
		return chatCacheBinding{}, ""
	}
	scope, affinity := chatCacheScope(subject, conversation)
	partition, _ = chatCacheScope(subject, "")
	return chatCacheBinding{scope: scope, partition: partition, affinity: affinity, persist: scope != "" && !noRetention}, ""
}

func forwardedChatCacheExtensions(values map[string]string, binding chatCacheBinding) map[string]string {
	out := cloneStrings(values)
	delete(out, CacheScopeExtension)
	delete(out, CachePartitionExtension)
	if binding.scope != "" {
		if out == nil {
			out = make(map[string]string, 1)
		}
		out[CacheScopeExtension] = binding.scope
		if binding.partition != "" {
			out[CachePartitionExtension] = binding.partition
		}
	}
	return out
}

// chatCacheScope derives one provider-owned CONV-T3 scope. The caller label
// contributes correlation within its attested account/program partition.
// An absent label retains that partition and selects no chat slot affinity.
func chatCacheScope(subject Subject, conversation string) (string, bool) {
	if subject.Account == "" || subject.Program == "" {
		return "", false
	}
	encoded, err := json.Marshal([]string{subject.Account, subject.Program, conversation})
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(append([]byte("oa.inference.prompt-cache.v1\n"), encoded...))
	return contentdigest.Hex(digest[:]), conversation != ""
}
