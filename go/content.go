package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// resolveImages resolves every distinct digest once. Request parts remain
// references; only the private adapter input carries bytes.
func (p *Provider) resolveImages(ctx context.Context, subject Subject, req wire.Request) (map[string][]byte, wire.StartOutcome, string) {
	var images map[string][]byte
	var total int64
	for _, message := range req.Messages {
		for _, part := range message.Parts {
			if part.Kind != wire.PartKindImage {
				continue
			}
			if data, ok := images[part.Digest]; ok {
				if http.DetectContentType(data) != part.MediaType {
					return nil, wire.StartOutcomeInvalid, "content:media-type"
				}
				if int64(len(data)) > p.cfg.MaxVisionBytes-total {
					return nil, wire.StartOutcomeInvalid, "content:too-large"
				}
				total += int64(len(data))
				continue
			}
			if p.cfg.ResolveContent == nil {
				return nil, wire.StartOutcomeUnavailable, "content:unavailable"
			}
			remaining := p.cfg.MaxVisionBytes - total
			if remaining <= 0 {
				return nil, wire.StartOutcomeInvalid, "content:too-large"
			}
			limit := min(p.cfg.MaxImageBytes, remaining)
			data, outcome := p.cfg.ResolveContent(ctx, subject, part.Digest, limit)
			if ctx.Err() != nil {
				return nil, wire.StartOutcomeUnavailable, "content:unavailable"
			}
			switch outcome {
			case ContentResolved:
				if int64(len(data)) > limit {
					return nil, wire.StartOutcomeInvalid, "content:too-large"
				}
				sum := sha256.Sum256(data)
				if "sha256:"+hex.EncodeToString(sum[:]) != part.Digest {
					return nil, wire.StartOutcomeInvalid, "content:digest"
				}
				if http.DetectContentType(data) != part.MediaType {
					return nil, wire.StartOutcomeInvalid, "content:media-type"
				}
				if images == nil {
					images = map[string][]byte{}
				}
				images[part.Digest] = append([]byte(nil), data...)
				total += int64(len(data))
			case ContentUnknown:
				return nil, wire.StartOutcomeInvalid, "content:unknown"
			case ContentForbidden:
				return nil, wire.StartOutcomeForbidden, "content:read:forbidden"
			case ContentTooLarge:
				return nil, wire.StartOutcomeInvalid, "content:too-large"
			case ContentUnavailable:
				return nil, wire.StartOutcomeUnavailable, "content:unavailable"
			default:
				return nil, wire.StartOutcomeUnavailable, "content:unavailable"
			}
		}
	}
	return images, 0, ""
}
