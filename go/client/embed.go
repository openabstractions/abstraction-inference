package client

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// EmbedRequest is an abstraction.inference/embed@1 request.
type EmbedRequest = wire.EmbedRequest

// EmbedResult is an embed@1 reply; Vectors decodes its vectors.
type EmbedResult = wire.Embeddings

// Embeddings calls abstraction.inference/embed@1.
type Embeddings struct{ transport listen.FrameClient }

// NewEmbeddings binds an explicit endpoint.
func NewEmbeddings(endpoint string) *Embeddings {
	return NewEmbeddingsWithTransport(listen.FrameClient{Endpoint: endpoint})
}

// NewEmbeddingsWithTransport retains the caller's endpoint, server trust and
// limits. A call waits for the host, so its budget is the caller's context.
func NewEmbeddingsWithTransport(transport listen.FrameClient) *Embeddings {
	return &Embeddings{transport: transport.WithDefaults(callMargin, 1<<20)}
}

// Embed sends one request and returns the reply. It is not retried.
func (e *Embeddings) Embed(ctx context.Context, request EmbedRequest) (EmbedResult, error) {
	if err := ctx.Err(); err != nil {
		return EmbedResult{}, err
	}
	r, err := wire.NewEmbedderClient(e.transport.WithContext(ctx)).Embed(request)
	if err == nil {
		completed := r.Outcome == wire.EmbedOutcomeCompleted
		err = check(completed == (r.Dimensions > 0) && (completed || len(r.Vectors) == 0) && (!completed || len(r.Vectors) == len(request.Inputs)))
	}
	return r, err
}

// Vectors decodes a completed reply's vectors into float32 values.
func Vectors(r EmbedResult) ([][]float32, error) {
	out := make([][]float32, len(r.Vectors))
	for i, v := range r.Vectors {
		packed, err := base64.StdEncoding.DecodeString(v)
		if err != nil || int64(len(packed)) != 4*r.Dimensions {
			return nil, ErrInconsistent
		}
		values := make([]float32, r.Dimensions)
		for k := range values {
			values[k] = math.Float32frombits(binary.LittleEndian.Uint32(packed[4*k:]))
		}
		out[i] = values
	}
	return out, nil
}
