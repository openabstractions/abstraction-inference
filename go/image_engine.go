package inference

import (
	"context"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// ImageInput contains content the inference service resolved and verified
// under the bound caller's authority before invoking an image engine.
type ImageInput struct {
	Data      []byte
	MediaType string
}

// ImageOutput contains one complete result. The inference service checks its
// count, media type, detected type and byte limits before publication.
type ImageOutput struct {
	Data      []byte
	MediaType string
}

// ImageEngineReply is the engine's result before caller-authorized content
// publication. Failed outcomes must not carry output images.
type ImageEngineReply struct {
	Outcome wire.ReplyOutcome
	Reason  string
	Images  []ImageOutput
	Usage   wire.Usage
	Cost    *wire.Cost
}

// ImageEngine is an explicitly composed local provider implementation.
// Unsupported runs before rights/content/credential preflight and must make
// no engine effect. Run starts only after those checks pass; ctx cancellation
// asks the adapter to stop its work. The service owns operation lifetime and
// output delivery, while the adapter owns its engine protocol and process.
type ImageEngine interface {
	Unsupported(req wire.ImageRequest) string
	Run(ctx context.Context, model string, req wire.ImageRequest, inputs map[string]ImageInput, progress func(percent int64, message string)) ImageEngineReply
}

func validImageAccounting(reason string, usage wire.Usage, cost *wire.Cost) bool {
	return len(reason) <= 4096 && utf8.ValidString(reason) &&
		usage.Input >= 0 && usage.Output >= 0 && usage.Cached >= 0 && usage.Cached <= usage.Input &&
		usage.Input <= 1<<53 && usage.Output <= 1<<53 &&
		(cost == nil || cost.Micros >= 0 && cost.Micros <= 1<<53 && len(cost.Currency) >= 1 && len(cost.Currency) <= 64 && utf8.ValidString(cost.Currency))
}

func (p *Provider) runImageEngine(ctx context.Context, host *router.Host, model string, req wire.ImageRequest, inputs map[string]resolvedImageInput, progress func(int64, string)) imageBackendReply {
	if p.cfg.ImageEngine == nil || p.cfg.ImageEngineHost != host {
		return imageBackendReply{outcome: wire.ReplyOutcomeUnavailable, reason: "engine:host"}
	}
	resolved := make(map[string]ImageInput, len(inputs))
	for digest, input := range inputs {
		resolved[digest] = ImageInput{Data: append([]byte(nil), input.data...), MediaType: input.mediaType}
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Credential = ""
	forwarded.Extensions = cloneStrings(req.Extensions)
	result := p.cfg.ImageEngine.Run(ctx, model, forwarded, resolved, progress)
	if ctx.Err() != nil {
		return imageBackendReply{outcome: wire.ReplyOutcomeCancelled, reason: "caller"}
	}
	if !result.Outcome.Known() || result.Outcome != wire.ReplyOutcomeCompleted && len(result.Images) != 0 ||
		!validImageAccounting(result.Reason, result.Usage, result.Cost) ||
		len(result.Images) > 10 || int64(len(result.Images)) > req.Count {
		return imageBackendReply{outcome: wire.ReplyOutcomeUnavailable, reason: "engine:invalid_reply"}
	}
	var total int64
	for _, output := range result.Images {
		size := int64(len(output.Data))
		if size == 0 || size > p.cfg.MaxGeneratedImageBytes || size > p.cfg.MaxImageOutputBytes-total || !generatedInputMedia(output.MediaType) {
			return imageBackendReply{outcome: wire.ReplyOutcomeUnavailable, reason: "engine:invalid_reply"}
		}
		total += size
	}
	outputs := make([]generatedImage, len(result.Images))
	for i, output := range result.Images {
		outputs[i] = generatedImage{data: append([]byte(nil), output.Data...), mediaType: output.MediaType}
	}
	var cost *wire.Cost
	if result.Cost != nil {
		copied := *result.Cost
		cost = &copied
	}
	return imageBackendReply{outcome: result.Outcome, reason: result.Reason, images: outputs, usage: result.Usage, cost: cost}
}
