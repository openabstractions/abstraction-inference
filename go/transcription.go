package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const TranscriptionContract = "abstraction.inference/transcription@1"
const maxTranscriptTextBytes = 32768

func validateTranscription(req wire.TranscriptionRequest) string {
	if req.Model == "" || len(req.Model) > 256 {
		return "model"
	}
	if !canonicalSHA256(req.AudioDigest) {
		return "audio_digest"
	}
	if !audioMediaType(req.MediaType) {
		return "media_type"
	}
	if req.Language != "" && (len(req.Language) > 35 || !nameChars(req.Language, "-")) {
		return "language"
	}
	if !req.Timestamps.Known() {
		return "timestamps"
	}
	for key := range req.Extensions {
		owner, name, ok := strings.Cut(key, "/")
		if !ok || owner == "" || name == "" || len(key) > 128 {
			return "extensions"
		}
	}
	local, hosted := false, false
	for _, g := range req.Guarantees {
		switch g {
		case wire.RequestGuaranteeLocalOnly:
			local = true
		case wire.RequestGuaranteeHostedAllowed:
			hosted = true
		default:
			return "guarantees"
		}
	}
	if local && hosted {
		return "guarantees"
	}
	if c := req.Credential; c != "" && (len(c) > 64 || !nameChars(c, "_-.")) {
		return "credential"
	}
	return ""
}

func audioMediaType(mediaType string) bool {
	switch mediaType {
	case "audio/wav", "audio/mpeg", "audio/mp4", "audio/webm", "audio/ogg", "audio/flac":
		return true
	}
	return false
}

func audioMatches(data []byte, mediaType string) bool {
	switch mediaType {
	case "audio/wav":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE"
	case "audio/mpeg":
		return len(data) >= 3 && (string(data[:3]) == "ID3" || data[0] == 0xff && data[1]&0xe0 == 0xe0)
	case "audio/mp4":
		return len(data) >= 12 && string(data[4:8]) == "ftyp"
	case "audio/webm":
		return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3})
	case "audio/ogg":
		return len(data) >= 4 && string(data[:4]) == "OggS"
	case "audio/flac":
		return len(data) >= 4 && string(data[:4]) == "fLaC"
	}
	return false
}

func (p *Provider) resolveAudio(ctx context.Context, subject Subject, req wire.TranscriptionRequest) ([]byte, wire.StartOutcome, string) {
	if p.cfg.ResolveContent == nil {
		return nil, wire.StartOutcomeUnavailable, "content:unavailable"
	}
	data, outcome := p.cfg.ResolveContent(ctx, subject, req.AudioDigest, p.cfg.MaxAudioBytes)
	if ctx.Err() != nil {
		return nil, wire.StartOutcomeUnavailable, "content:unavailable"
	}
	switch outcome {
	case ContentUnknown:
		return nil, wire.StartOutcomeInvalid, "content:unknown"
	case ContentForbidden:
		return nil, wire.StartOutcomeForbidden, "content:forbidden"
	case ContentTooLarge:
		return nil, wire.StartOutcomeInvalid, "content:too-large"
	case ContentUnavailable:
		return nil, wire.StartOutcomeUnavailable, "content:unavailable"
	case ContentResolved:
	default:
		return nil, wire.StartOutcomeUnavailable, "content:unavailable"
	}
	if int64(len(data)) > p.cfg.MaxAudioBytes {
		return nil, wire.StartOutcomeInvalid, "content:too-large"
	}
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != req.AudioDigest {
		return nil, wire.StartOutcomeInvalid, "content:digest"
	}
	if !audioMatches(data, req.MediaType) {
		return nil, wire.StartOutcomeInvalid, "content:media-type"
	}
	return append([]byte(nil), data...), 0, ""
}

func transcriptionAdapterFor(host *router.Host) string {
	switch {
	case host.Wire == router.WireRemote:
		return "remote"
	case host.Wire == router.WireDeepgramPrerecorded:
		return "deepgram"
	case host.Name == "whispercpp":
		return "whispercpp"
	case host.Wire == "" || host.Wire == router.WireOpenAICompatible:
		return "openai"
	}
	return ""
}

func (p *Provider) StartTranscription(ctx context.Context, subject Subject, req wire.TranscriptionRequest) wire.Admission {
	started, via := p.cfg.Now(), routeOf(ctx)
	if via.name == RouteRemote {
		via.claim = req.Extensions[ClaimExtension]
	}
	refuse := func(outcome wire.StartOutcome, reason string, host *router.Host, model string) wire.Admission {
		r := Record{Profile: router.ProfileTranscription, Route: via.name, Rung: via.rung, Account: subject.Account, Program: subject.Program, Model: req.Model, Family: familyOf(req.Model), Credential: req.Credential, Outcome: outcome.String(), Reason: reason, WallMS: p.cfg.Now().Sub(started).Milliseconds(), Domain: via.domain, Claim: via.claim}
		if host != nil {
			r.Host, r.Model = host.Name, model
			if host.Wire == router.WireRemote {
				r.Domain = host.Domain
			}
		}
		if p.cfg.Ceilings != nil && req.Credential != "" {
			r.Ceiling = p.cfg.Ceilings.State(req.Credential)
		}
		p.record(r)
		return refusal(outcome, reason)
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.StartOutcomeForbidden, "caller", nil, "")
	}
	if reason := validateTranscription(req); reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason, nil, "")
	}
	host, model, reason := p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileTranscription)
	if host == nil {
		return refuse(wire.StartOutcomeNoHost, reason, nil, "")
	}
	kind := transcriptionAdapterFor(host)
	if kind == "" || host.TranscriptionURL() == "" {
		return refuse(wire.StartOutcomeUnsupportedFeature, "wire:"+host.Wire, host, model)
	}
	if kind == "whispercpp" && req.MediaType != "audio/wav" {
		return refuse(wire.StartOutcomeUnsupportedFeature, "format:"+req.MediaType, host, model)
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		p.report(err)
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	switch word {
	case "permitted":
	case "denied", "not_granted", "unknown_action":
		return refuse(wire.StartOutcomeNotPermitted, "rights:"+word, host, model)
	default:
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	remote := host.Wire == router.WireRemote
	var audio []byte
	if !remote {
		var outcome wire.StartOutcome
		audio, outcome, reason = p.resolveAudio(ctx, subject, req)
		if reason != "" {
			return refuse(outcome, reason, host, model)
		}
	}
	credential := ""
	if host.Hosted {
		credential = host.Credential
	}
	if credential != "" && p.cfg.Ceilings != nil && !remote {
		if unit, over := p.cfg.Ceilings.Exceeded(credential); over {
			return refuse(wire.StartOutcomeBudgetExceeded, "ceiling:"+unit+":"+credential, host, model)
		}
	}
	if p.active(subject) >= p.cfg.MaxOperations {
		return refuse(wire.StartOutcomeExhausted, "capacity", host, model)
	}
	var headers map[string]string
	if credential != "" && !remote {
		headers, word = p.cfg.Apply(ctx, subject, TranscriptionContract, credential, router.Target(host.Base))
		switch word {
		case "applied":
		case "unavailable", "":
			return refuse(wire.StartOutcomeUnavailable, "credential:unavailable:"+credential, host, model)
		default:
			return refuse(wire.StartOutcomeNotPermitted, "credential:"+word+":"+credential, host, model)
		}
	}
	id, err := operationID()
	if err != nil {
		clear(headers)
		return refuse(wire.StartOutcomeUnavailable, "capacity", host, model)
	}
	op := newOperation(id, subject, host, model, credential, p.cfg.RetainedDeltas, started)
	op.profile, op.route, op.remote = router.ProfileTranscription, via, remote
	runCtx, cancel := context.WithCancel(p.ctx)
	op.cancel = cancel
	if outcome := p.admitOperation(op); outcome != wire.StartOutcomeAccepted {
		cancel()
		clear(headers)
		reason := "capacity"
		if outcome == wire.StartOutcomeUnavailable {
			reason = "provider:closed"
		}
		return refuse(outcome, reason, host, model)
	}
	go p.runTranscription(runCtx, op, kind, req, audio, headers)
	a := wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: id, Host: host.Name, Model: model}
	p.bounds(&a)
	return a
}

func (p *Provider) ObserveTranscription(ctx context.Context, subject Subject, id string, cursor, maxDeltas, maxBytes, waitMS int64) wire.DeltaPage {
	return p.observeProfile(ctx, subject, id, router.ProfileTranscription, cursor, maxDeltas, maxBytes, waitMS)
}
func (p *Provider) CancelTranscription(subject Subject, id string) wire.Cancellation {
	return p.cancelProfile(subject, id, router.ProfileTranscription)
}

func (p *Provider) runTranscription(ctx context.Context, op *operation, kind string, req wire.TranscriptionRequest, audio []byte, headers map[string]string) {
	defer p.runs.Done()
	defer close(op.finished)
	defer op.cancel()
	emit := func(segment wire.TranscriptSegment) {
		op.emit(wire.Delta{Kind: wire.DeltaKindSegment, Segment: &segment}, p.cfg.Now())
	}
	var end wire.TranscriptionReply
	if kind == "remote" {
		end = p.runRemoteTranscription(ctx, op, req, emit)
	} else {
		end = runTranscriptionHTTP(ctx, p.cfg.HTTP, op.host, op.model, kind, req, audio, headers, emit)
	}
	clear(headers)
	if reason := op.stopped(); reason != "" {
		end.Outcome, end.Reason = wire.ReplyOutcomeCancelled, reason
	} else if p.ctx.Err() != nil || ctx.Err() != nil && end.Outcome == wire.ReplyOutcomeUnavailable {
		end.Outcome, end.Reason = wire.ReplyOutcomeCancelled, "caller"
	}
	end.Host, end.Model = op.host.Name, op.model
	audioSeconds := (end.DurationMs + 999) / 1000
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		var cost int64
		if end.Cost != nil {
			cost = end.Cost.Micros
		}
		if err := p.cfg.Ceilings.AddUsage(op.credential, end.Usage.Input+end.Usage.Output, cost, audioSeconds); err != nil {
			p.report(err)
		}
	}
	now := p.cfg.Now()
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, TranscriptionEnd: &end}, now)
	r := Record{Operation: op.id, Profile: router.ProfileTranscription, Route: op.route.name, Rung: op.route.rung, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name, Model: op.model, Family: familyOf(op.model), Credential: op.credential, Outcome: end.Outcome.String(), Reason: end.Reason, TokensIn: end.Usage.Input, TokensOut: end.Usage.Output, AudioSeconds: audioSeconds, WallMS: now.Sub(op.started).Milliseconds(), Domain: op.route.domain, Claim: op.route.claim}
	if op.remote {
		r.Domain = op.host.Domain
	}
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		r.Ceiling = p.cfg.Ceilings.State(op.credential)
	}
	p.record(r)
}

func transcriptionFailed(outcome wire.ReplyOutcome, reason string) wire.TranscriptionReply {
	return wire.TranscriptionReply{Outcome: outcome, Reason: reason}
}
func fileExtension(mediaType string) string {
	return map[string]string{"audio/wav": ".wav", "audio/mpeg": ".mp3", "audio/mp4": ".mp4", "audio/webm": ".webm", "audio/ogg": ".ogg", "audio/flac": ".flac"}[mediaType]
}
func safeMillis(seconds float64) (int64, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/1000 {
		return 0, false
	}
	return int64(math.Round(seconds * 1000)), true
}

func validUpstreamLanguage(language string) bool {
	return language == "" || len(language) <= 35 && nameChars(language, "-")
}

func runTranscriptionHTTP(ctx context.Context, client *http.Client, host *router.Host, model, kind string, input wire.TranscriptionRequest, audio []byte, headers map[string]string, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	if kind == "deepgram" {
		return runDeepgram(ctx, client, host, model, input, audio, headers, emit)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="audio%s"`, fileExtension(input.MediaType)))
	h.Set("Content-Type", input.MediaType)
	part, _ := mw.CreatePart(h)
	_, _ = part.Write(audio)
	_ = mw.WriteField("response_format", "verbose_json")
	if kind != "whispercpp" {
		_ = mw.WriteField("model", model)
	}
	if input.Language != "" {
		_ = mw.WriteField("language", input.Language)
	}
	if input.Timestamps == wire.TimestampModeSegment || input.Timestamps == wire.TimestampModeSegmentAndWord {
		_ = mw.WriteField("timestamp_granularities[]", "segment")
	}
	if input.Timestamps == wire.TimestampModeWord || input.Timestamps == wire.TimestampModeSegmentAndWord {
		_ = mw.WriteField("timestamp_granularities[]", "word")
		if kind == "whispercpp" {
			_ = mw.WriteField("token_timestamps", "true")
		}
	}
	_ = mw.Close()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, host.TranscriptionURL(), &body)
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:address")
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		outcome := wire.ReplyOutcomeRefused
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return transcriptionFailed(outcome, fmt.Sprintf("upstream:%d", resp.StatusCode))
	}
	var result openAITranscript
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result) != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:malformed")
	}
	durationMS, ok := safeMillis(result.Duration)
	if !ok || !validUpstreamLanguage(result.Language) || len(result.Text) > maxTranscriptTextBytes {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
	}
	if input.Timestamps == wire.TimestampModeNone && result.Text != "" {
		emit(wire.TranscriptSegment{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 0, EndMs: durationMS, Text: result.Text})
	}
	if input.Timestamps == wire.TimestampModeSegment || input.Timestamps == wire.TimestampModeSegmentAndWord {
		for i, segment := range result.Segments {
			startMS, startOK := safeMillis(segment.Start)
			endMS, endOK := safeMillis(segment.End)
			if len(segment.Text) > maxTranscriptTextBytes || !startOK || !endOK || endMS < startMS {
				return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
			}
			emit(wire.TranscriptSegment{Index: int64(i), Kind: wire.TranscriptUnitKindSegment, StartMs: startMS, EndMs: endMS, Text: segment.Text})
		}
	}
	if input.Timestamps == wire.TimestampModeWord || input.Timestamps == wire.TimestampModeSegmentAndWord {
		for i, word := range result.Words {
			startMS, startOK := safeMillis(word.Start)
			endMS, endOK := safeMillis(word.End)
			if len(word.Word) > maxTranscriptTextBytes || !startOK || !endOK || endMS < startMS {
				return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
			}
			emit(wire.TranscriptSegment{Index: int64(i), Kind: wire.TranscriptUnitKindWord, StartMs: startMS, EndMs: endMS, Text: word.Word})
		}
	}
	end := wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted, Language: result.Language, DurationMs: durationMS}
	if result.Usage != nil {
		end.Usage.Input = max(result.Usage.Input, result.Usage.InputTokens)
		end.Usage.Output = max(result.Usage.Output, result.Usage.OutputTokens)
	}
	return end
}

type openAITranscript struct {
	Language string  `json:"language"`
	Duration float64 `json:"duration"`
	Text     string  `json:"text"`
	Segments []struct {
		ID    int64   `json:"id"`
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
	Words []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Word  string  `json:"word"`
	} `json:"words"`
	Usage *struct {
		Input        int64 `json:"input"`
		Output       int64 `json:"output"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

func runDeepgram(ctx context.Context, client *http.Client, host *router.Host, model string, input wire.TranscriptionRequest, audio []byte, headers map[string]string, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	u, err := url.Parse(host.TranscriptionURL())
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:address")
	}
	q := u.Query()
	q.Set("model", model)
	q.Set("utterances", "true")
	q.Set("punctuate", "true")
	if input.Language != "" {
		q.Set("language", input.Language)
	} else {
		q.Set("detect_language", "true")
	}
	u.RawQuery = q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(audio))
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:address")
	}
	httpReq.Header.Set("Content-Type", input.MediaType)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		outcome := wire.ReplyOutcomeRefused
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return transcriptionFailed(outcome, fmt.Sprintf("upstream:%d", resp.StatusCode))
	}
	var result deepgramTranscript
	if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result) != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:malformed")
	}
	durationMS, ok := safeMillis(result.Metadata.Duration)
	if !ok {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
	}
	language := input.Language
	if language == "" && len(result.Results.Channels) > 0 {
		language = result.Results.Channels[0].DetectedLanguage
	}
	if !validUpstreamLanguage(language) {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
	}
	if input.Timestamps == wire.TimestampModeNone && len(result.Results.Channels) > 0 && len(result.Results.Channels[0].Alternatives) > 0 {
		text := result.Results.Channels[0].Alternatives[0].Transcript
		if len(text) > maxTranscriptTextBytes {
			return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
		}
		if text != "" {
			emit(wire.TranscriptSegment{Index: 0, Kind: wire.TranscriptUnitKindSegment, StartMs: 0, EndMs: durationMS, Text: text})
		}
	}
	if input.Timestamps == wire.TimestampModeSegment || input.Timestamps == wire.TimestampModeSegmentAndWord {
		for i, unit := range result.Results.Utterances {
			startMS, startOK := safeMillis(unit.Start)
			endMS, endOK := safeMillis(unit.End)
			if len(unit.Transcript) > maxTranscriptTextBytes || !startOK || !endOK || endMS < startMS {
				return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
			}
			emit(wire.TranscriptSegment{Index: int64(i), Kind: wire.TranscriptUnitKindSegment, StartMs: startMS, EndMs: endMS, Text: unit.Transcript})
		}
	}
	if input.Timestamps == wire.TimestampModeWord || input.Timestamps == wire.TimestampModeSegmentAndWord {
		if len(result.Results.Channels) > 0 && len(result.Results.Channels[0].Alternatives) > 0 {
			for i, unit := range result.Results.Channels[0].Alternatives[0].Words {
				startMS, startOK := safeMillis(unit.Start)
				endMS, endOK := safeMillis(unit.End)
				if len(unit.Word) > maxTranscriptTextBytes || !startOK || !endOK || endMS < startMS {
					return transcriptionFailed(wire.ReplyOutcomeUnavailable, "upstream:segments")
				}
				emit(wire.TranscriptSegment{Index: int64(i), Kind: wire.TranscriptUnitKindWord, StartMs: startMS, EndMs: endMS, Text: unit.Word})
			}
		}
	}
	return wire.TranscriptionReply{Outcome: wire.ReplyOutcomeCompleted, Language: language, DurationMs: durationMS}
}

type deepgramTranscript struct {
	Metadata struct {
		Duration float64 `json:"duration"`
	} `json:"metadata"`
	Results struct {
		Utterances []struct {
			Start      float64 `json:"start"`
			End        float64 `json:"end"`
			Transcript string  `json:"transcript"`
		} `json:"utterances"`
		Channels []struct {
			DetectedLanguage string `json:"detected_language"`
			Alternatives     []struct {
				Transcript string `json:"transcript"`
				Words      []struct {
					Start float64 `json:"start"`
					End   float64 `json:"end"`
					Word  string  `json:"word"`
				} `json:"words"`
			} `json:"alternatives"`
		} `json:"channels"`
	} `json:"results"`
}

func (p *Provider) runRemoteTranscription(ctx context.Context, op *operation, req wire.TranscriptionRequest, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	transport, ok := op.host.RemoteTransport()
	if !ok {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "remote:transport")
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Extensions = cloneStrings(req.Extensions)
	if forwarded.Extensions == nil {
		forwarded.Extensions = map[string]string{}
	}
	forwarded.Extensions[ClaimExtension] = op.subject.Program
	client := wire.NewTranscriptionClient(transport.WithContext(ctx))
	admission, err := client.Start(forwarded)
	if err != nil {
		return transcriptionFailed(wire.ReplyOutcomeUnavailable, "remote:unreachable")
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return transcriptionFailed(outcome, "remote:"+admission.Reason)
	}
	cancelRemote := func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, _ = wire.NewTranscriptionClient(transport.WithContext(stop)).Cancel(admission.Operation)
	}
	cursor := int64(0)
	for {
		page, err := client.Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancelRemote()
			return transcriptionFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil {
			cancelRemote()
			return transcriptionFailed(wire.ReplyOutcomeUnavailable, "remote:unreachable")
		}
		if page.Outcome != wire.PageOutcomePage {
			cancelRemote()
			return transcriptionFailed(wire.ReplyOutcomeUnavailable, "remote:"+page.Outcome.String())
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindSegment && d.Segment != nil {
				emit(*d.Segment)
			}
			if d.Kind == wire.DeltaKindEnd && d.TranscriptionEnd != nil {
				return *d.TranscriptionEnd
			}
		}
		cursor = page.Next
	}
}

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
