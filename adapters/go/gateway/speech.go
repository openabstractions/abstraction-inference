package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type oaiSpeechRequest struct {
	Model          string   `json:"model"`
	Voice          string   `json:"voice"`
	Input          string   `json:"input"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed"`
	Instructions   string   `json:"instructions"`
}

// speechOptions carries speed and instructions as speech@1 extensions
// (INF-S9). A speed of exactly 1 is the default and is not sent.
func speechOptions(in oaiSpeechRequest) (map[string]string, error) {
	options := map[string]string{}
	if in.Speed != nil && *in.Speed != 1 {
		if *in.Speed < inference.MinSpeechSpeed || *in.Speed > inference.MaxSpeechSpeed {
			return nil, fmt.Errorf("speed must be a number from %v to %v", inference.MinSpeechSpeed, inference.MaxSpeechSpeed)
		}
		options[inference.SpeechSpeedExtension] = strconv.FormatFloat(*in.Speed, 'f', -1, 64)
	}
	if in.Instructions != "" {
		if len(in.Instructions) > inference.MaxSpeechInstructionsBytes {
			return nil, fmt.Errorf("instructions must be at most %d bytes", inference.MaxSpeechInstructionsBytes)
		}
		options[inference.SpeechInstructionsExtension] = in.Instructions
	}
	if len(options) == 0 {
		return nil, nil
	}
	return options, nil
}

// speechOptionRefusal is the plain answer to a speech server that cannot
// honour a requested option, or false for any other refusal.
func speechOptionRefusal(model, reason string) (refusal, bool) {
	key, found := strings.CutPrefix(reason, "extension:")
	field := map[string]string{inference.SpeechSpeedExtension: "speed", inference.SpeechInstructionsExtension: "instructions"}[key]
	if !found || field == "" {
		return refusal{}, false
	}
	return refusal{status: http.StatusBadRequest, code: "unsupported_feature",
		message: fmt.Sprintf("%s is unsupported by the speech server for model %q; send the request without it", field, model)}, true
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
	options, err := speechOptions(in)
	if err != nil {
		w.badRequest(rw, b, in.Model, err)
		return
	}
	req := wire.SpeechRequest{Model: in.Model, Voice: in.Voice, Text: in.Input, Format: format, Extensions: options}
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	provider := w.cfg.Chat.(SpeechSynthesizer)
	a := w.startHeld(r, routed(r.Context(), b), b, req.Model, "speech", func(ctx context.Context) wire.Admission {
		return provider.StartSpeech(ctx, b.Subject(), req)
	})
	if a.Outcome != wire.StartOutcomeAccepted {
		if plain, ok := speechOptionRefusal(in.Model, a.Reason); ok && a.Outcome == wire.StartOutcomeUnsupportedFeature {
			// The option's name only: instructions can carry the caller's data.
			log.Printf("inference gateway: speech request from %s for model %q refused: the server cannot honour %s",
				b.Subject().Program, in.Model, a.Reason)
			plain.write(rw)
			return
		}
		refusalOf(a.Outcome.String(), a.Reason).write(rw)
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
					refusalOf(d.SpeechEnd.Outcome.String(), d.SpeechEnd.Reason).write(rw)
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
