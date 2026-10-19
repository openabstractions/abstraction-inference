package inference

import (
	"context"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
)

// TranscriptionEngine is an explicitly composed local transcription engine,
// such as a managed provider that builds whisper-server's request itself.
// Unsupported runs before rights, content and credential preflight and must
// make no engine effect; its non-empty answer refuses the request
// `unsupported_feature` with that reason. Run starts only after every
// admission step passed, with the caller's audio resolved and verified; ctx
// cancellation asks the engine to stop. The service checks each emitted
// segment and the terminal reply (INF-M6).
type TranscriptionEngine interface {
	Unsupported(req wire.TranscriptionRequest) string
	Run(ctx context.Context, model string, req wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply
}

// SpeechEngineReply is a speech engine's complete result before the service
// checks and publishes it. A failed outcome carries no audio.
type SpeechEngineReply struct {
	Outcome wire.ReplyOutcome
	Reason  string
	Audio   []byte
}

// SpeechEngine is an explicitly composed local speech engine. Unsupported
// runs as TranscriptionEngine's does. Run returns the complete audio in the
// requested format; the service checks its size and media type, streams it
// as audio deltas and publishes it under the caller's output authorization
// (INF-S6, INF-S7).
type SpeechEngine interface {
	Unsupported(req wire.SpeechRequest) string
	Run(ctx context.Context, model string, req wire.SpeechRequest) SpeechEngineReply
}

// speechChunk keeps one audio delta's compact JSON under Observe's 64 KiB
// page budget after base64 expansion.
const speechChunk = 47 * 1024

func (p *Provider) transcriptionEngineFor(host *router.Host) TranscriptionEngine {
	if p.cfg.TranscriptionEngine != nil && host != nil && host == p.cfg.TranscriptionEngineHost {
		return p.cfg.TranscriptionEngine
	}
	return nil
}

func (p *Provider) speechEngineFor(host *router.Host) SpeechEngine {
	if p.cfg.SpeechEngine != nil && host != nil && host == p.cfg.SpeechEngineHost {
		return p.cfg.SpeechEngine
	}
	return nil
}

// engineRequest is the request an engine receives: the selected model, no
// credential name, and a copy of the vendor extensions.
func engineTranscriptionRequest(req wire.TranscriptionRequest, model string) wire.TranscriptionRequest {
	forwarded := req
	forwarded.Model = model
	forwarded.Credential = ""
	forwarded.Extensions = cloneStrings(req.Extensions)
	return forwarded
}

func engineSpeechRequest(req wire.SpeechRequest, model string) wire.SpeechRequest {
	forwarded := req
	forwarded.Model = model
	forwarded.Credential = ""
	forwarded.Extensions = cloneStrings(req.Extensions)
	return forwarded
}

func validSegment(s wire.TranscriptSegment) bool {
	return s.Kind.Known() && s.Index >= 0 && s.StartMs >= 0 && s.EndMs >= s.StartMs && len(s.Text) <= maxTranscriptTextBytes && utf8.ValidString(s.Text)
}

func (p *Provider) runTranscriptionEngine(ctx context.Context, op *operation, req wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	engine := p.transcriptionEngineFor(op.host)
	if engine == nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "engine:host")
	}
	invalid := false
	checked := func(s wire.TranscriptSegment) {
		if invalid || ctx.Err() != nil {
			return
		}
		if !validSegment(s) {
			invalid = true
			return
		}
		emit(s)
	}
	end := engine.Run(ctx, op.model, engineTranscriptionRequest(req, op.model), append([]byte(nil), audio...), checked)
	if ctx.Err() != nil {
		return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
	}
	if invalid || !end.Outcome.Known() || !validImageAccounting(end.Reason, end.Usage, end.Cost) || !validUpstreamLanguage(end.Language) || end.DurationMs < 0 {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "engine:invalid_reply")
	}
	return end
}

func (p *Provider) runSpeechEngine(ctx context.Context, op *operation, req wire.SpeechRequest, commit ContentCommitter, emit func([]byte)) wire.SpeechReply {
	engine := p.speechEngineFor(op.host)
	if engine == nil {
		return speechFailed(wire.ReplyOutcomeUnavailable, "engine:host")
	}
	result := engine.Run(ctx, op.model, engineSpeechRequest(req, op.model))
	if ctx.Err() != nil {
		return speechFailed(wire.ReplyOutcomeCancelled, "caller")
	}
	if !result.Outcome.Known() || len(result.Reason) > 4096 || !utf8.ValidString(result.Reason) || result.Outcome != wire.ReplyOutcomeCompleted && len(result.Audio) != 0 {
		return speechFailed(wire.ReplyOutcomeUnavailable, "engine:invalid_reply")
	}
	if result.Outcome != wire.ReplyOutcomeCompleted {
		return speechFailed(result.Outcome, result.Reason)
	}
	if int64(len(result.Audio)) > p.cfg.MaxSpeechBytes {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:too-large")
	}
	media := speechMediaType(req.Format)
	if !mediationcontent.MediaMatches(result.Audio, media) {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:media-type")
	}
	audio := append([]byte(nil), result.Audio...)
	for offset := 0; offset < len(audio); offset += speechChunk {
		emit(audio[offset:min(offset+speechChunk, len(audio))])
	}
	return publishSpeech(ctx, audio, media, commit, op.beginCompletion)
}
