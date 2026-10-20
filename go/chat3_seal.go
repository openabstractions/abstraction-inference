package inference

import (
	"encoding/json"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const chat3SealBytes = 512 << 10

// chat3SealBuilder bounds the whole replay material while its provider emits
// fragments. Public text has its independent event path. Once omitted, no
// prefix is retained or exposed as a seal (Architect Codex 260).
type chat3SealBuilder struct {
	origin  string
	fields  map[string][]byte
	size    int
	omitted *wire.SealOmission
}

func (s *chat3SealBuilder) omit(reason wire.SealOmission) {
	s.fields, s.size, s.omitted = nil, 0, &reason
}

func (s *chat3SealBuilder) append(origin, field string, b []byte) {
	if s.omitted != nil {
		return
	}
	if len(origin) > 256 {
		s.omit(wire.SealOmissionEncodedSize)
		return
	}
	valid := origin != "" && utf8.ValidString(origin)
	for _, c := range origin {
		valid = valid && c > 31 && (c < 127 || c > 159)
	}
	if !valid || s.origin != "" && s.origin != origin {
		s.omit(wire.SealOmissionIncompatibleOrigin)
		return
	}
	s.origin = origin
	allowed := origin == router.WireOpenAICompatible && field == "reasoning_content" ||
		origin == router.WireAnthropicMessages && (field == "thinking" || field == "signature" || field == "data")
	if !allowed {
		s.omit(wire.SealOmissionIncompatibleOrigin)
		return
	}
	if len(b) > chat3SealBytes-s.size {
		s.omit(wire.SealOmissionEncodedSize)
		return
	}
	if s.fields == nil {
		s.fields = map[string][]byte{}
	}
	s.fields[field] = append(s.fields[field], b...)
	s.size += len(b)
}

func (s *chat3SealBuilder) finish() (*wire.ReasoningSeal, *wire.SealOmission) {
	if s.omitted != nil || s.fields == nil {
		return nil, s.omitted
	}
	var b []byte
	switch s.origin {
	case router.WireOpenAICompatible:
		b = s.fields["reasoning_content"]
	case router.WireAnthropicMessages:
		var block map[string]string
		if data, present := s.fields["data"]; present {
			block = map[string]string{"type": "redacted_thinking", "data": string(data)}
		} else if signature, present := s.fields["signature"]; present {
			block = map[string]string{"type": "thinking", "thinking": string(s.fields["thinking"]), "signature": string(signature)}
		} else {
			// A public thinking block with no replay signature provides no seal.
			return nil, nil
		}
		// String values always encode; the whole wrapper belongs to this wire
		// adapter, and remains opaque in clients and relays.
		//unchecked: a map of string keys and values cannot fail JSON encoding
		b, _ = json.Marshal(block)
	}
	if len(b) > chat3SealBytes {
		s.omit(wire.SealOmissionEncodedSize)
		return nil, s.omitted
	}
	return &wire.ReasoningSeal{Origin: s.origin, Bytes: append([]byte{}, b...)}, nil
}
