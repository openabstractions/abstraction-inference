package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
	"net/url"
	"strings"
	"sync"
)

const LiveContract = "abstraction.inference/live@1"
const liveMediaType = "audio/pcm;rate=24000;channels=1;format=s16le"

type liveSession struct {
	mu               sync.Mutex
	ready            chan struct{}
	backend          liveBackend
	next, inputBytes int64
	last             [32]byte
	committed        bool
}

func validateLive(req wire.LiveRequest) string { return validateLiveFields(req, true) }

// validateLiveFields checks one live request. voice is false for the window's
// pre-upgrade preflight, which knows the model from the WebSocket URL and the
// voice only from the session.update that arrives after the upgrade.
func validateLiveFields(req wire.LiveRequest, voice bool) string {
	if req.Model == "" || len(req.Model) > 256 {
		return "model"
	}
	if voice && (req.Voice == "" || len(req.Voice) > 256 || !nameChars(req.Voice, "_-.")) {
		return "voice"
	}
	if !req.Format.Known() {
		return "format"
	}
	// Reuse the shared request routing vocabulary without embedding audio bytes.
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
	if req.Credential != "" && (len(req.Credential) > 64 || !nameChars(req.Credential, "_-.")) {
		return "credential"
	}
	for k := range req.Extensions {
		a, b, ok := strings.Cut(k, "/")
		if !ok || a == "" || b == "" || len(k) > 128 {
			return "extensions"
		}
	}
	return ""
}

// PreflightLive answers whether subject may open a live session for the model
// in req, deciding the same host routing, wire support and
// abstraction.inference/complete rule StartLive decides, and admitting nothing.
// The gateway window calls it before it completes a Realtime WebSocket
// upgrade, so a program without the rule is refused with an HTTP status
// instead of an error event on an open socket (CONTRACT.md INF-W8). Voice,
// budget, credential and capacity stay with StartLive, which the session.update
// that follows a successful upgrade reaches. Accepted carries the routed host
// and model and an empty Operation.
func (p *Provider) PreflightLive(ctx context.Context, subject Subject, req wire.LiveRequest) wire.Admission {
	started := p.cfg.Now()
	via := routeOf(ctx)
	refuse := func(outcome wire.StartOutcome, reason string, h *router.Host, model string) wire.Admission {
		r := Record{Route: via.name, Rung: via.rung, Domain: via.domain, Claim: via.claim, Family: familyOf(req.Model), Profile: router.ProfileLive, Account: subject.Account, Program: subject.Program, Model: req.Model, Credential: req.Credential, Outcome: outcome.String(), Reason: reason, WallMS: p.cfg.Now().Sub(started).Milliseconds()}
		if h != nil {
			r.Host = h.Name
			r.Model = model
			if h.Wire == router.WireRemote {
				r.Domain = h.Domain
			}
		}
		p.record(r)
		return refusal(outcome, reason)
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.StartOutcomeForbidden, "caller", nil, "")
	}
	if reason := validateLiveFields(req, false); reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason, nil, "")
	}
	host, model, reason := p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileLive)
	if host == nil {
		return refuse(wire.StartOutcomeNoHost, reason, nil, "")
	}
	if host.Wire != router.WireOpenAIRealtime && host.Wire != router.WireRemote {
		return refuse(wire.StartOutcomeUnsupportedFeature, "wire:"+host.Wire, host, model)
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	if word != "permitted" {
		if word == "denied" || word == "not_granted" || word == "unknown_action" {
			return refuse(wire.StartOutcomeNotPermitted, "rights:"+word, host, model)
		}
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	if host.Wire == router.WireOpenAIRealtime && p.cfg.LiveDialer == nil {
		return refuse(wire.StartOutcomeUnavailable, "upstream:unavailable", host, model)
	}
	a := wire.Admission{Outcome: wire.StartOutcomeAccepted, Host: host.Name, Model: model}
	// The window needs idle_ms before it upgrades, to bound a connection that
	// admits no session. Operation stays empty: nothing was admitted.
	p.bounds(&a)
	return a
}

func (p *Provider) StartLive(ctx context.Context, subject Subject, req wire.LiveRequest) wire.Admission {
	started := p.cfg.Now()
	via := routeOf(ctx)
	if via.name == RouteRemote {
		via.claim = req.Extensions[ClaimExtension]
	}
	refuse := func(outcome wire.StartOutcome, reason string, h *router.Host, model string) wire.Admission {
		r := Record{Route: via.name, Rung: via.rung, Domain: via.domain, Claim: via.claim, Family: familyOf(req.Model), Profile: router.ProfileLive, Account: subject.Account, Program: subject.Program, Model: req.Model, Credential: req.Credential, Outcome: outcome.String(), Reason: reason, WallMS: p.cfg.Now().Sub(started).Milliseconds()}
		if h != nil {
			r.Host = h.Name
			r.Model = model
			if h.Wire == router.WireRemote {
				r.Domain = h.Domain
			}
		}
		p.record(r)
		return refusal(outcome, reason)
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.StartOutcomeForbidden, "caller", nil, "")
	}
	if reason := validateLive(req); reason != "" {
		return refuse(wire.StartOutcomeInvalid, reason, nil, "")
	}
	host, model, reason := p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileLive)
	if host == nil {
		return refuse(wire.StartOutcomeNoHost, reason, nil, "")
	}
	if host.Wire != router.WireOpenAIRealtime && host.Wire != router.WireRemote {
		return refuse(wire.StartOutcomeUnsupportedFeature, "wire:"+host.Wire, host, model)
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	if word != "permitted" {
		if word == "denied" || word == "not_granted" || word == "unknown_action" {
			return refuse(wire.StartOutcomeNotPermitted, "rights:"+word, host, model)
		}
		return refuse(wire.StartOutcomeUnavailable, "rights:unavailable", host, model)
	}
	if host.Wire == router.WireOpenAIRealtime && p.cfg.LiveDialer == nil {
		return refuse(wire.StartOutcomeUnavailable, "upstream:unavailable", host, model)
	}
	remote := host.Wire == router.WireRemote
	var commit ContentCommitter
	if !remote {
		if p.cfg.PrepareContentWrite == nil {
			return refuse(wire.StartOutcomeUnavailable, "content:unavailable", host, model)
		}
		var outcome ContentOutcome
		commit, outcome = p.cfg.PrepareContentWrite(ctx, subject, "live", liveMediaType, p.cfg.MaxSpeechBytes)
		if outcome != ContentResolved || commit == nil {
			if outcome == ContentForbidden {
				return refuse(wire.StartOutcomeForbidden, "output:forbidden", host, model)
			}
			return refuse(wire.StartOutcomeUnavailable, "content:unavailable", host, model)
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
		headers, word = p.cfg.Apply(ctx, subject, LiveContract, credential, router.Target(host.Base))
		if word != "applied" {
			clear(headers)
			if word == "unavailable" || word == "" {
				return refuse(wire.StartOutcomeUnavailable, "credential:unavailable", host, model)
			}
			return refuse(wire.StartOutcomeNotPermitted, "credential:"+word, host, model)
		}
	}
	id, err := operationID()
	if err != nil {
		clear(headers)
		return refuse(wire.StartOutcomeUnavailable, "capacity", host, model)
	}
	op := newOperation(id, subject, host, model, credential, p.cfg.RetainedDeltas, started)
	op.profile, op.route, op.remote = router.ProfileLive, via, remote
	op.live = &liveSession{ready: make(chan struct{})}
	runCtx, cancel := context.WithCancel(p.ctx)
	op.cancel = cancel
	if outcome := p.admitOperation(op); outcome != wire.StartOutcomeAccepted {
		cancel()
		clear(headers)
		return refuse(outcome, "capacity", host, model)
	}
	go p.runLive(runCtx, op, req, headers, commit)
	a := wire.Admission{Outcome: wire.StartOutcomeAccepted, Operation: id, Host: host.Name, Model: model}
	p.bounds(&a)
	return a
}

func (p *Provider) ObserveLive(ctx context.Context, s Subject, id string, cursor, maxDeltas, maxBytes, waitMS int64) wire.DeltaPage {
	return p.observeProfile(ctx, s, id, router.ProfileLive, cursor, maxDeltas, maxBytes, waitMS)
}
func (p *Provider) CancelLive(s Subject, id string) wire.Cancellation {
	return p.cancelProfile(s, id, router.ProfileLive)
}

func (p *Provider) liveInput(ctx context.Context, s Subject, id string, sequence int64, audio []byte, commit bool) wire.LiveInputResult {
	result := func(o wire.LiveInputOutcome, next int64) wire.LiveInputResult {
		return wire.LiveInputResult{Outcome: o, NextSequence: next}
	}
	if s.Account == "" || s.Program == "" {
		return result(wire.LiveInputOutcomeForbidden, 0)
	}
	if id == "" || len(id) > 128 || (!commit && (sequence < 0 || len(audio) == 0 || len(audio) > 65536 || len(audio)%2 != 0)) {
		return result(wire.LiveInputOutcomeInvalid, 0)
	}
	op := p.visibleProfile(s, id, router.ProfileLive)
	if op == nil {
		return result(wire.LiveInputOutcomeUnknown, 0)
	}
	ls := op.live
	select {
	case <-ls.ready:
	case <-ctx.Done():
		return result(wire.LiveInputOutcomeUnavailable, 0)
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if commit && ls.committed {
		return result(wire.LiveInputOutcomeDuplicate, ls.next)
	}
	if op.isEnded() || op.stopped() != "" || ls.backend == nil || ls.committed {
		return result(wire.LiveInputOutcomeClosed, ls.next)
	}
	var err error
	if commit {
		if ls.inputBytes < 4800 {
			return result(wire.LiveInputOutcomeInvalid, ls.next)
		}
		err = ls.backend.commit(ctx)
		if err == nil {
			ls.committed = true
		}
	} else {
		sum := sha256.Sum256(audio)
		if sequence == ls.next-1 && ls.next > 0 && sum == ls.last {
			return result(wire.LiveInputOutcomeDuplicate, ls.next)
		}
		if sequence != ls.next {
			return result(wire.LiveInputOutcomeOutOfOrder, ls.next)
		}
		if int64(len(audio)) > p.cfg.MaxAudioBytes-ls.inputBytes {
			return result(wire.LiveInputOutcomeExhausted, ls.next)
		}
		// Count attempted bytes even if the send acknowledgement is uncertain.
		ls.inputBytes += int64(len(audio))
		err = ls.backend.append(ctx, audio)
		if err == nil {
			ls.next++
			ls.last = sum
		}
	}
	if err != nil {
		op.stop("upstream:write")
		return result(wire.LiveInputOutcomeUnavailable, ls.next)
	}
	op.mu.Lock()
	op.lastObserve = p.cfg.Now()
	op.mu.Unlock()
	return result(wire.LiveInputOutcomeAccepted, ls.next)
}
func (p *Provider) AppendLive(ctx context.Context, s Subject, id string, sequence int64, audio []byte) wire.LiveInputResult {
	return p.liveInput(ctx, s, id, sequence, audio, false)
}
func (p *Provider) CommitLive(ctx context.Context, s Subject, id string) wire.LiveInputResult {
	return p.liveInput(ctx, s, id, 0, nil, true)
}

func (p *Provider) runLive(ctx context.Context, op *operation, req wire.LiveRequest, headers map[string]string, commit ContentCommitter) {
	defer p.runs.Done()
	defer close(op.finished)
	defer op.cancel()
	end := wire.LiveReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "upstream:unavailable", Host: op.host.Name, Model: op.model}
	ls := op.live
	var output []byte
	defer func() {
		ls.mu.Lock()
		end.InputAudioBytes = ls.inputBytes
		ls.mu.Unlock()
		end.OutputAudioBytes = int64(len(output))
		if reason := op.stopped(); reason != "" {
			if reason == "upstream:write" {
				end.Outcome = wire.ReplyOutcomeUnavailable
			} else {
				end.Outcome = wire.ReplyOutcomeCancelled
			}
			end.Reason = reason
			end.Delivery = wire.Delivery{}
		}
		if ctx.Err() != nil && end.Outcome == wire.ReplyOutcomeUnavailable && end.Reason != "upstream:write" {
			end.Outcome = wire.ReplyOutcomeCancelled
			end.Reason = "caller"
		}
		seconds := (end.InputAudioBytes + end.OutputAudioBytes + 47999) / 48000
		if op.credential != "" && p.cfg.Ceilings != nil && !op.remote {
			if err := p.cfg.Ceilings.AddUsage(op.credential, end.Usage.Input+end.Usage.Output, 0, seconds); err != nil {
				p.report(err)
			}
		}
		now := p.cfg.Now()
		op.emit(wire.Delta{Kind: wire.DeltaKindEnd, LiveEnd: &end}, now)
		domain := op.route.domain
		if op.remote {
			domain = op.host.Domain
		}
		p.record(Record{Route: op.route.name, Rung: op.route.rung, Domain: domain, Claim: op.route.claim, Family: familyOf(op.model), Operation: op.id, Profile: router.ProfileLive, Account: op.subject.Account, Program: op.subject.Program, Host: op.host.Name, Model: op.model, Credential: op.credential, Outcome: end.Outcome.String(), Reason: end.Reason, TokensIn: end.Usage.Input, TokensOut: end.Usage.Output, AudioSeconds: seconds, WallMS: now.Sub(op.started).Milliseconds()})
	}()
	var backend liveBackend
	var err error
	if op.remote {
		backend, err = dialRemoteLiveBackend(ctx, op, req)
	} else {
		var address *url.URL
		address, err = url.Parse(strings.TrimRight(op.host.Base, "/") + "/realtime")
		if err == nil {
			switch address.Scheme {
			case "https":
				address.Scheme = "wss"
			case "http":
				address.Scheme = "ws"
			default:
				err = errors.New("unsupported live URL scheme")
			}
		}
		if err == nil {
			if p.cfg.LiveDialer == nil {
				err = liveError(liveBackendUnavailable)
			} else {
				// The credential-bearing HTTP policy stays in core. Adapters receive
				// only this guarded client for the session handshake.
				connection, dialErr := p.cfg.LiveDialer(ctx, credentialRequestClient(p.cfg.HTTP, headers), address.String(), op.model, req.Voice, req.Format, headers)
				err = dialErr
				if err != nil && connection != nil {
					connection.Close()
				} else if connection != nil {
					backend = localLiveBackend{connection: connection}
				} else if err == nil {
					err = liveError(liveBackendUnavailable)
				}
			}
		}
	}
	clear(headers)
	ls.mu.Lock()
	ls.backend = backend
	close(ls.ready)
	ls.mu.Unlock()
	if err != nil {
		end.Outcome, end.Reason = liveFailure(err)
		return
	}
	defer backend.close()
	stop := context.AfterFunc(ctx, backend.close)
	defer stop()
	for index := int64(0); ; {
		event, err := backend.read(ctx)
		if err != nil {
			end.Outcome, end.Reason = liveFailure(err)
			return
		}
		switch event.kind {
		case liveBackendEventAudio:
			if len(event.audio) == 0 || len(event.audio)%2 != 0 {
				end.Reason = "upstream:malformed"
				return
			}
			if int64(len(event.audio)) > p.cfg.MaxSpeechBytes-int64(len(output)) {
				end.Reason = "content:too-large"
				return
			}
			output = append(output, event.audio...)
			op.emit(wire.Delta{Kind: wire.DeltaKindAudio, Audio: &wire.AudioChunk{Index: index, Data: append([]byte(nil), event.audio...)}}, p.cfg.Now())
			index++
		case liveBackendEventTranscript:
			op.emit(wire.Delta{Kind: wire.DeltaKindTranscript, Transcript: &wire.LiveTranscript{Text: event.transcript, IsFinal: event.final}}, p.cfg.Now())
		case liveBackendEventTerminal:
			if op.remote {
				remoteEnd := event.remoteEnd
				if remoteEnd == nil {
					end.Reason = "remote:malformed"
					return
				}
				end = *remoteEnd
				if end.Outcome == wire.ReplyOutcomeCompleted {
					ls.mu.Lock()
					inputBytes := ls.inputBytes
					ls.mu.Unlock()
					if len(output) == 0 || end.InputAudioBytes != inputBytes || end.OutputAudioBytes != int64(len(output)) || end.Delivery.Size != int64(len(output)) || end.Delivery.MediaType != liveMediaType || end.Delivery.Digest != liveAudioDigest(output) {
						end = wire.LiveReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "remote:delivery", Host: op.host.Name, Model: op.model}
						return
					}
					if !op.beginCompletion() {
						end = wire.LiveReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "cancel", Host: op.host.Name, Model: op.model}
						return
					}
					end.Delivery.Location = op.host.Domain
				}
				return
			}
			end.Usage = wire.Usage{Input: event.usage.inputTokens, Output: event.usage.outputTokens}
			if event.outcome != "completed" {
				end.Outcome = wire.ReplyOutcomeRefused
				end.Reason = "upstream:" + event.outcome
				return
			}
			if len(output) == 0 {
				end.Reason = "upstream:empty-audio"
				return
			}
			sum := sha256.Sum256(output)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			if !op.beginCompletion() {
				end.Outcome = wire.ReplyOutcomeCancelled
				end.Reason = "cancel"
				return
			}
			switch commit(ctx, digest, output) {
			case ContentResolved:
			case ContentForbidden:
				end.Outcome = wire.ReplyOutcomeForbidden
				end.Reason = "output:forbidden"
				return
			default:
				end.Reason = "content:unavailable"
				return
			}
			end.Outcome = wire.ReplyOutcomeCompleted
			end.Reason = ""
			end.Delivery = wire.Delivery{Digest: digest, MediaType: liveMediaType, Size: int64(len(output)), Location: "local"}
			return
		}
	}
}
func liveFailure(err error) (wire.ReplyOutcome, string) {
	var remote *remoteLiveError
	if errors.As(err, &remote) {
		return remote.outcome, remote.reason
	}
	var e *liveBackendError
	if errors.As(err, &e) {
		if e.code == liveBackendRefused {
			return wire.ReplyOutcomeRefused, "upstream:refused"
		}
		return wire.ReplyOutcomeUnavailable, "upstream:" + string(e.code)
	}
	return wire.ReplyOutcomeUnavailable, "upstream:unavailable"
}

func liveAudioDigest(audio []byte) string {
	sum := sha256.Sum256(audio)
	return "sha256:" + hex.EncodeToString(sum[:])
}
