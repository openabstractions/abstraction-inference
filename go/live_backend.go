package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

const (
	livePCM16At24KHz       = "pcm16_24000"
	maxLiveAudioBytes      = 64 << 10
	maxLiveTranscriptBytes = 16 << 10
	maxLiveServerFrame     = 128 << 10
	liveHandshakeTimeout   = 10 * time.Second
)

type liveBackend interface {
	append(context.Context, []byte) error
	commit(context.Context) error
	read(context.Context) (liveBackendEvent, error)
	close()
}

type liveBackendEventKind string

const (
	liveBackendEventAudio      liveBackendEventKind = "audio"
	liveBackendEventTranscript liveBackendEventKind = "transcript"
	liveBackendEventTerminal   liveBackendEventKind = "terminal"
)

type liveBackendUsage struct {
	inputTokens       int64
	outputTokens      int64
	inputAudioTokens  int64
	outputAudioTokens int64
}

type liveBackendEvent struct {
	kind       liveBackendEventKind
	audio      []byte
	transcript string
	final      bool
	outcome    string
	reason     string
	usage      liveBackendUsage
	remoteEnd  *wire.LiveReply
}

type liveBackendErrorCode string

const (
	liveBackendCancelled    liveBackendErrorCode = "cancelled"
	liveBackendUnavailable  liveBackendErrorCode = "unavailable"
	liveBackendRefused      liveBackendErrorCode = "refused"
	liveBackendMalformed    liveBackendErrorCode = "malformed"
	liveBackendTooLarge     liveBackendErrorCode = "too_large"
	liveBackendInvalid      liveBackendErrorCode = "invalid"
	liveBackendInvalidState liveBackendErrorCode = "invalid_state"
)

// liveBackendError is deliberately content-free. Upstream errors can include
// request URLs and headers, and those values must stay out of provider errors.
type liveBackendError struct{ code liveBackendErrorCode }

func (e *liveBackendError) Error() string { return "live backend: " + string(e.code) }

func liveError(code liveBackendErrorCode) error { return &liveBackendError{code: code} }

func liveErrorForContext(ctx context.Context, fallback liveBackendErrorCode) error {
	if ctx.Err() != nil {
		return liveError(liveBackendCancelled)
	}
	return liveError(fallback)
}

type openAIRealtimeBackend struct {
	conn *websocket.Conn

	writeMu   sync.Mutex
	readMu    sync.Mutex
	stateMu   sync.Mutex
	closed    bool
	committed bool
	terminal  bool
	closeOnce sync.Once
}

// dialLiveBackend opens the service-owned upstream Realtime connection and
// waits for the server to acknowledge the complete GA session configuration.
func dialLiveBackend(ctx context.Context, client *http.Client, address, model, voice, format string, headers map[string]string) (liveBackend, error) {
	if client == nil || ctx == nil || format != livePCM16At24KHz || model == "" || len(model) > 256 || voice == "" || len(voice) > 256 {
		return nil, liveError(liveBackendInvalid)
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil {
		return nil, liveError(liveBackendInvalid)
	}

	h := make(http.Header, len(headers))
	for k, v := range headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, liveError(liveBackendInvalid)
		}
		h.Set(k, v)
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, liveHandshakeTimeout)
	defer cancel()
	conn, response, err := websocket.Dial(handshakeCtx, address, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: h,
	})
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, liveErrorForContext(handshakeCtx, liveBackendUnavailable)
	}
	b := &openAIRealtimeBackend{conn: conn}
	conn.SetReadLimit(maxLiveServerFrame)

	update := struct {
		Type    string              `json:"type"`
		Session liveSessionSettings `json:"session"`
	}{Type: "session.update"}
	update.Session.Type = "realtime"
	update.Session.Model = model
	update.Session.OutputModalities = []string{"audio"}
	update.Session.Audio.Input.Format = liveAudioFormat{Type: "audio/pcm", Rate: 24000}
	update.Session.Audio.Input.TurnDetection = nil
	update.Session.Audio.Output.Format = liveAudioFormat{Type: "audio/pcm", Rate: 24000}
	update.Session.Audio.Output.Voice = voice
	if err := b.writeJSON(handshakeCtx, update); err != nil {
		b.close()
		return nil, err
	}
	for messages := 0; messages < 16; messages++ {
		typ, payload, err := conn.Read(handshakeCtx)
		if err != nil {
			b.close()
			return nil, liveReadError(handshakeCtx, err)
		}
		if typ != websocket.MessageText {
			b.close()
			return nil, liveError(liveBackendMalformed)
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Type == "" {
			b.close()
			return nil, liveError(liveBackendMalformed)
		}
		switch event.Type {
		case "session.updated":
			return b, nil
		case "error":
			b.close()
			return nil, liveError(liveBackendRefused)
		case "session.created":
		default:
		}
	}
	b.close()
	return nil, liveError(liveBackendMalformed)
}

type liveAudioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type liveSessionSettings struct {
	Type             string   `json:"type"`
	Model            string   `json:"model"`
	OutputModalities []string `json:"output_modalities"`
	Audio            struct {
		Input struct {
			Format        liveAudioFormat `json:"format"`
			TurnDetection any             `json:"turn_detection"`
		} `json:"input"`
		Output struct {
			Format liveAudioFormat `json:"format"`
			Voice  string          `json:"voice"`
		} `json:"output"`
	} `json:"audio"`
}

func (b *openAIRealtimeBackend) append(ctx context.Context, audio []byte) error {
	if len(audio) == 0 {
		return liveError(liveBackendInvalid)
	}
	if len(audio) > maxLiveAudioBytes {
		return liveError(liveBackendTooLarge)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.ended(true) {
		return liveError(liveBackendInvalidState)
	}
	event := struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}{Type: "input_audio_buffer.append", Audio: base64.StdEncoding.EncodeToString(audio)}
	if err := b.writeJSON(ctx, event); err != nil {
		b.close()
		return err
	}
	return nil
}

func (b *openAIRealtimeBackend) commit(ctx context.Context) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.stateMu.Lock()
	if b.closed || b.committed || b.terminal {
		b.stateMu.Unlock()
		return liveError(liveBackendInvalidState)
	}
	b.committed = true
	b.stateMu.Unlock()
	if err := b.writeJSON(ctx, struct {
		Type string `json:"type"`
	}{Type: "input_audio_buffer.commit"}); err != nil {
		b.close()
		return err
	}
	if err := b.writeJSON(ctx, struct {
		Type string `json:"type"`
	}{Type: "response.create"}); err != nil {
		b.close()
		return err
	}
	return nil
}

func (b *openAIRealtimeBackend) read(ctx context.Context) (liveBackendEvent, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	for {
		if b.ended(false) {
			return liveBackendEvent{}, liveError(liveBackendInvalidState)
		}
		typ, payload, err := b.conn.Read(ctx)
		if err != nil {
			b.close()
			return liveBackendEvent{}, liveReadError(ctx, err)
		}
		if typ != websocket.MessageText {
			b.close()
			return liveBackendEvent{}, liveError(liveBackendMalformed)
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response *struct {
				Status string `json:"status"`
				Usage  *struct {
					InputTokens       int64 `json:"input_tokens"`
					OutputTokens      int64 `json:"output_tokens"`
					InputTokenDetails struct {
						AudioTokens int64 `json:"audio_tokens"`
					} `json:"input_token_details"`
					OutputTokenDetails struct {
						AudioTokens int64 `json:"audio_tokens"`
					} `json:"output_token_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Type == "" {
			b.close()
			return liveBackendEvent{}, liveError(liveBackendMalformed)
		}
		switch event.Type {
		case "response.output_audio.delta":
			if event.Delta == "" || base64.StdEncoding.DecodedLen(len(event.Delta)) > maxLiveAudioBytes {
				b.close()
				return liveBackendEvent{}, liveError(liveBackendTooLarge)
			}
			audio, err := base64.StdEncoding.Strict().DecodeString(event.Delta)
			if err != nil {
				b.close()
				return liveBackendEvent{}, liveError(liveBackendMalformed)
			}
			if len(audio) == 0 || len(audio) > maxLiveAudioBytes {
				b.close()
				return liveBackendEvent{}, liveError(liveBackendTooLarge)
			}
			return liveBackendEvent{kind: liveBackendEventAudio, audio: audio}, nil
		case "response.output_audio_transcript.delta":
			if event.Delta == "" || len(event.Delta) > maxLiveTranscriptBytes {
				b.close()
				return liveBackendEvent{}, liveError(liveBackendTooLarge)
			}
			return liveBackendEvent{kind: liveBackendEventTranscript, transcript: event.Delta}, nil
		case "response.output_audio_transcript.done":
			// The done event can repeat the complete transcript. Deltas already
			// carry that text, so expose only the completion marker.
			return liveBackendEvent{kind: liveBackendEventTranscript, final: true}, nil
		case "response.done":
			terminal, err := terminalLiveEvent(event.Response)
			if err != nil {
				b.close()
				return liveBackendEvent{}, err
			}
			b.stateMu.Lock()
			b.terminal = true
			b.stateMu.Unlock()
			b.close()
			return terminal, nil
		case "error":
			b.close()
			return liveBackendEvent{}, liveError(liveBackendRefused)
		default:
		}
	}
}

func terminalLiveEvent(response *struct {
	Status string `json:"status"`
	Usage  *struct {
		InputTokens       int64 `json:"input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		InputTokenDetails struct {
			AudioTokens int64 `json:"audio_tokens"`
		} `json:"input_token_details"`
		OutputTokenDetails struct {
			AudioTokens int64 `json:"audio_tokens"`
		} `json:"output_token_details"`
	} `json:"usage"`
}) (liveBackendEvent, error) {
	if response == nil || response.Status == "" {
		return liveBackendEvent{}, liveError(liveBackendMalformed)
	}
	e := liveBackendEvent{kind: liveBackendEventTerminal, final: true, outcome: response.Status, reason: "upstream:" + response.Status}
	switch response.Status {
	case "completed":
		e.reason = ""
	case "cancelled", "failed", "incomplete":
	default:
		return liveBackendEvent{}, liveError(liveBackendMalformed)
	}
	if response.Usage != nil {
		u := response.Usage
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.InputTokenDetails.AudioTokens < 0 || u.OutputTokenDetails.AudioTokens < 0 {
			return liveBackendEvent{}, liveError(liveBackendMalformed)
		}
		e.usage = liveBackendUsage{inputTokens: u.InputTokens, outputTokens: u.OutputTokens,
			inputAudioTokens: u.InputTokenDetails.AudioTokens, outputAudioTokens: u.OutputTokenDetails.AudioTokens}
	}
	return e, nil
}

func (b *openAIRealtimeBackend) writeJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return liveError(liveBackendInvalid)
	}
	if err := b.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return liveErrorForContext(ctx, liveBackendUnavailable)
	}
	return nil
}

func (b *openAIRealtimeBackend) ended(includeCommitted bool) bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.closed || b.terminal || includeCommitted && b.committed
}

func (b *openAIRealtimeBackend) close() {
	b.closeOnce.Do(func() {
		b.stateMu.Lock()
		b.closed = true
		b.stateMu.Unlock()
		_ = b.conn.CloseNow()
	})
}

func liveBackendErrorIs(err error, code liveBackendErrorCode) bool {
	var target *liveBackendError
	return errors.As(err, &target) && target.code == code
}

func liveReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return liveError(liveBackendCancelled)
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return liveError(liveBackendTooLarge)
	}
	return liveError(liveBackendUnavailable)
}
