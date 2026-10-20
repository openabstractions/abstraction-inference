package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	digestfmt "github.com/openabstractions/abstraction-storage/go/digest"
	"io"
	"net/http"
	"strconv"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type oaiImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int64  `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

func imageDefaults(model, prompt, size string, count int64, grant Grant) wire.ImageRequest {
	if size == "" {
		size = "1024x1024"
	}
	if count == 0 {
		count = 1
	}
	req := wire.ImageRequest{Model: model, Mode: wire.ImageModeGenerate, Prompt: prompt, Size: size, Count: count, Extensions: map[string]string{}}
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	return req
}

func (w *Window) openAIImageGeneration(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	var input oaiImageRequest
	if !w.readBody(rw, r, b, &input) {
		return
	}
	if input.ResponseFormat != "" && input.ResponseFormat != "b64_json" {
		w.badRequest(rw, b, input.Model, errField("response_format"))
		return
	}
	w.finishOpenAIImage(rw, r, b, imageDefaults(input.Model, input.Prompt, input.Size, input.N, grant), nil)
}

func gatewayImageMedia(data []byte) string {
	media := http.DetectContentType(data)
	if media == "image/png" || media == "image/jpeg" || media == "image/webp" {
		return media
	}
	return ""
}

func gatewayDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return digestfmt.Hex(sum[:])
}

// storeGatewayImage stores one upload for the bound subject on ctx, which
// carries the hold's first-use collector.
func storeGatewayImage(w *Window, ctx context.Context, b Bound, data []byte) inference.ContentOutcome {
	return w.cfg.StoreContent(ctx, b.Subject(), gatewayDigest(data), data)
}

// imageWriteFailure answers an upload the store did not take. reason is the
// refusal's reason for ContentForbidden: content:write:forbidden and the
// questions still pending (INF-W11).
func (w *Window) imageWriteFailure(rw http.ResponseWriter, b Bound, model string, outcome inference.ContentOutcome, reason string) {
	s := b.Subject()
	switch outcome {
	case inference.ContentForbidden:
		w.record(inference.Record{Profile: "image", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "forbidden", Reason: storeRefusedReason})
		refusalOf("forbidden", reason).write(rw)
	case inference.ContentTooLarge, inference.ContentUnknown:
		w.record(inference.Record{Profile: "image", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "invalid", Reason: "content:invalid"})
		writeError(rw, http.StatusBadRequest, "invalid", "image content is invalid or too large")
	default:
		w.record(inference.Record{Profile: "image", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "unavailable", Reason: "content:write"})
		writeError(rw, http.StatusServiceUnavailable, "unavailable", "content store is unavailable")
	}
}

func (w *Window) openAIImageEdit(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	r.Body = http.MaxBytesReader(rw, r.Body, MaxImageEditBodyBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		w.badRequest(rw, b, "", errField("multipart"))
		return
	}
	fields := map[string]string{}
	var image, mask []byte
	for {
		part, next := reader.NextPart()
		if errors.Is(next, io.EOF) {
			break
		}
		if next != nil {
			w.badRequest(rw, b, fields["model"], errField("multipart"))
			return
		}
		var data []byte
		if part.FormName() == "image" || part.FormName() == "mask" {
			data, err = io.ReadAll(io.LimitReader(part, (20<<20)+1))
			if len(data) > 20<<20 {
				err = errors.New("image too large")
			}
			if part.FormName() == "image" {
				image = data
			} else {
				mask = data
			}
		} else {
			data, err = io.ReadAll(io.LimitReader(part, 32769))
			fields[part.FormName()] = string(data)
		}
		//unchecked: mime/multipart.Part.Close always returns nil
		part.Close()
		if err != nil {
			w.badRequest(rw, b, fields["model"], errField("multipart"))
			return
		}
	}
	count := int64(1)
	if fields["n"] != "" {
		var parseErr error
		count, parseErr = strconv.ParseInt(fields["n"], 10, 64)
		if parseErr != nil {
			w.badRequest(rw, b, fields["model"], errField("n"))
			return
		}
	}
	if len(image) == 0 || fields["model"] == "" || fields["prompt"] == "" {
		w.badRequest(rw, b, fields["model"], errField("image"))
		return
	}
	if err := b.Recheck(); err != nil {
		writeError(rw, http.StatusForbidden, "forbidden", "the connection no longer answers for the program that opened it")
		return
	}
	imageMedia := gatewayImageMedia(image)
	if imageMedia == "" {
		w.imageWriteFailure(rw, b, fields["model"], inference.ContentUnknown, "")
		return
	}
	req := imageDefaults(fields["model"], fields["prompt"], fields["size"], count, grant)
	req.Mode, req.ImageDigest, req.ImageMediaType = wire.ImageModeEdit, gatewayDigest(image), imageMedia
	if len(mask) > 0 {
		maskMedia := gatewayImageMedia(mask)
		if maskMedia == "" {
			w.imageWriteFailure(rw, b, fields["model"], inference.ContentUnknown, "")
			return
		}
		if maskMedia != "image/png" {
			w.badRequest(rw, b, fields["model"], errField("mask"))
			return
		}
		req.MaskDigest, req.MaskMediaType = gatewayDigest(mask), maskMedia
	}
	// The uploads are stored inside the start's hold: a store refused for
	// want of input:image holds for its question like a refused start.
	w.finishOpenAIImage(rw, r, b, req, func(ctx context.Context) inference.ContentOutcome {
		if outcome := storeGatewayImage(w, ctx, b, image); outcome != inference.ContentResolved || len(mask) == 0 {
			return outcome
		}
		return storeGatewayImage(w, ctx, b, mask)
	})
}

// finishOpenAIImage stores the request's uploads through store, when it is
// not nil, and admits and answers the request, all within one hold.
func (w *Window) finishOpenAIImage(rw http.ResponseWriter, r *http.Request, b Bound, req wire.ImageRequest, store func(context.Context) inference.ContentOutcome) {
	provider := w.cfg.Chat.(ImageGenerator)
	ctx := inference.WithLocalImageDelivery(routed(r.Context(), b))
	admission, stored := w.startStored(r, ctx, b, req.Model, "image", store, func(ctx context.Context) wire.Admission {
		return provider.StartImage(ctx, b.Subject(), req)
	})
	if stored != inference.ContentResolved {
		w.imageWriteFailure(rw, b, req.Model, stored, admission.Reason)
		return
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		refusalOf(admission.Outcome.String(), admission.Reason).write(rw)
		return
	}
	var deliveries []wire.Delivery
	var end *wire.ImageReply
	for cursor := int64(0); end == nil; {
		page := provider.ObserveImage(r.Context(), b.Subject(), admission.Operation, cursor, 256, 65536, 25000)
		if r.Context().Err() != nil {
			provider.CancelImage(b.Subject(), admission.Operation)
			return
		}
		if page.Outcome != wire.PageOutcomePage {
			provider.CancelImage(b.Subject(), admission.Operation)
			writeError(rw, http.StatusServiceUnavailable, "unavailable", "observe: "+page.Outcome.String())
			return
		}
		for _, delta := range page.Deltas {
			if delta.ImageResult != nil {
				deliveries = append(deliveries, delta.ImageResult.Delivery)
			}
			if delta.ImageEnd != nil {
				end = delta.ImageEnd
			}
		}
		if page.AtEnd && end == nil {
			writeError(rw, http.StatusServiceUnavailable, "unavailable", "observe: missing terminal reply")
			return
		}
		cursor = page.Next
	}
	if end.Outcome != wire.ReplyOutcomeCompleted || len(deliveries) != len(end.Deliveries) {
		refusalOf(end.Outcome.String(), end.Reason).write(rw)
		return
	}
	data := make([]map[string]string, 0, len(deliveries))
	for _, delivery := range deliveries {
		if delivery.Location != "local" {
			writeError(rw, http.StatusServiceUnavailable, "unavailable", "generated image is not locally readable")
			return
		}
		bytes, outcome := w.cfg.ReadContent(r.Context(), b.Subject(), delivery.Digest, delivery.Size)
		if outcome != inference.ContentResolved || int64(len(bytes)) != delivery.Size {
			writeError(rw, http.StatusForbidden, "forbidden", "generated image is not readable")
			return
		}
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(bytes)})
	}
	rw.Header().Set("Content-Type", "application/json")
	//unchecked: terminal write of the response; headers and status are already committed and this package has no logger to report a write failure to (matches writeError and the other Encode calls in this package)
	_ = json.NewEncoder(rw).Encode(map[string]any{"data": data})
}
