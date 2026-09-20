package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

// embeddingsInputs reads input as one string or a list of strings. Token
// arrays are refused: the contract carries texts.
func embeddingsInputs(raw json.RawMessage) ([]string, error) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many, nil
	}
	return nil, errors.New("input must be a string or a list of strings")
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
	reply := embedder.Embed(inference.WithRoute(r.Context(), inference.RouteWindow, b.Rung()), b.Subject(), req)
	if reply.Outcome != wire.EmbedOutcomeCompleted {
		writeError(rw, status(reply.Outcome.String()), reply.Outcome.String(), refusalMessage(reply.Outcome.String(), reply.Reason))
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
	json.NewEncoder(rw).Encode(map[string]any{"object": "list", "data": data, "model": reply.Model,
		"usage": map[string]any{"prompt_tokens": reply.Usage.Input, "total_tokens": reply.Usage.Input}})
}
