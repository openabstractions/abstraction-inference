package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

const (
	maxRealtimeAudioBytes      = 64 << 10
	maxRealtimeTranscriptBytes = 1 << 20
	realtimeAuthorityInterval  = 500 * time.Millisecond
	// realtimeFallbackBudget bounds a connection whose preflight reported no
	// idle_ms. It is the provider's own default idle.
	realtimeFallbackBudget = 10 * time.Second
)

type liveWindowSession struct {
	conn     *websocket.Conn
	provider LiveProvider
	subject  inference.Subject
	cancel   context.CancelFunc

	mu        sync.Mutex
	operation string
	ended     bool
	stopOnce  sync.Once
}

func (s *liveWindowSession) setOperation(operation string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.operation = operation
	return true
}

func (s *liveWindowSession) finish() {
	s.mu.Lock()
	s.ended = true
	s.operation = ""
	s.mu.Unlock()
}

func (s *liveWindowSession) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		operation, ended := s.operation, s.ended
		s.ended = true
		s.operation = ""
		s.mu.Unlock()
		//unchecked: best-effort close in a sync.Once teardown helper that returns nothing; no caller left to report the failure to
		_ = s.conn.CloseNow()
		if operation != "" && !ended {
			s.provider.CancelLive(s.subject, operation)
		}
	})
}

func (w *Window) registerLive(session *liveWindowSession) bool {
	w.liveMu.Lock()
	defer w.liveMu.Unlock()
	if w.closed {
		return false
	}
	if w.live == nil {
		w.live = map[*liveWindowSession]struct{}{}
	}
	w.live[session] = struct{}{}
	return true
}

func (w *Window) unregisterLive(session *liveWindowSession) {
	w.liveMu.Lock()
	delete(w.live, session)
	w.liveMu.Unlock()
}

type realtimeFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type realtimeSession struct {
	Type             string   `json:"type"`
	Model            string   `json:"model"`
	OutputModalities []string `json:"output_modalities"`
	Audio            struct {
		Input struct {
			Format        realtimeFormat  `json:"format"`
			TurnDetection json.RawMessage `json:"turn_detection"`
		} `json:"input"`
		Output struct {
			Format realtimeFormat `json:"format"`
			Voice  string         `json:"voice"`
		} `json:"output"`
	} `json:"audio"`
}

type realtimeSessionUpdate struct {
	Type    string          `json:"type"`
	EventID string          `json:"event_id,omitempty"`
	Session realtimeSession `json:"session"`
}

type realtimeAppend struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
	Audio   string `json:"audio"`
}

type realtimeControl struct {
	Type    string `json:"type"`
	EventID string `json:"event_id,omitempty"`
}

type realtimeInbound struct {
	payload []byte
	err     error
}

type liveObserved struct {
	delta *wire.Delta
	err   error
}

// liveGuarantees puts the key's grant on a live request: a key issued with a
// credential name spends under it, a key without one stays local (INF-W5).
func liveGuarantees(req *wire.LiveRequest, grant Grant) {
	if grant.Credential != "" {
		req.Guarantees, req.Credential = []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, grant.Credential
		return
	}
	req.Guarantees = []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}
}

func (w *Window) openAIRealtime(rw http.ResponseWriter, r *http.Request, bound Bound, grant Grant) {
	provider := w.cfg.Chat.(LiveProvider)
	key, _ := r.Context().Value(verifiedKeyContext{}).(string)
	subject := bound.Subject()
	// The model names the host, and the host is what complete is decided on.
	// The Realtime wire carries it in the URL, so the decision is made here,
	// with an HTTP status still available, rather than on an upgraded socket.
	model := r.URL.Query().Get("model")
	if model == "" || len(model) > 256 {
		rw.Header().Set("Connection", "close")
		w.record(inference.Record{Rung: bound.Rung(), Account: subject.Account, Program: subject.Program, Model: model, Outcome: "invalid", Reason: "window:model"})
		writeError(rw, http.StatusBadRequest, "invalid", "GET /v1/realtime names its model in the query: /v1/realtime?model=<name>")
		return
	}
	preflight := wire.LiveRequest{Model: model, Format: wire.LiveFormatPcm1624000}
	liveGuarantees(&preflight, grant)
	routed := inference.WithRoute(r.Context(), inference.RouteWindow, bound.Rung())
	decision := provider.PreflightLive(routed, subject, preflight)
	if decision.Outcome != wire.StartOutcomeAccepted {
		word := decision.Outcome.String()
		rw.Header().Set("Connection", "close")
		writeError(rw, status(word), word, refusalMessage(word, decision.Reason))
		return
	}
	conn, err := websocket.Accept(rw, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(MaxBodyBytes)
	ctx, cancel := context.WithCancel(r.Context())
	session := &liveWindowSession{conn: conn, provider: provider, subject: bound.Subject(), cancel: cancel}
	if !w.registerLive(session) {
		session.stop()
		return
	}
	defer func() {
		session.stop()
		w.unregisterLive(session)
	}()

	eventSequence := int64(0)
	send := func(value any) bool {
		if event, ok := value.(map[string]any); ok {
			eventSequence++
			event["event_id"] = fmt.Sprintf("event_gateway_%d", eventSequence)
		}
		payload, err := json.Marshal(value)
		if err != nil || conn.Write(ctx, websocket.MessageText, payload) != nil {
			return false
		}
		return true
	}
	fail := func(code, message string) {
		_ = send(map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "code": code, "message": message}})
	}
	if !send(map[string]any{"type": "session.created", "session": map[string]any{"type": "realtime"}}) {
		return
	}

	inbound := make(chan realtimeInbound, 1)
	go func() {
		for {
			typ, payload, err := conn.Read(ctx)
			if err == nil && typ != websocket.MessageText {
				err = errors.New("non-text realtime message")
			}
			select {
			case inbound <- realtimeInbound{payload: payload, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	authority := time.NewTicker(realtimeAuthorityInterval)
	defer authority.Stop()
	checkAuthority := func() bool {
		if err := bound.Recheck(); err != nil {
			fail("forbidden", "the connection no longer answers for the program that opened it")
			return false
		}
		current, word := w.cfg.Keys(ctx, bound.Subject(), key)
		if word != KeyVerified || current.Credential != grant.Credential {
			fail("forbidden", "the local key is no longer authorized for this session")
			return false
		}
		return true
	}

	// The wait budget is the contract's own idle rule (INF-Q3), which the
	// preflight reports before the upgrade: a connection that says nothing
	// and receives nothing for idle_ms is closed and its session cancelled,
	// the same outcome the provider gives an operation nobody observes.
	budget := time.Duration(decision.IdleMs) * time.Millisecond
	if budget <= 0 {
		budget = realtimeFallbackBudget
	}
	waited := time.NewTimer(budget)
	defer waited.Stop()
	spend := func() {
		if !waited.Stop() {
			select {
			case <-waited.C:
			default:
			}
		}
		waited.Reset(budget)
	}

	configured, inputClosed, responseStarted := false, false, false
	sequence := int64(0)
	var operation, responseID, itemID string
	var observed <-chan liveObserved
	transcript := make([]byte, 0, 1024)
	transcriptDone := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-waited.C:
			if responseStarted {
				// The turn is under way. From here the provider's own idle
				// rule and the terminal delta bound the wait, and the window
				// is observing continuously.
				continue
			}
			fail("cancelled", "the connection sent no client event within the window's wait budget")
			return
		case <-authority.C:
			if !checkAuthority() {
				return
			}
		case message, ok := <-inbound:
			if !ok {
				return
			}
			if message.err != nil {
				return
			}
			if !checkAuthority() {
				return
			}
			spend()
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(message.payload, &envelope) != nil || envelope.Type == "" {
				fail("invalid_event", "the client event is not a supported JSON object")
				return
			}
			switch envelope.Type {
			case "session.update":
				if configured || responseStarted {
					fail("invalid_state", "session.update is accepted once before audio")
					return
				}
				var update realtimeSessionUpdate
				if decodeRealtime(message.payload, &update) != nil || !supportedRealtimeSession(update.Session, model) {
					fail("unsupported_session", "the window supports realtime audio/pcm at 24000 Hz, manual turn detection, audio output and the model named in the connection URL")
					return
				}
				req := wire.LiveRequest{Model: model, Voice: update.Session.Audio.Output.Voice, Format: wire.LiveFormatPcm1624000}
				liveGuarantees(&req, grant)
				admission := provider.StartLive(inference.WithRoute(ctx, inference.RouteWindow, bound.Rung()), bound.Subject(), req)
				if admission.Outcome != wire.StartOutcomeAccepted {
					fail(admission.Outcome.String(), refusalMessage(admission.Outcome.String(), admission.Reason))
					return
				}
				operation = admission.Operation
				if !session.setOperation(operation) {
					provider.CancelLive(bound.Subject(), operation)
					return
				}
				configured = true
				if !send(map[string]any{"type": "session.updated", "session": update.Session}) {
					return
				}
			case "input_audio_buffer.append":
				if !configured || inputClosed || responseStarted {
					fail("invalid_state", "audio can be appended after session.update and before input commit")
					return
				}
				var appendEvent realtimeAppend
				if decodeRealtime(message.payload, &appendEvent) != nil || appendEvent.Audio == "" || len(appendEvent.Audio) > base64.StdEncoding.EncodedLen(maxRealtimeAudioBytes) {
					fail("invalid_audio", "audio must be one base64 PCM16 chunk of at most 65536 bytes")
					return
				}
				audio, err := base64.StdEncoding.Strict().DecodeString(appendEvent.Audio)
				if err != nil || len(audio) == 0 || len(audio) > maxRealtimeAudioBytes || len(audio)%2 != 0 {
					fail("invalid_audio", "audio must be one base64 PCM16 chunk of at most 65536 bytes")
					return
				}
				result := provider.AppendLive(ctx, bound.Subject(), operation, sequence, audio)
				if result.Outcome != wire.LiveInputOutcomeAccepted || result.NextSequence != sequence+1 {
					fail(result.Outcome.String(), "the live input could not be appended")
					return
				}
				sequence = result.NextSequence
			case "input_audio_buffer.commit":
				var control realtimeControl
				if decodeRealtime(message.payload, &control) != nil || !configured || inputClosed || responseStarted {
					fail("invalid_state", "input_audio_buffer.commit closes configured input once")
					return
				}
				inputClosed = true
				itemID = "item_" + operation
				if !send(map[string]any{"type": "input_audio_buffer.committed", "item_id": itemID}) {
					return
				}
			case "response.create":
				var control realtimeControl
				if decodeRealtime(message.payload, &control) != nil || !configured || !inputClosed || responseStarted {
					fail("invalid_state", "response.create is accepted once after input_audio_buffer.commit")
					return
				}
				result := provider.CommitLive(ctx, bound.Subject(), operation)
				if result.Outcome != wire.LiveInputOutcomeAccepted || result.NextSequence != sequence {
					fail(result.Outcome.String(), "the live response could not be started")
					return
				}
				responseStarted = true
				responseID = "resp_" + operation
				if !send(map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "realtime.response", "status": "in_progress"}}) {
					return
				}
				observed = observeLiveWindow(ctx, provider, bound.Subject(), operation)
			default:
				fail("unsupported_event", "the single-turn window does not support this client event")
				return
			}
		case result, ok := <-observed:
			if !ok {
				fail("unavailable", "the live operation ended without a terminal reply")
				return
			}
			if result.err != nil {
				fail("unavailable", "the live operation could not be observed")
				return
			}
			delta := result.delta
			switch {
			case delta.Audio != nil:
				if len(delta.Audio.Data) == 0 || len(delta.Audio.Data) > maxRealtimeAudioBytes {
					fail("unavailable", "the live operation returned an invalid audio fragment")
					return
				}
				if !send(map[string]any{"type": "response.output_audio.delta", "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "delta": base64.StdEncoding.EncodeToString(delta.Audio.Data)}) {
					return
				}
			case delta.Transcript != nil:
				if !utf8.ValidString(delta.Transcript.Text) || len(delta.Transcript.Text) > 16384 {
					fail("unavailable", "the live operation returned an invalid transcript fragment")
					return
				}
				if delta.Transcript.Text != "" {
					if len(delta.Transcript.Text) > maxRealtimeTranscriptBytes-len(transcript) {
						fail("too_large", "the output transcript exceeded the window limit")
						return
					}
					transcript = append(transcript, delta.Transcript.Text...)
					if !send(map[string]any{"type": "response.output_audio_transcript.delta", "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta.Transcript.Text}) {
						return
					}
				}
				if delta.Transcript.IsFinal && !transcriptDone {
					transcriptDone = true
					if !send(map[string]any{"type": "response.output_audio_transcript.done", "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "transcript": string(transcript)}) {
						return
					}
				}
			case delta.LiveEnd != nil:
				end := delta.LiveEnd
				if !transcriptDone {
					if !send(map[string]any{"type": "response.output_audio_transcript.done", "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0, "transcript": string(transcript)}) {
						return
					}
				}
				if !send(map[string]any{"type": "response.output_audio.done", "response_id": responseID, "item_id": itemID, "output_index": 0, "content_index": 0}) {
					return
				}
				statusWord := "failed"
				if end.Outcome == wire.ReplyOutcomeCompleted {
					statusWord = "completed"
				} else if end.Outcome == wire.ReplyOutcomeCancelled {
					statusWord = "cancelled"
				}
				response := map[string]any{"id": responseID, "object": "realtime.response", "status": statusWord,
					"usage": map[string]any{"input_tokens": end.Usage.Input, "output_tokens": end.Usage.Output, "total_tokens": end.Usage.Input + end.Usage.Output}}
				if statusWord != "completed" {
					response["status_details"] = map[string]any{"type": end.Outcome.String(), "reason": end.Reason}
				}
				if !send(map[string]any{"type": "response.done", "response": response}) {
					return
				}
				session.finish()
				//unchecked: best-effort close after the session has already sent its final message; the handler returns nothing and has no caller left to report the failure to
				_ = conn.Close(websocket.StatusNormalClosure, statusWord)
				return
			}
		}
	}
}

// supportedRealtimeSession accepts the session shape the window maps onto
// live@1. The model is fixed by the connection URL, which the complete
// decision was made against; a session.update may repeat it or leave it out,
// and may not name another one.
func supportedRealtimeSession(session realtimeSession, model string) bool {
	return session.Type == "realtime" && (session.Model == "" || session.Model == model) && session.Audio.Output.Voice != "" && len(session.Audio.Output.Voice) <= 256 &&
		len(session.OutputModalities) == 1 && session.OutputModalities[0] == "audio" &&
		session.Audio.Input.Format == (realtimeFormat{Type: "audio/pcm", Rate: 24000}) && bytes.Equal(bytes.TrimSpace(session.Audio.Input.TurnDetection), []byte("null")) &&
		session.Audio.Output.Format == (realtimeFormat{Type: "audio/pcm", Rate: 24000})
}

func decodeRealtime(payload []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing realtime data")
	}
	return nil
}

func observeLiveWindow(ctx context.Context, provider LiveProvider, subject inference.Subject, operation string) <-chan liveObserved {
	out := make(chan liveObserved, 1)
	go func() {
		defer close(out)
		for cursor := int64(0); ; {
			page := provider.ObserveLive(ctx, subject, operation, cursor, 256, 65536, 500)
			if ctx.Err() != nil {
				return
			}
			if page.Outcome != wire.PageOutcomePage {
				select {
				case out <- liveObserved{err: fmt.Errorf("observe: %s", page.Outcome)}:
				case <-ctx.Done():
				}
				return
			}
			for i := range page.Deltas {
				delta := page.Deltas[i]
				select {
				case out <- liveObserved{delta: &delta}:
				case <-ctx.Done():
					return
				}
				if delta.LiveEnd != nil {
					return
				}
			}
			cursor = page.Next
			if page.AtEnd {
				select {
				case out <- liveObserved{err: errors.New("observe ended without live reply")}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out
}
