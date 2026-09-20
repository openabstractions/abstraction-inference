package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	router "github.com/openabstractions/abstraction-router/go"
)

type replicateJobBackend struct {
	client            *http.Client
	maxEach, maxTotal int64
}

func (b replicateJobBackend) Supports(host *router.Host, request jobwire.Request) bool {
	return host != nil && host.Wire == router.WireReplicatePredictions && request.Image.Mode == jobwire.ModeGenerate
}

// Replicate has no documented caller idempotency key or lookup by caller key.
// A restart after submit began without a recorded prediction id therefore stays
// uncertain and is never replayed.
func (replicateJobBackend) Reconcile(context.Context, *router.Host, string, jobwire.Request, map[string]string) (durableJobStatus, error) {
	return durableJobStatus{State: durableJobUnknown}, nil
}

func (b replicateJobBackend) Submit(ctx context.Context, host *router.Host, _ string, model string, request jobwire.Request, headers map[string]string) (durableJobStatus, error) {
	parts := strings.Split(model, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return durableJobStatus{}, errors.New("invalid replicate model")
	}
	width, height, _ := imageDimensions(request.Image.Size)
	input := map[string]any{"prompt": request.Image.Prompt, "num_outputs": request.Image.Count}
	if width > 0 {
		input["width"], input["height"] = width, height
	}
	if request.Profile == jobwire.ProfileVideo {
		input["duration"] = float64(request.DurationMs) / 1000
	}
	body := map[string]any{"input": input}
	if version := request.Image.Extensions["replicate/version"]; version != "" {
		body["version"] = version
	}
	address := strings.TrimRight(host.Base, "/") + "/v1/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/predictions"
	response, failed := jsonImageRequest(ctx, b.client, http.MethodPost, address, body, headers)
	if response == nil {
		return durableJobStatus{}, errors.New(failed.reason)
	}
	defer response.Body.Close()
	var prediction replicatePrediction
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&prediction) != nil || prediction.ID == "" {
		return durableJobStatus{}, errors.New("invalid upstream response")
	}
	return b.status(ctx, prediction, request)
}

func (b replicateJobBackend) Poll(ctx context.Context, host *router.Host, handle string, request jobwire.Request, headers map[string]string) (durableJobStatus, error) {
	response, failed := imageRequest(ctx, b.client, http.MethodGet, strings.TrimRight(host.Base, "/")+"/v1/predictions/"+url.PathEscape(handle), "", nil, headers)
	if response == nil {
		return durableJobStatus{}, errors.New(failed.reason)
	}
	defer response.Body.Close()
	var prediction replicatePrediction
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&prediction) != nil || prediction.ID != handle {
		return durableJobStatus{}, errors.New("invalid upstream response")
	}
	return b.status(ctx, prediction, request)
}

func (b replicateJobBackend) status(ctx context.Context, prediction replicatePrediction, request jobwire.Request) (durableJobStatus, error) {
	result := durableJobStatus{Handle: prediction.ID}
	switch prediction.Status {
	case "starting", "processing":
		result.State = durableJobRunning
	case "failed":
		result.State, result.Reason = durableJobFailed, "upstream:failed"
	case "canceled":
		result.State = durableJobCancelled
	case "succeeded":
		result.State = durableJobSucceeded
		if int64(len(prediction.Output)) != request.Image.Count {
			return durableJobStatus{}, errors.New("invalid upstream output count")
		}
		remaining := b.maxTotal
		if remaining <= 0 {
			remaining = 64 << 20
		}
		for _, raw := range prediction.Output {
			limit := remaining
			if request.Profile == jobwire.ProfileImageBatch {
				limit = min(b.maxEach, remaining)
			}
			output, ok := fetchGeneratedImage(ctx, b.client, raw, "replicate", limit)
			if !ok {
				return durableJobStatus{}, errors.New("invalid upstream output")
			}
			if request.Profile == jobwire.ProfileVideo && output.mediaType != "video/mp4" || request.Profile == jobwire.ProfileImageBatch && !generatedInputMedia(output.mediaType) {
				return durableJobStatus{}, errors.New("invalid upstream output media")
			}
			result.Outputs = append(result.Outputs, durableJobOutput{Data: output.data, MediaType: output.mediaType})
			remaining -= int64(len(output.data))
		}
		if len(result.Outputs) == 0 {
			return durableJobStatus{}, errors.New("missing upstream output")
		}
	default:
		return durableJobStatus{}, errors.New("invalid upstream status")
	}
	return result, nil
}

func (b replicateJobBackend) Cancel(ctx context.Context, host *router.Host, handle string, headers map[string]string) error {
	response, failed := jsonImageRequest(ctx, b.client, http.MethodPost, strings.TrimRight(host.Base, "/")+"/v1/predictions/"+url.PathEscape(handle)+"/cancel", map[string]any{}, headers)
	if response == nil {
		return errors.New(failed.reason)
	}
	response.Body.Close()
	return nil
}
