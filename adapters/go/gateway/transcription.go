package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func audioType(data []byte) string {
	switch {
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE":
		return "audio/wav"
	case len(data) >= 3 && (string(data[:3]) == "ID3" || data[0] == 0xff && data[1]&0xe0 == 0xe0):
		return "audio/mpeg"
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "audio/mp4"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "audio/webm"
	case len(data) >= 4 && string(data[:4]) == "OggS":
		return "audio/ogg"
	case len(data) >= 4 && string(data[:4]) == "fLaC":
		return "audio/flac"
	}
	return ""
}

func (w *Window) transcriptionBadRequest(rw http.ResponseWriter, b Bound, model, reason string) {
	s := b.Subject()
	w.record(inference.Record{Profile: "transcription", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "invalid", Reason: reason})
	writeError(rw, http.StatusBadRequest, "invalid", reason)
}

// openAITranscription translates a bounded multipart upload into a digest
// reference, stores it under the original caller's content.write policy, and
// then starts transcription. Start independently requires content.read.
func (w *Window) openAITranscription(rw http.ResponseWriter, r *http.Request, b Bound, grant Grant) {
	r.Body = http.MaxBytesReader(rw, r.Body, MaxTranscriptionBodyBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		w.transcriptionBadRequest(rw, b, "", "multipart")
		return
	}
	fields := map[string][]string{}
	var audio []byte
	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			w.transcriptionBadRequest(rw, b, "", "multipart")
			return
		}
		name := part.FormName()
		if name == "file" {
			if audio != nil {
				//unchecked: mime/multipart.Part.Close always returns nil
				part.Close()
				w.transcriptionBadRequest(rw, b, "", "file")
				return
			}
			audio, err = io.ReadAll(io.LimitReader(part, (16<<20)+1))
		} else {
			var raw []byte
			raw, err = io.ReadAll(io.LimitReader(part, 4097))
			if len(raw) > 4096 {
				err = errors.New("field too large")
			}
			fields[name] = append(fields[name], string(raw))
		}
		//unchecked: mime/multipart.Part.Close always returns nil
		part.Close()
		if err != nil {
			w.transcriptionBadRequest(rw, b, "", "multipart")
			return
		}
	}
	model := first(fields["model"])
	if len(audio) == 0 || len(audio) > 16<<20 || model == "" {
		w.transcriptionBadRequest(rw, b, model, "file")
		return
	}
	mediaType := audioType(audio)
	if mediaType == "" {
		w.transcriptionBadRequest(rw, b, model, "media_type")
		return
	}
	format := first(fields["response_format"])
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "verbose_json" {
		w.transcriptionBadRequest(rw, b, model, "response_format")
		return
	}
	mode := wire.TimestampModeSegment
	if values := fields["timestamp_granularities[]"]; len(values) > 0 {
		segment, word := false, false
		for _, value := range values {
			segment, word = segment || value == "segment", word || value == "word"
			if value != "segment" && value != "word" {
				w.transcriptionBadRequest(rw, b, model, "timestamp_granularities")
				return
			}
		}
		switch {
		case segment && word:
			mode = wire.TimestampModeSegmentAndWord
		case word:
			mode = wire.TimestampModeWord
		}
	}
	if err := b.Recheck(); err != nil {
		s := b.Subject()
		w.record(inference.Record{Profile: "transcription", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "forbidden", Reason: "binding:" + BindingWord(err)})
		writeError(rw, http.StatusForbidden, "forbidden", "the connection no longer answers for the program that opened it")
		return
	}
	sum := sha256.Sum256(audio)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	switch outcome := w.cfg.StoreContent(r.Context(), b.Subject(), digest, audio); outcome {
	case inference.ContentResolved:
	case inference.ContentForbidden:
		s := b.Subject()
		w.record(inference.Record{Profile: "transcription", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "forbidden", Reason: "content:write:forbidden"})
		writeError(rw, http.StatusForbidden, "forbidden", "content write is not permitted")
		return
	case inference.ContentTooLarge:
		s := b.Subject()
		w.record(inference.Record{Profile: "transcription", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "invalid", Reason: "content:too-large"})
		writeError(rw, http.StatusBadRequest, "invalid", "audio is too large")
		return
	default:
		s := b.Subject()
		w.record(inference.Record{Profile: "transcription", Rung: b.Rung(), Account: s.Account, Program: s.Program, Model: model, Outcome: "unavailable", Reason: "content:write"})
		writeError(rw, http.StatusServiceUnavailable, "unavailable", "content store is unavailable")
		return
	}
	req := wire.TranscriptionRequest{Model: model, AudioDigest: digest, MediaType: mediaType, Language: first(fields["language"]), Timestamps: mode}
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
	} else {
		req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
	}
	transcriber := w.cfg.Chat.(Transcriber)
	ctx := inference.WithRoute(r.Context(), inference.RouteWindow, b.Rung())
	a := transcriber.StartTranscription(ctx, b.Subject(), req)
	if a.Outcome != wire.StartOutcomeAccepted {
		writeError(rw, status(a.Outcome.String()), a.Outcome.String(), refusalMessage(a.Outcome.String(), a.Reason))
		return
	}
	var units []wire.TranscriptSegment
	var end *wire.TranscriptionReply
	for cursor := int64(0); end == nil; {
		page := transcriber.ObserveTranscription(r.Context(), b.Subject(), a.Operation, cursor, 256, 65536, 25000)
		if r.Context().Err() != nil {
			transcriber.CancelTranscription(b.Subject(), a.Operation)
			return
		}
		if page.Outcome != wire.PageOutcomePage {
			transcriber.CancelTranscription(b.Subject(), a.Operation)
			writeError(rw, http.StatusServiceUnavailable, "unavailable", "observe: "+page.Outcome.String())
			return
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindSegment && d.Segment != nil {
				units = append(units, *d.Segment)
			}
			if d.Kind == wire.DeltaKindEnd {
				end = d.TranscriptionEnd
			}
		}
		cursor = page.Next
	}
	if end == nil || end.Outcome != wire.ReplyOutcomeCompleted {
		outcome, reason := "unavailable", "malformed terminal reply"
		if end != nil {
			outcome, reason = end.Outcome.String(), end.Reason
		}
		writeError(rw, status(outcome), outcome, refusalMessage(outcome, reason))
		return
	}
	var text strings.Builder
	for _, unit := range units {
		if unit.Kind == wire.TranscriptUnitKindSegment {
			//unchecked: strings.Builder.WriteString never returns a non-nil error
			text.WriteString(unit.Text)
		}
	}
	out := map[string]any{"text": text.String()}
	if format == "verbose_json" {
		out["language"], out["duration"] = end.Language, float64(end.DurationMs)/1000
		segments := make([]map[string]any, 0, len(units))
		words := make([]map[string]any, 0, len(units))
		for _, unit := range units {
			entry := map[string]any{"start": float64(unit.StartMs) / 1000, "end": float64(unit.EndMs) / 1000}
			if unit.Kind == wire.TranscriptUnitKindWord {
				entry["word"] = unit.Text
				words = append(words, entry)
			} else {
				entry["id"], entry["text"] = unit.Index, unit.Text
				segments = append(segments, entry)
			}
		}
		out["segments"], out["words"] = segments, words
	}
	rw.Header().Set("Content-Type", "application/json")
	//unchecked: terminal write of the response; headers and status are already committed and this package has no logger to report a write failure to (matches writeError and the other Encode calls in this package)
	_ = json.NewEncoder(rw).Encode(out)
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
