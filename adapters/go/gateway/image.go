package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	w.finishOpenAIImage(rw, r, b, imageDefaults(input.Model, input.Prompt, input.Size, input.N, grant))
}

func gatewayImageMedia(data []byte) string {
	media := http.DetectContentType(data)
	if media == "image/png" || media == "image/jpeg" || media == "image/webp" {
		return media
	}
	return ""
}

func storeGatewayImage(w *Window, ctx *http.Request, b Bound, data []byte) (string, string, inference.ContentOutcome) {
	media := gatewayImageMedia(data)
	if media == "" {
		return "", "", inference.ContentUnknown
	}
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	outcome := w.cfg.StoreContent(ctx.Context(), b.Subject(), digest, data)
	if outcome != inference.ContentResolved {
		return "", "", outcome
	}
	return digest, media, inference.ContentResolved
}

func (w *Window) imageWriteFailure(rw http.ResponseWriter, b Bound, model string, outcome inference.ContentOutcome) {
	s := b.Subject()
	switch outcome {
	case inference.ContentForbidden:
		w.record(inference.Record{Profile: "image", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "forbidden", Reason: "content:write:forbidden"})
		writeError(rw, http.StatusForbidden, "forbidden", "content write is not permitted")
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
	imageDigest, imageMedia, outcome := storeGatewayImage(w, r, b, image)
	if outcome != inference.ContentResolved {
		w.imageWriteFailure(rw, b, fields["model"], outcome)
		return
	}
	req := imageDefaults(fields["model"], fields["prompt"], fields["size"], count, grant)
	req.Mode, req.ImageDigest, req.ImageMediaType = wire.ImageModeEdit, imageDigest, imageMedia
	if len(mask) > 0 {
		maskDigest, maskMedia, outcome := storeGatewayImage(w, r, b, mask)
		if outcome != inference.ContentResolved {
			w.imageWriteFailure(rw, b, fields["model"], outcome)
			return
		}
		if maskMedia != "image/png" {
			w.badRequest(rw, b, fields["model"], errField("mask"))
			return
		}
		req.MaskDigest, req.MaskMediaType = maskDigest, maskMedia
	}
	w.finishOpenAIImage(rw, r, b, req)
}

func (w *Window) finishOpenAIImage(rw http.ResponseWriter, r *http.Request, b Bound, req wire.ImageRequest) {
	provider := w.cfg.Chat.(ImageGenerator)
	ctx := inference.WithLocalImageDelivery(inference.WithRoute(r.Context(), inference.RouteWindow, b.Rung()))
	admission := provider.StartImage(ctx, b.Subject(), req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		writeError(rw, status(admission.Outcome.String()), admission.Outcome.String(), refusalMessage(admission.Outcome.String(), admission.Reason))
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
		writeError(rw, status(end.Outcome.String()), end.Outcome.String(), refusalMessage(end.Outcome.String(), end.Reason))
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
