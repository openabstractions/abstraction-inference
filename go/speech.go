package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const SpeechContract = "abstraction.inference/speech@1"

func speechFormatWord(format wire.SpeechFormat) string { return format.String() }

func speechMediaType(format wire.SpeechFormat) string {
	return map[wire.SpeechFormat]string{
		wire.SpeechFormatMp3: "audio/mpeg", wire.SpeechFormatOpus: "audio/ogg", wire.SpeechFormatAac: "audio/aac",
		wire.SpeechFormatFlac: "audio/flac", wire.SpeechFormatWav: "audio/wav", wire.SpeechFormatPcm: "audio/pcm",
	}[format]
}

func validateSpeech(req wire.SpeechRequest) (int64, string) {
	if req.Model == "" || len(req.Model) > 256 {
		return 0, "model"
	}
	if req.Voice == "" || len(req.Voice) > 256 || !nameChars(req.Voice, "_-.:") {
		return 0, "voice"
	}
	if !utf8.ValidString(req.Text) || len(req.Text) == 0 || len(req.Text) > 16384 {
		return 0, "text"
	}
	characters := int64(utf8.RuneCountInString(req.Text))
	if characters < 1 || characters > 4096 {
		return 0, "text"
	}
	if !req.Format.Known() {
		return 0, "format"
	}
	for key := range req.Extensions {
		owner, name, ok := strings.Cut(key, "/")
		if !ok || owner == "" || name == "" || len(key) > 128 {
			return 0, "extensions"
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
			return 0, "guarantees"
		}
	}
	if local && hosted {
		return 0, "guarantees"
	}
	if c := req.Credential; c != "" && (len(c) > 64 || !nameChars(c, "_-.")) {
		return 0, "credential"
	}
	return characters, ""
}

func speechAdapterFor(host *router.Host) string {
	switch {
	case host.Wire == router.WireRemote:
		return "remote"
	case host.Wire == router.WireElevenLabsStream:
		return "elevenlabs"
	case host.Name == "piper":
		return "piper"
	case host.Wire == "" || host.Wire == router.WireOpenAICompatible:
		return "openai"
	}
	return ""
}

func (p *Provider) StartSpeech(ctx context.Context, subject Subject, req wire.SpeechRequest) wire.Admission {
	started, via := p.cfg.Now(), routeOf(ctx)
	if via.name == RouteRemote {
		via.claim = req.Extensions[ClaimExtension]
	}
	refuse := func(outcome wire.StartOutcome, reason string, host *router.Host, model string) wire.Admission {
		r := Record{Profile: router.ProfileSpeech, Route: via.name, Rung: via.rung, Account: subject.Account, Program: subject.Program, Model: req.Model,
			Family: familyOf(req.Model), Credential: req.Credential, Outcome: outcome.String(), Reason: reason, WallMS: p.cfg.Now().Sub(started).Milliseconds(), Domain: via.domain, Claim: via.claim}
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
	characters, reason := validateSpeech(req)
	if reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason, nil, "")
	}
	host, model, reason := p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileSpeech)
	if host == nil {
		return refuse(wire.StartOutcomeNoHost, reason, nil, "")
	}
	kind := speechAdapterFor(host)
	if kind == "" || host.SpeechURL() == "" {
		return refuse(wire.StartOutcomeUnsupportedFeature, "wire:"+host.Wire, host, model)
	}
	if !host.SupportsSpeechFormat(speechFormatWord(req.Format)) {
		return refuse(wire.StartOutcomeUnsupportedFeature, "format:"+speechFormatWord(req.Format), host, model)
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
	var commit ContentCommitter
	if !remote {
		if p.cfg.PrepareContentWrite == nil {
			return refuse(wire.StartOutcomeUnavailable, "output:unavailable", host, model)
		}
		var outcome ContentOutcome
		commit, outcome = p.cfg.PrepareContentWrite(ctx, subject, "speech", speechMediaType(req.Format), p.cfg.MaxSpeechBytes)
		switch outcome {
		case ContentResolved:
		case ContentForbidden:
			return refuse(wire.StartOutcomeForbidden, "output:forbidden", host, model)
		default:
			return refuse(wire.StartOutcomeUnavailable, "output:unavailable", host, model)
		}
		if commit == nil {
			return refuse(wire.StartOutcomeUnavailable, "output:unavailable", host, model)
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
	// Avoid credential work for an operation that is already known to exceed
	// capacity. admitOperation repeats this check atomically with insertion.
	if p.active(subject) >= p.cfg.MaxOperations {
		return refuse(wire.StartOutcomeExhausted, "capacity", host, model)
	}
	var headers map[string]string
	if credential != "" && !remote {
		headers, word = p.cfg.Apply(ctx, subject, SpeechContract, credential, router.Target(host.Base))
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
	op.profile, op.route, op.remote = router.ProfileSpeech, via, remote
	runCtx, cancel := context.WithCancel(p.ctx)
	op.cancel = cancel
	if outcome := p.admitOperation(op); outcome != wire.StartOutcomeAccepted {
		cancel()
		clear(headers)
		reason = "capacity"
		if outcome == wire.StartOutcomeUnavailable {
			reason = "provider:closed"
		}
		return refuse(outcome, reason, host, model)
	}
	go p.runSpeech(runCtx, op, kind, req, characters, headers, commit)
	a := wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: id, Host: host.Name, Model: model}
	p.bounds(&a)
	return a
}

func (p *Provider) ObserveSpeech(ctx context.Context, subject Subject, id string, cursor, maxDeltas, maxBytes, waitMS int64) wire.DeltaPage {
	return p.observeProfile(ctx, subject, id, router.ProfileSpeech, cursor, maxDeltas, maxBytes, waitMS)
}
func (p *Provider) CancelSpeech(subject Subject, id string) wire.Cancellation {
	return p.cancelProfile(subject, id, router.ProfileSpeech)
}

func (p *Provider) runSpeech(ctx context.Context, op *operation, kind string, req wire.SpeechRequest, characters int64, headers map[string]string, commit ContentCommitter) {
	defer p.runs.Done()
	defer close(op.finished)
	defer op.cancel()
	index := int64(0)
	emit := func(chunk []byte) {
		copied := append([]byte(nil), chunk...)
		op.emit(wire.Delta{Kind: wire.DeltaKindAudio, Audio: &wire.AudioChunk{Index: index, Data: copied}}, p.cfg.Now())
		index++
	}
	var end wire.SpeechReply
	if kind == "remote" {
		end = p.runRemoteSpeech(ctx, op, req, emit)
	} else {
		end = runSpeechHTTP(ctx, p.cfg.HTTP, op.host, op.model, kind, req, headers, p.cfg.MaxSpeechBytes, commit, op.beginCompletion, emit)
	}
	clear(headers)
	if reason := op.stopped(); reason != "" {
		end.Outcome, end.Reason = wire.ReplyOutcomeCancelled, reason
	} else if p.ctx.Err() != nil || ctx.Err() != nil && end.Outcome == wire.ReplyOutcomeUnavailable {
		end.Outcome, end.Reason = wire.ReplyOutcomeCancelled, "caller"
	}
	end.Host, end.Model, end.Characters = op.host.Name, op.model, characters
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		var cost int64
		if end.Cost != nil {
			cost = end.Cost.Micros
		}
		if err := p.cfg.Ceilings.AddUnits(op.credential, end.Usage.Input+end.Usage.Output, cost, 0, characters); err != nil {
			p.report(err)
		}
	}
	now := p.cfg.Now()
	op.emit(wire.Delta{Kind: wire.DeltaKindEnd, SpeechEnd: &end}, now)
	r := Record{Operation: op.id, Profile: router.ProfileSpeech, Route: op.route.name, Rung: op.route.rung, Account: op.subject.Account, Program: op.subject.Program,
		Host: op.host.Name, Model: op.model, Family: familyOf(op.model), Credential: op.credential, Outcome: end.Outcome.String(), Reason: end.Reason, Characters: characters,
		WallMS: now.Sub(op.started).Milliseconds(), Domain: op.route.domain, Claim: op.route.claim}
	if op.remote {
		r.Domain = op.host.Domain
	}
	if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
		r.Ceiling = p.cfg.Ceilings.State(op.credential)
	}
	p.record(r)
}

func emptyDelivery() wire.Delivery { return wire.Delivery{} }
func speechFailed(outcome wire.ReplyOutcome, reason string) wire.SpeechReply {
	return wire.SpeechReply{Outcome: outcome, Reason: reason, Delivery: emptyDelivery()}
}

func runSpeechHTTP(ctx context.Context, client *http.Client, host *router.Host, model, kind string, input wire.SpeechRequest, headers map[string]string, maxBytes int64, commit ContentCommitter, beginCompletion func() bool, emit func([]byte)) wire.SpeechReply {
	address := host.SpeechURL()
	body := map[string]any{"text": input.Text}
	if kind == "openai" {
		body = map[string]any{"model": model, "voice": input.Voice, "input": input.Text, "response_format": speechFormatWord(input.Format)}
	}
	if kind == "piper" {
		body["voice"] = input.Voice
	}
	if kind == "elevenlabs" {
		address += "/" + url.PathEscape(input.Voice) + "/stream"
		u, err := url.Parse(address)
		if err != nil {
			return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:address")
		}
		q := u.Query()
		q.Set("output_format", elevenFormat(input.Format))
		u.RawQuery = q.Encode()
		address = u.String()
		body = map[string]any{"text": input.Text, "model_id": model}
	}
	//unchecked: body above holds only string values; json.Marshal on plain data types cannot fail
	raw, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:address")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := credentialRequestClient(client, headers).Do(httpReq)
	if err != nil {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		//unchecked: best-effort drain for connection reuse; the outcome is already decided by the status code below
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode == 400 || resp.StatusCode == 404 || resp.StatusCode == 422 {
			return speechFailed(wire.ReplyOutcomeUnsupportedFeature, "voice:"+input.Voice)
		}
		outcome := wire.ReplyOutcomeRefused
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return speechFailed(outcome, fmt.Sprintf("upstream:%d", resp.StatusCode))
	}
	expected := speechMediaType(input.Format)
	//unchecked: a parse failure yields an empty contentType, which the following check already treats the same as a missing header (skip the media-type assertion) by design
	if contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); contentType != "" && contentType != "application/octet-stream" && contentType != expected {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:media-type")
	}
	var whole bytes.Buffer
	// Keep the compact-JSON delta under Observe's 64 KiB page budget after
	// base64 expansion while remaining below the protocol's 64 KiB audio cap.
	buf := make([]byte, 47*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if int64(whole.Len()+n) > maxBytes {
				return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:too-large")
			}
			//unchecked: bytes.Buffer.Write never returns a non-nil error
			_, _ = whole.Write(buf[:n])
			emit(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:read")
		}
	}
	if whole.Len() == 0 {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:empty")
	}
	if input.Format == wire.SpeechFormatWav && (whole.Len() < 12 || string(whole.Bytes()[:4]) != "RIFF" || string(whole.Bytes()[8:12]) != "WAVE") {
		return speechFailed(wire.ReplyOutcomeUnavailable, "upstream:media-type")
	}
	sum := sha256.Sum256(whole.Bytes())
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if !beginCompletion() {
		return speechFailed(wire.ReplyOutcomeCancelled, "cancel")
	}
	switch outcome := commit(ctx, digest, whole.Bytes()); outcome {
	case ContentResolved:
	case ContentForbidden:
		return speechFailed(wire.ReplyOutcomeForbidden, "output:forbidden")
	default:
		return speechFailed(wire.ReplyOutcomeUnavailable, "output:unavailable")
	}
	return wire.SpeechReply{Outcome: wire.ReplyOutcomeCompleted, Delivery: wire.Delivery{Digest: digest, MediaType: expected, Size: int64(whole.Len()), Location: "local"}}
}

func elevenFormat(format wire.SpeechFormat) string {
	switch format {
	case wire.SpeechFormatOpus:
		return "opus_48000_32"
	case wire.SpeechFormatPcm:
		return "pcm_24000"
	default:
		return "mp3_44100_128"
	}
}

func (p *Provider) runRemoteSpeech(ctx context.Context, op *operation, req wire.SpeechRequest, emit func([]byte)) wire.SpeechReply {
	transport, ok := op.host.RemoteTransport()
	if !ok {
		return speechFailed(wire.ReplyOutcomeUnavailable, "remote:transport")
	}
	forwarded := req
	forwarded.Model = op.model
	forwarded.Extensions = cloneStrings(req.Extensions)
	if forwarded.Extensions == nil {
		forwarded.Extensions = map[string]string{}
	}
	forwarded.Extensions[ClaimExtension] = op.subject.Program
	client := wire.NewSpeechClient(transport.WithContext(ctx))
	admission, err := client.Start(forwarded)
	if err != nil {
		return speechFailed(wire.ReplyOutcomeUnavailable, "remote:unreachable")
	}
	if admission.Outcome != wire.StartOutcomeAccepted {
		outcome := replyFromStart(admission.Outcome)
		if !outcome.Known() {
			outcome = wire.ReplyOutcomeUnavailable
		}
		return speechFailed(outcome, "remote:"+admission.Reason)
	}
	cancelRemote := func() {
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		//unchecked: best-effort cancel in a cleanup closure; the caller already has its own error and nothing left to receive a second one
		_, _ = wire.NewSpeechClient(transport.WithContext(stop)).Cancel(admission.Operation)
	}
	remoteAudio := sha256.New()
	var remoteSize int64
	for cursor := int64(0); ; {
		page, err := client.Observe(admission.Operation, cursor, 256, 65536, remoteObserveWaitMS)
		if ctx.Err() != nil {
			cancelRemote()
			return speechFailed(wire.ReplyOutcomeCancelled, "caller")
		}
		if err != nil {
			cancelRemote()
			return speechFailed(wire.ReplyOutcomeUnavailable, "remote:unreachable")
		}
		if page.Outcome != wire.PageOutcomePage {
			cancelRemote()
			return speechFailed(wire.ReplyOutcomeUnavailable, "remote:"+page.Outcome.String())
		}
		for _, d := range page.Deltas {
			if d.Kind == wire.DeltaKindAudio && d.Audio != nil {
				if len(d.Audio.Data) == 0 || len(d.Audio.Data) > 65536 || remoteSize+int64(len(d.Audio.Data)) > p.cfg.MaxSpeechBytes {
					cancelRemote()
					return speechFailed(wire.ReplyOutcomeUnavailable, "remote:audio")
				}
				remoteSize += int64(len(d.Audio.Data))
				//unchecked: hash.Hash.Write is documented to never return a non-nil error
				_, _ = remoteAudio.Write(d.Audio.Data)
				emit(d.Audio.Data)
			}
			if d.Kind == wire.DeltaKindEnd && d.SpeechEnd != nil {
				end := *d.SpeechEnd
				if end.Outcome == wire.ReplyOutcomeCompleted {
					wantDigest := "sha256:" + hex.EncodeToString(remoteAudio.Sum(nil))
					if remoteSize == 0 || end.Delivery.Size != remoteSize || end.Delivery.Digest != wantDigest || end.Delivery.MediaType != speechMediaType(req.Format) {
						cancelRemote()
						return speechFailed(wire.ReplyOutcomeUnavailable, "remote:delivery")
					}
					if !op.beginCompletion() {
						cancelRemote()
						return speechFailed(wire.ReplyOutcomeCancelled, "cancel")
					}
					end.Delivery.Location = op.host.Domain
				}
				return end
			}
		}
		cursor = page.Next
	}
}
