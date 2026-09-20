package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const upstreamImageReadLimit = 80 << 20

const (
	minimumImagePollDelay = 25 * time.Millisecond
	maximumImagePollTime  = 30 * time.Minute
)

type imageOutputBounds struct {
	each  int64
	total int64
}

func runImageBackend(ctx context.Context, client *http.Client, host *router.Host, model, kind string, req wire.ImageRequest, inputs map[string]resolvedImageInput, headers map[string]string, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	switch kind {
	case "openai":
		return runOpenAIImage(ctx, client, host, model, req, inputs, headers)
	case "stability":
		return runStabilityImage(ctx, client, host, model, req, inputs, headers)
	case "replicate":
		return runReplicateImage(ctx, client, host, model, req, headers, bounds, progress)
	case "fal":
		return runFalImage(ctx, client, host, model, req, headers, bounds, progress)
	case "comfyui":
		return runComfyImage(ctx, client, host, model, req, bounds, progress)
	case "swarmui":
		return runSwarmImage(ctx, client, host, model, req, bounds)
	default:
		return imageBackendReply{outcome: wire.ReplyOutcomeUnsupportedFeature, reason: "wire:" + host.Wire}
	}
}

func imageBackendFailure(outcome wire.ReplyOutcome, reason string) imageBackendReply {
	return imageBackendReply{outcome: outcome, reason: reason}
}

func imageHTTPOutcome(status int) imageBackendReply {
	switch {
	case status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity:
		return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, fmt.Sprintf("upstream:%d", status))
	case status == http.StatusTooManyRequests || status >= 500:
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, fmt.Sprintf("upstream:%d", status))
	default:
		return imageBackendFailure(wire.ReplyOutcomeRefused, fmt.Sprintf("upstream:%d", status))
	}
}

func imageRequest(ctx context.Context, client *http.Client, method, address, contentType string, body io.Reader, headers map[string]string) (*http.Response, imageBackendReply) {
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:address")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, imageHTTPOutcome(resp.StatusCode)
	}
	return resp, imageBackendReply{}
}

func jsonImageRequest(ctx context.Context, client *http.Client, method, address string, body any, headers map[string]string) (*http.Response, imageBackendReply) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, imageBackendFailure(wire.ReplyOutcomeInvalid, "upstream:encoding")
	}
	return imageRequest(ctx, client, method, address, "application/json", bytes.NewReader(raw), headers)
}

func multipartImageBody(fields map[string]string, files map[string]resolvedImageInput) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := writer.WriteField(key, fields[key]); err != nil {
			return nil, "", err
		}
	}
	keys = keys[:0]
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		input := files[key]
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[input.mediaType]
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, key, key+ext))
		header.Set("Content-Type", input.mediaType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(input.data); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}

func decodeBase64Images(resp *http.Response) imageBackendReply {
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			Base64 string `json:"b64_json"`
		} `json:"data"`
		Usage wire.Usage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, upstreamImageReadLimit)).Decode(&payload); err != nil || len(payload.Data) == 0 {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	result := imageBackendReply{outcome: wire.ReplyOutcomeCompleted, images: make([]generatedImage, 0, len(payload.Data)), usage: payload.Usage}
	for _, item := range payload.Data {
		data, err := base64.StdEncoding.DecodeString(item.Base64)
		if err != nil || len(data) == 0 {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
		}
		result.images = append(result.images, generatedImage{data: data, mediaType: http.DetectContentType(data)})
	}
	return result
}

func runOpenAIImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, inputs map[string]resolvedImageInput, headers map[string]string) imageBackendReply {
	address := host.ImageURL()
	if req.Mode == wire.ImageModeGenerate {
		body := map[string]any{"model": model, "prompt": req.Prompt, "n": req.Count, "size": req.Size, "response_format": "b64_json"}
		resp, failed := jsonImageRequest(ctx, client, http.MethodPost, address+"/generations", body, headers)
		if resp == nil {
			return failed
		}
		return decodeBase64Images(resp)
	}
	fields := map[string]string{"model": model, "prompt": req.Prompt, "n": strconv.FormatInt(req.Count, 10), "size": req.Size, "response_format": "b64_json"}
	files := map[string]resolvedImageInput{"image": inputs[req.ImageDigest]}
	if req.MaskDigest != "" {
		files["mask"] = inputs[req.MaskDigest]
	}
	body, contentType, err := multipartImageBody(fields, files)
	if err != nil {
		return imageBackendFailure(wire.ReplyOutcomeInvalid, "upstream:encoding")
	}
	resp, failed := imageRequest(ctx, client, http.MethodPost, address+"/edits", contentType, body, headers)
	if resp == nil {
		return failed
	}
	return decodeBase64Images(resp)
}

func runStabilityImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, inputs map[string]resolvedImageInput, headers map[string]string) imageBackendReply {
	endpoint := host.Base + "/v2beta/stable-image/generate/" + url.PathEscape(model)
	fields := map[string]string{"prompt": req.Prompt, "output_format": "png"}
	files := map[string]resolvedImageInput{}
	if req.Mode == wire.ImageModeEdit {
		endpoint = host.Base + "/v2beta/stable-image/edit/inpaint"
		files["image"] = inputs[req.ImageDigest]
		files["mask"] = inputs[req.MaskDigest]
	}
	body, contentType, err := multipartImageBody(fields, files)
	if err != nil {
		return imageBackendFailure(wire.ReplyOutcomeInvalid, "upstream:encoding")
	}
	with := cloneStrings(headers)
	with["Accept"] = "image/*"
	resp, failed := imageRequest(ctx, client, http.MethodPost, endpoint, contentType, body, with)
	clear(with)
	if resp == nil {
		return failed
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, upstreamImageReadLimit))
	if err != nil || len(data) == 0 {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
	}
	return imageBackendReply{outcome: wire.ReplyOutcomeCompleted, images: []generatedImage{{data: data, mediaType: http.DetectContentType(data)}}}
}

func retryDelay(resp *http.Response) time.Duration {
	if resp != nil {
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 && seconds <= 60 {
			delay := time.Duration(seconds) * time.Second
			if delay < minimumImagePollDelay {
				return minimumImagePollDelay
			}
			return delay
		}
	}
	return 100 * time.Millisecond
}

func imagePollContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, maximumImagePollTime)
}

func waitImagePoll(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func safeOutputURL(raw, kind string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	switch kind {
	case "replicate":
		return u, host == "replicate.delivery" || strings.HasSuffix(host, ".replicate.delivery")
	case "fal":
		return u, host == "fal.media" || strings.HasSuffix(host, ".fal.media")
	}
	return nil, false
}

func outputFetchClient(client *http.Client, kind string) *http.Client {
	clone := *client
	previous := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if _, ok := safeOutputURL(req.URL.String(), kind); !ok {
			return http.ErrUseLastResponse
		}
		if previous != nil {
			return previous(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}

func readBoundedImage(body io.Reader, limit int64) ([]byte, bool) {
	if limit < 1 {
		return nil, false
	}
	readLimit := limit + 1
	if readLimit < limit {
		readLimit = limit
	}
	data, err := io.ReadAll(io.LimitReader(body, readLimit))
	return data, err == nil && len(data) > 0 && int64(len(data)) <= limit
}

func fetchGeneratedImage(ctx context.Context, client *http.Client, raw, kind string, limit int64) (generatedImage, bool) {
	u, ok := safeOutputURL(raw, kind)
	if !ok {
		return generatedImage{}, false
	}
	resp, failed := imageRequest(ctx, outputFetchClient(client, kind), http.MethodGet, u.String(), "", nil, nil)
	if resp == nil || failed.outcome != 0 {
		return generatedImage{}, false
	}
	defer resp.Body.Close()
	data, ok := readBoundedImage(resp.Body, min(limit, int64(upstreamImageReadLimit)))
	if !ok {
		return generatedImage{}, false
	}
	return generatedImage{data: data, mediaType: http.DetectContentType(data)}, true
}

type replicatePrediction struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Output []string `json:"output"`
	Error  any      `json:"error"`
}

func runReplicateImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, headers map[string]string, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	parts := strings.Split(model, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, "model")
	}
	width, height, _ := imageDimensions(req.Size)
	input := map[string]any{"prompt": req.Prompt, "num_outputs": req.Count}
	if width > 0 {
		input["width"], input["height"] = width, height
	}
	body := map[string]any{"input": input}
	if version := req.Extensions["replicate/version"]; version != "" {
		body["version"] = version
	}
	address := strings.TrimRight(host.Base, "/") + "/v1/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/predictions"
	with := cloneStrings(headers)
	if with == nil {
		with = map[string]string{}
	}
	with["Prefer"] = "wait=1"
	resp, failed := jsonImageRequest(ctx, client, http.MethodPost, address, body, with)
	clear(with)
	if resp == nil {
		return failed
	}
	var prediction replicatePrediction
	delay := retryDelay(resp)
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&prediction) != nil {
		resp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	resp.Body.Close()
	if prediction.ID == "" {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	callerCtx := ctx
	ctx, stopPolling := imagePollContext(ctx)
	defer stopPolling()
	terminal := false
	defer func() {
		if !terminal {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, _ := jsonImageRequest(cancelCtx, client, http.MethodPost, strings.TrimRight(host.Base, "/")+"/v1/predictions/"+url.PathEscape(prediction.ID)+"/cancel", map[string]any{}, headers)
			if response != nil {
				response.Body.Close()
			}
		}
	}()
	for prediction.Status != "succeeded" {
		switch prediction.Status {
		case "failed", "canceled":
			terminal = true
			return imageBackendFailure(wire.ReplyOutcomeRefused, "upstream:"+prediction.Status)
		case "starting", "processing":
		default:
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:status")
		}
		progress(0, prediction.Status)
		if !waitImagePoll(ctx, delay) {
			if callerCtx.Err() != nil {
				return imageBackendFailure(wire.ReplyOutcomeCancelled, "caller")
			}
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:timeout")
		}
		poll, failed := imageRequest(ctx, client, http.MethodGet, strings.TrimRight(host.Base, "/")+"/v1/predictions/"+url.PathEscape(prediction.ID), "", nil, headers)
		if poll == nil {
			return failed
		}
		delay = retryDelay(poll)
		if json.NewDecoder(io.LimitReader(poll.Body, 1<<20)).Decode(&prediction) != nil {
			poll.Body.Close()
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
		}
		poll.Body.Close()
	}
	terminal = true
	return fetchQueuedImages(ctx, client, prediction.Output, req.Count, "replicate", bounds)
}

type falQueueReply struct {
	RequestID   string `json:"request_id"`
	Status      string `json:"status"`
	ResponseURL string `json:"response_url"`
	StatusURL   string `json:"status_url"`
	CancelURL   string `json:"cancel_url"`
	Images      []struct {
		URL string `json:"url"`
	} `json:"images"`
}

func runFalImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, headers map[string]string, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	parts := strings.Split(model, "/")
	if len(parts) < 2 {
		return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, "model")
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, "model")
		}
		parts[i] = url.PathEscape(part)
	}
	base := strings.TrimRight(host.Base, "/") + "/" + strings.Join(parts, "/")
	width, height, _ := imageDimensions(req.Size)
	input := map[string]any{"prompt": req.Prompt, "num_images": req.Count}
	if width > 0 {
		input["image_size"] = map[string]int{"width": width, "height": height}
	}
	resp, failed := jsonImageRequest(ctx, client, http.MethodPost, base, input, headers)
	if resp == nil {
		return failed
	}
	var state falQueueReply
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&state) != nil || state.RequestID == "" {
		resp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	delay := retryDelay(resp)
	resp.Body.Close()
	statusURL := base + "/requests/" + url.PathEscape(state.RequestID) + "/status"
	responseURL := base + "/requests/" + url.PathEscape(state.RequestID)
	cancelURL := base + "/requests/" + url.PathEscape(state.RequestID) + "/cancel"
	callerCtx := ctx
	ctx, stopPolling := imagePollContext(ctx)
	defer stopPolling()
	terminal := false
	defer func() {
		if !terminal {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			response, _ := jsonImageRequest(cancelCtx, client, http.MethodPut, cancelURL, map[string]any{}, headers)
			if response != nil {
				response.Body.Close()
			}
		}
	}()
	for {
		progress(0, strings.ToLower(state.Status))
		if !waitImagePoll(ctx, delay) {
			if callerCtx.Err() != nil {
				return imageBackendFailure(wire.ReplyOutcomeCancelled, "caller")
			}
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:timeout")
		}
		poll, failed := imageRequest(ctx, client, http.MethodGet, statusURL, "", nil, headers)
		if poll == nil {
			return failed
		}
		delay = retryDelay(poll)
		if json.NewDecoder(io.LimitReader(poll.Body, 1<<20)).Decode(&state) != nil {
			poll.Body.Close()
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
		}
		poll.Body.Close()
		switch strings.ToUpper(state.Status) {
		case "COMPLETED":
			terminal = true
			goto result
		case "FAILED", "CANCELLED":
			terminal = true
			return imageBackendFailure(wire.ReplyOutcomeRefused, "upstream:"+strings.ToLower(state.Status))
		case "IN_QUEUE", "IN_PROGRESS":
		default:
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:status")
		}
	}
result:
	resultResp, failed := imageRequest(ctx, client, http.MethodGet, responseURL, "", nil, headers)
	if resultResp == nil {
		return failed
	}
	if json.NewDecoder(io.LimitReader(resultResp.Body, 8<<20)).Decode(&state) != nil {
		resultResp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	resultResp.Body.Close()
	urls := make([]string, 0, len(state.Images))
	for _, image := range state.Images {
		urls = append(urls, image.URL)
	}
	return fetchQueuedImages(ctx, client, urls, req.Count, "fal", bounds)
}

func fetchQueuedImages(ctx context.Context, client *http.Client, urls []string, expected int64, kind string, bounds imageOutputBounds) imageBackendReply {
	if int64(len(urls)) != expected {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:count")
	}
	result := imageBackendReply{outcome: wire.ReplyOutcomeCompleted, images: make([]generatedImage, 0, len(urls))}
	remaining := bounds.total
	for _, raw := range urls {
		limit := min(bounds.each, remaining)
		if limit < 1 {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
		}
		image, ok := fetchGeneratedImage(ctx, client, raw, kind, limit)
		if !ok {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image-url")
		}
		result.images = append(result.images, image)
		remaining -= int64(len(image.data))
	}
	return result
}

func comfyWorkflow(model string, req wire.ImageRequest) map[string]any {
	width, height, _ := imageDimensions(req.Size)
	if width == 0 {
		width, height = 512, 512
	}
	steps, _ := strconv.Atoi(req.Extensions["comfyui/steps"])
	if steps < 1 || steps > 150 {
		steps = 20
	}
	cfg, _ := strconv.ParseFloat(req.Extensions["comfyui/cfg"], 64)
	if cfg <= 0 || cfg > 30 {
		cfg = 7
	}
	sampler := req.Extensions["comfyui/sampler"]
	if sampler == "" {
		sampler = "euler"
	}
	negative := req.Extensions["comfyui/negative-prompt"]
	return map[string]any{
		"1": map[string]any{"class_type": "CheckpointLoaderSimple", "inputs": map[string]any{"ckpt_name": model}},
		"2": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": req.Prompt, "clip": []any{"1", 1}}},
		"3": map[string]any{"class_type": "CLIPTextEncode", "inputs": map[string]any{"text": negative, "clip": []any{"1", 1}}},
		"4": map[string]any{"class_type": "EmptyLatentImage", "inputs": map[string]any{"width": width, "height": height, "batch_size": req.Count}},
		"5": map[string]any{"class_type": "KSampler", "inputs": map[string]any{"seed": 0, "steps": steps, "cfg": cfg, "sampler_name": sampler, "scheduler": "normal", "denoise": 1, "model": []any{"1", 0}, "positive": []any{"2", 0}, "negative": []any{"3", 0}, "latent_image": []any{"4", 0}}},
		"6": map[string]any{"class_type": "VAEDecode", "inputs": map[string]any{"samples": []any{"5", 0}, "vae": []any{"1", 2}}},
		"7": map[string]any{"class_type": "SaveImage", "inputs": map[string]any{"filename_prefix": "openabstractions", "images": []any{"6", 0}}},
	}
}

func runComfyImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	clientID, _ := operationID()
	resp, failed := jsonImageRequest(ctx, client, http.MethodPost, strings.TrimRight(host.Base, "/")+"/prompt", map[string]any{"prompt": comfyWorkflow(model, req), "client_id": clientID}, nil)
	if resp == nil {
		return failed
	}
	var accepted struct {
		PromptID string `json:"prompt_id"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&accepted) != nil || accepted.PromptID == "" {
		resp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	delay := retryDelay(resp)
	resp.Body.Close()
	callerCtx := ctx
	ctx, stopPolling := imagePollContext(ctx)
	defer stopPolling()
	terminal := false
	defer func() {
		if terminal {
			return
		}
		cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cancelResp, _ := jsonImageRequest(cancelCtx, client, http.MethodPost, strings.TrimRight(host.Base, "/")+"/queue", map[string]any{"delete": []string{accepted.PromptID}}, nil)
		if cancelResp != nil {
			cancelResp.Body.Close()
		}
	}()
	for {
		if !waitImagePoll(ctx, delay) {
			if callerCtx.Err() != nil {
				return imageBackendFailure(wire.ReplyOutcomeCancelled, "caller")
			}
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:timeout")
		}
		history, failed := imageRequest(ctx, client, http.MethodGet, strings.TrimRight(host.Base, "/")+"/history/"+url.PathEscape(accepted.PromptID), "", nil, nil)
		if history == nil {
			if ctx.Err() != nil {
				if callerCtx.Err() != nil {
					return imageBackendFailure(wire.ReplyOutcomeCancelled, "caller")
				}
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:timeout")
			}
			return failed
		}
		delay = retryDelay(history)
		var records map[string]struct {
			Status struct {
				Completed bool   `json:"completed"`
				Status    string `json:"status_str"`
			} `json:"status"`
			Outputs map[string]struct {
				Images []struct{ Filename, Subfolder, Type string } `json:"images"`
			} `json:"outputs"`
		}
		if json.NewDecoder(io.LimitReader(history.Body, 8<<20)).Decode(&records) != nil {
			history.Body.Close()
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
		}
		history.Body.Close()
		record, ok := records[accepted.PromptID]
		if !ok || !record.Status.Completed {
			progress(0, "processing")
			continue
		}
		terminal = true
		var refs []struct{ Filename, Subfolder, Type string }
		keys := make([]string, 0, len(record.Outputs))
		for key := range record.Outputs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			refs = append(refs, record.Outputs[key].Images...)
		}
		if int64(len(refs)) != req.Count {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:count")
		}
		result := imageBackendReply{outcome: wire.ReplyOutcomeCompleted, images: make([]generatedImage, 0, len(refs))}
		remaining := bounds.total
		for _, ref := range refs {
			limit := min(bounds.each, remaining)
			if limit < 1 {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
			}
			query := url.Values{"filename": {ref.Filename}, "subfolder": {ref.Subfolder}, "type": {ref.Type}}
			view, failed := imageRequest(ctx, client, http.MethodGet, strings.TrimRight(host.Base, "/")+"/view?"+query.Encode(), "", nil, nil)
			if view == nil {
				return failed
			}
			data, ok := readBoundedImage(view.Body, limit)
			view.Body.Close()
			if !ok {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
			}
			result.images = append(result.images, generatedImage{data: data, mediaType: http.DetectContentType(data)})
			remaining -= int64(len(data))
		}
		return result
	}
}

func runSwarmImage(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.ImageRequest, bounds imageOutputBounds) imageBackendReply {
	base := strings.TrimRight(host.Base, "/")
	sessionResp, failed := jsonImageRequest(ctx, client, http.MethodPost, base+"/API/GetNewSession", map[string]any{}, nil)
	if sessionResp == nil {
		return failed
	}
	var session struct {
		SessionID string `json:"session_id"`
	}
	if json.NewDecoder(io.LimitReader(sessionResp.Body, 1<<20)).Decode(&session) != nil || session.SessionID == "" {
		sessionResp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	sessionResp.Body.Close()
	width, height, _ := imageDimensions(req.Size)
	body := map[string]any{"session_id": session.SessionID, "model": model, "prompt": req.Prompt, "images": req.Count}
	if width > 0 {
		body["width"], body["height"] = width, height
	}
	if negative := req.Extensions["swarmui/negative-prompt"]; negative != "" {
		body["negativeprompt"] = negative
	}
	resp, failed := jsonImageRequest(ctx, client, http.MethodPost, base+"/API/GenerateText2Image", body, nil)
	if resp == nil {
		return failed
	}
	var payload struct {
		Images []string `json:"images"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload) != nil {
		resp.Body.Close()
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:response")
	}
	resp.Body.Close()
	if int64(len(payload.Images)) != req.Count {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:count")
	}
	result := imageBackendReply{outcome: wire.ReplyOutcomeCompleted, images: make([]generatedImage, 0, len(payload.Images))}
	remaining := bounds.total
	for _, item := range payload.Images {
		var data []byte
		limit := min(bounds.each, remaining)
		if limit < 1 {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
		}
		if strings.HasPrefix(item, "data:image/") {
			_, encoded, ok := strings.Cut(item, ",")
			if !ok {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
			}
			data, ok = readBoundedImage(base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded)), limit)
			if !ok {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
			}
		} else {
			u, err := url.Parse(item)
			if err != nil || u.IsAbs() || strings.Contains(item, "..") {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image-url")
			}
			view, failed := imageRequest(ctx, client, http.MethodGet, base+"/"+strings.TrimLeft(path.Clean("/"+item), "/"), "", nil, nil)
			if view == nil {
				return failed
			}
			var ok bool
			data, ok = readBoundedImage(view.Body, limit)
			view.Body.Close()
			if !ok {
				return imageBackendFailure(wire.ReplyOutcomeUnavailable, "upstream:image")
			}
		}
		result.images = append(result.images, generatedImage{data: data, mediaType: http.DetectContentType(data)})
		remaining -= int64(len(data))
	}
	return result
}
