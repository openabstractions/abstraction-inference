package inference

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3SealWholeBoundAndOneByteOverOmission(t *testing.T) {
	var s chat3SealBuilder
	b := bytes.Repeat([]byte("r"), chat3SealBytes)
	s.append(router.WireOpenAICompatible, "reasoning_content", b)
	seal, omitted := s.finish()
	if seal == nil || omitted != nil || !bytes.Equal(seal.Bytes, b) {
		t.Fatal("exact-bound seal lost bytes")
	}
	s.append(router.WireOpenAICompatible, "reasoning_content", []byte("x"))
	seal, omitted = s.finish()
	if seal != nil || omitted == nil || *omitted != wire.SealOmissionEncodedSize || s.fields != nil {
		t.Fatal("over-bound seal was retained or truncated")
	}
}

func TestChat3SealAnthropicReplayPreservesThinkingAndSignature(t *testing.T) {
	var s chat3SealBuilder
	s.append(router.WireAnthropicMessages, "thinking", []byte("first\n"))
	s.append(router.WireAnthropicMessages, "thinking", []byte("second"))
	s.append(router.WireAnthropicMessages, "signature", []byte("opaque+/="))
	seal, omitted := s.finish()
	if seal == nil || omitted != nil {
		t.Fatal("signed thinking seal missing")
	}
	var block map[string]string
	if err := json.Unmarshal(seal.Bytes, &block); err != nil {
		t.Fatal(err)
	}
	if block["type"] != "thinking" || block["thinking"] != "first\nsecond" || block["signature"] != "opaque+/=" {
		t.Fatalf("replay material changed: %v", block)
	}
}

func TestChat3SealEncodedWrapperAndOriginAreWholeOmissions(t *testing.T) {
	var s chat3SealBuilder
	s.append(router.WireAnthropicMessages, "thinking", bytes.Repeat([]byte("\n"), chat3SealBytes/2))
	s.append(router.WireAnthropicMessages, "signature", []byte("signature"))
	seal, omitted := s.finish()
	if seal != nil || omitted == nil || *omitted != wire.SealOmissionEncodedSize {
		t.Fatal("encoded wrapper ceiling ignored")
	}
	var origin chat3SealBuilder
	origin.append(strings.Repeat("x", 257), "reasoning_content", []byte("not exposed"))
	seal, omitted = origin.finish()
	if seal != nil || omitted == nil || *omitted != wire.SealOmissionEncodedSize {
		t.Fatal("oversized origin did not omit the whole seal")
	}
	var mixed chat3SealBuilder
	mixed.append(router.WireOpenAICompatible, "reasoning_content", []byte("a"))
	mixed.append(router.WireAnthropicMessages, "signature", []byte("b"))
	seal, omitted = mixed.finish()
	if seal != nil || omitted == nil || *omitted != wire.SealOmissionIncompatibleOrigin {
		t.Fatal("incompatible origins were combined")
	}
}
