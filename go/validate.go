package inference

import (
	"encoding/json"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func canonicalSHA256(digest string) bool {
	if len(digest) != 71 || !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	for _, c := range digest[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func imageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func nameChars(s string, extra string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(extra, c)) {
			return false
		}
	}
	return true
}

func jsonObject(text string) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal([]byte(text), &v) == nil
}

// validate returns the reason an argument is invalid, or "".
func validate(req wire.Request) string {
	if req.Model == "" || len(req.Model) > 256 {
		return "model"
	}
	if len(req.Messages) < 1 || len(req.Messages) > 512 {
		return "messages"
	}
	for _, m := range req.Messages {
		if !m.Role.Known() || len(m.Parts) < 1 || len(m.Parts) > 64 {
			return "messages"
		}
		for _, part := range m.Parts {
			switch part.Kind {
			case wire.PartKindText:
				if part.Digest != "" || part.MediaType != "" || part.CallID != "" || part.Name != "" || part.Arguments != "" {
					return "part:text"
				}
			case wire.PartKindImage:
				if m.Role != wire.RoleUser || !canonicalSHA256(part.Digest) || !imageMediaType(part.MediaType) || part.Text != "" || part.CallID != "" || part.Name != "" || part.Arguments != "" {
					return "part:image"
				}
			case wire.PartKindToolCall:
				if part.CallID == "" || part.Name == "" || part.Text != "" || part.Digest != "" || part.Arguments != "" && !jsonObject(part.Arguments) {
					return "part:tool_call"
				}
			case wire.PartKindToolResult:
				if part.CallID == "" || part.Name != "" || part.Digest != "" || part.Arguments != "" {
					return "part:tool_result"
				}
			default:
				return "part"
			}
		}
	}
	if len(req.Tools) > 128 {
		return "tools"
	}
	for _, t := range req.Tools {
		if t.Name == "" || len(t.Name) > 64 || !nameChars(t.Name, "_-") || !jsonObject(t.Parameters) {
			return "tools"
		}
	}
	if o := req.Options; o != nil {
		if o.MaxOutput < 0 || len(o.Stop) > 8 || o.Temperature != nil && (o.Temperature.Milli < 0 || o.Temperature.Milli > 2000) {
			return "options"
		}
		if o.JSONSchema != "" && !jsonObject(o.JSONSchema) {
			return "options"
		}
	}
	for key := range req.Extensions {
		owner, name, ok := strings.Cut(key, "/")
		if !ok || owner == "" || name == "" || len(key) > 128 {
			return "extensions"
		}
	}
	for _, key := range req.RequiredExtensions {
		if _, ok := req.Extensions[key]; !ok {
			return "extensions"
		}
	}
	local, hosted := false, false
	for _, g := range req.Guarantees {
		switch g {
		case wire.RequestGuaranteeLocalOnly:
			local = true
		case wire.RequestGuaranteeHostedAllowed:
			hosted = true
		default:
			return "guarantees"
		}
	}
	if local && hosted {
		return "guarantees"
	}
	if c := req.Credential; c != "" && (len(c) > 64 || !nameChars(c, "_-.")) {
		return "credential"
	}
	return ""
}
