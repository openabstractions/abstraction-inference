package gateway

import (
	"net/http"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type oaiSpeechRequest struct {
	Model          string `json:"model"`
	Voice          string `json:"voice"`
	Input          string `json:"input"`
	ResponseFormat string `json:"response_format"`
}

func speechFormat(value string) (wire.SpeechFormat, bool) {
	if value == "" {
		value = "mp3"
	}
	return wire.ParseSpeechFormat(value)
}

func speechContentType(format wire.SpeechFormat) string {
	return map[wire.SpeechFormat]string{wire.SpeechFormatMp3: "audio/mpeg", wire.SpeechFormatOpus: "audio/ogg", wire.SpeechFormatAac: "audio/aac",
		wire.SpeechFormatFlac: "audio/flac", wire.SpeechFormatWav: "audio/wav", wire.SpeechFormatPcm: "audio/pcm"}[format]
}

func (w *Window) openAISpeech(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	var in oaiSpeechRequest
	if !w.readBody(rw, r, b, &in) {
		return
	}
	format, ok := speechFormat(in.ResponseFormat)
	if !ok {
		w.badRequest(rw, b, in.Model, errField("response_format"))
		return
	}
	req := wire.SpeechRequest{Model: in.Model, Voice: in.Voice, Text: in.Input, Format: format}
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	provider := w.cfg.Chat.(SpeechSynthesizer)
	ctx := inference.WithRoute(r.Context(), inference.RouteWindow, b.Rung())
	a := provider.StartSpeech(ctx, b.Subject(), req)
	if a.Outcome != wire.StartOutcomeAccepted {
		writeError(rw, status(a.Outcome.String()), a.Outcome.String(), refusalMessage(a.Outcome.String(), a.Reason))
		return
	}
	rw.Header().Set("Content-Type", speechContentType(format))
	flusher, _ := rw.(http.Flusher)
	wrote := false
	for cursor := int64(0); ; {
		page := provider.ObserveSpeech(r.Context(), b.Subject(), a.Operation, cursor, 256, 65536, 25000)
		if r.Context().Err() != nil {
			provider.CancelSpeech(b.Subject(), a.Operation)
			return
		}
		if page.Outcome != wire.PageOutcomePage {
			provider.CancelSpeech(b.Subject(), a.Operation)
			if !wrote {
				writeError(rw, http.StatusServiceUnavailable, "unavailable", "observe: "+page.Outcome.String())
			}
			return
		}
		for _, d := range page.Deltas {
			if d.Audio != nil {
				if _, err := rw.Write(d.Audio.Data); err != nil {
					provider.CancelSpeech(b.Subject(), a.Operation)
					return
				}
				wrote = true
				if flusher != nil {
					flusher.Flush()
				}
			}
			if d.SpeechEnd != nil {
				if d.SpeechEnd.Outcome != wire.ReplyOutcomeCompleted && !wrote {
					writeError(rw, status(d.SpeechEnd.Outcome.String()), d.SpeechEnd.Outcome.String(), refusalMessage(d.SpeechEnd.Outcome.String(), d.SpeechEnd.Reason))
				}
				return
			}
		}
		if page.AtEnd {
			provider.CancelSpeech(b.Subject(), a.Operation)
			if !wrote {
				writeError(rw, http.StatusServiceUnavailable, "unavailable", "observe: missing terminal reply")
			}
			return
		}
		cursor = page.Next
	}
}

type fieldError string

func (e fieldError) Error() string { return string(e) }
func errField(name string) error   { return fieldError(name) }
