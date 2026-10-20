package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/client"
)

// Embedder is the provider the window sends embeddings requests to.
// *inference.Provider satisfies it; a Config.Chat without it serves no
// /v1/embeddings route.
type Embedder interface {
	Embed(ctx context.Context, subject inference.Subject, req wire.EmbedRequest) wire.Embeddings
}

// oaiEmbeddingsRequest is an OpenAI embeddings request as the window reads it.
type oaiEmbeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     int64           `json:"dimensions"`
	EncodingFormat string          `json:"encoding_format"`
	User           string          `json:"user"`
}

// MaxEmbeddingsInputs bounds the inputs of one /v1/embeddings request, the
// limit OpenAI documents. The window sends them in several embed@1 calls.
const MaxEmbeddingsInputs = 2048

// embeddingsInputs reads input as one string or a list of strings. Token
// arrays are refused: the contract carries texts.
func embeddingsInputs(raw json.RawMessage) ([]string, error) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		if len(many) > MaxEmbeddingsInputs {
			return nil, fmt.Errorf("input holds %d texts; send at most %d per request", len(many), MaxEmbeddingsInputs)
		}
		return many, nil
	}
	var tokens []json.RawMessage
	if json.Unmarshal(raw, &tokens) == nil {
		return nil, errors.New("input must be text; token arrays are not accepted (in LangChain set check_embedding_ctx_length=False)")
	}
	return nil, errors.New("input must be a string or a list of strings")
}

// embedBatch is the number of inputs the next embed@1 call carries: at most
// MaxEmbedInputs, and few enough that dims-wide vectors stay within
// MaxEmbedValues. An unknown width counts as the widest the contract allows.
func embedBatch(dims int64) int {
	if dims <= 0 || dims > inference.MaxEmbedDimensions {
		dims = inference.MaxEmbedDimensions
	}
	return int(max(1, min(int64(inference.MaxEmbedInputs), inference.MaxEmbedValues/dims)))
}

// embedInBatches sends inputs as consecutive embed@1 calls sized by
// embedBatch and joins the vectors in input order. The first call that does
// not complete answers for the whole request.
func embedInBatches(ctx context.Context, embedder Embedder, subject inference.Subject, req wire.EmbedRequest) wire.Embeddings {
	inputs := req.Inputs
	var joined wire.Embeddings
	dims := req.Dimensions
	for start := 0; start < len(inputs); {
		end := min(len(inputs), start+embedBatch(dims))
		part := req
		part.Inputs = inputs[start:end]
		reply := embedder.Embed(ctx, subject, part)
		if reply.Outcome != wire.EmbedOutcomeCompleted {
			return reply
		}
		if start == 0 {
			joined = reply
			joined.Vectors = append(make([]string, 0, len(inputs)), reply.Vectors...)
			dims = reply.Dimensions
		} else {
			if reply.Dimensions != joined.Dimensions || reply.Model != joined.Model {
				return wire.Embeddings{Outcome: wire.EmbedOutcomeUnavailable, Reason: "upstream:malformed"}
			}
			joined.Vectors = append(joined.Vectors, reply.Vectors...)
			joined.Usage.Input += reply.Usage.Input
			joined.Usage.Output += reply.Usage.Output
			if reply.Cost != nil {
				if joined.Cost == nil {
					joined.Cost = reply.Cost
				} else {
					joined.Cost = &wire.Cost{Micros: joined.Cost.Micros + reply.Cost.Micros, Currency: joined.Cost.Currency}
				}
			}
		}
		start = end
	}
	return joined
}

// openAIEmbeddings translates POST /v1/embeddings to embed@1 and the reply
// back, as number arrays or as base64 float32 when encoding_format asks.
func (w *Window) openAIEmbeddings(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	embedder, _ := w.cfg.Chat.(Embedder)
	var in oaiEmbeddingsRequest
	if !w.readBody(rw, r, b, &in) {
		return
	}
	inputs, err := embeddingsInputs(in.Input)
	if err == nil && in.EncodingFormat != "" && in.EncodingFormat != "float" && in.EncodingFormat != "base64" {
		err = errors.New("encoding_format must be float or base64")
	}
	if err != nil {
		w.badRequest(rw, b, in.Model, err)
		return
	}
	req := wire.EmbedRequest{Model: in.Model, Inputs: inputs, Dimensions: in.Dimensions}
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	var reply wire.Embeddings
	reason, bound := w.holdForQuestions(r, routed(r.Context(), b), b, in.Model, "embed", func(ctx context.Context) (bool, string) {
		reply = embedInBatches(ctx, embedder, b.Subject(), req)
		return reply.Outcome == wire.EmbedOutcomeNotPermitted || reply.Outcome == wire.EmbedOutcomeForbidden, reply.Reason
	})
	if !bound {
		reply = wire.Embeddings{Outcome: wire.EmbedOutcomeForbidden}
	}
	reply.Reason = reason
	if reply.Outcome != wire.EmbedOutcomeCompleted {
		refusalOf(reply.Outcome.String(), reply.Reason).write(rw)
		return
	}
	data := make([]map[string]any, len(reply.Vectors))
	if in.EncodingFormat == "base64" {
		for i, v := range reply.Vectors {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
		}
	} else {
		vectors, err := client.Vectors(reply)
		if err != nil {
			writeError(rw, http.StatusBadGateway, "unavailable", "the provider returned a malformed vector")
			return
		}
		for i, v := range vectors {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
		}
	}
	rw.Header().Set("Content-Type", "application/json")
	//unchecked: terminal write of the response; headers and status are already committed and this package has no logger to report a write failure to (matches writeError and the other Encode calls in this package)
	json.NewEncoder(rw).Encode(map[string]any{"object": "list", "data": data, "model": reply.Model,
		"usage": map[string]any{"prompt_tokens": reply.Usage.Input, "total_tokens": reply.Usage.Input}})
}
