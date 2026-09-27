package realtime

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

	websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

const (
	livePCM16At24KHz       = wire.LiveFormatPcm1624000
	maxLiveAudioBytes      = 64 << 10
	maxLiveTranscriptBytes = 16 << 10
	maxLiveServerFrame     = 128 << 10
	liveHandshakeTimeout   = 10 * time.Second
)

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

// Dial opens the service-owned upstream Realtime connection and
// waits for the server to acknowledge the complete GA session configuration.
func Dial(ctx context.Context, client *http.Client, address, model, voice string, format wire.LiveFormat, headers map[string]string) (inference.LiveConnection, error) {
	if client == nil || ctx == nil || format != livePCM16At24KHz || model == "" || len(model) > 256 || voice == "" || len(voice) > 256 {
		return nil, inference.NewLiveError(inference.LiveErrorInvalid)
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil {
		return nil, inference.NewLiveError(inference.LiveErrorInvalid)
	}

	h := make(http.Header, len(headers))
	for k, v := range headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k, "\r\n") || strings.ContainsAny(v, "\r\n") {
			return nil, inference.NewLiveError(inference.LiveErrorInvalid)
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
			//unchecked: cleanup of the failed handshake's response body before returning the more specific dial error
			response.Body.Close()
		}
		return nil, errorForContext(handshakeCtx, inference.LiveErrorUnavailable)
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
		b.Close()
		return nil, err
	}
	for messages := 0; messages < 16; messages++ {
		typ, payload, err := conn.Read(handshakeCtx)
		if err != nil {
			b.Close()
			return nil, liveReadError(handshakeCtx, err)
		}
		if typ != websocket.MessageText {
			b.Close()
			return nil, inference.NewLiveError(inference.LiveErrorMalformed)
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Type == "" {
			b.Close()
			return nil, inference.NewLiveError(inference.LiveErrorMalformed)
		}
		switch event.Type {
		case "session.updated":
			return b, nil
		case "error":
			b.Close()
			return nil, inference.NewLiveError(inference.LiveErrorRefused)
		case "session.created":
		default:
		}
	}
	b.Close()
	return nil, inference.NewLiveError(inference.LiveErrorMalformed)
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

func (b *openAIRealtimeBackend) Append(ctx context.Context, audio []byte) error {
	if len(audio) == 0 {
		return inference.NewLiveError(inference.LiveErrorInvalid)
	}
	if len(audio) > maxLiveAudioBytes {
		return inference.NewLiveError(inference.LiveErrorTooLarge)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	if b.ended(true) {
		return inference.NewLiveError(inference.LiveErrorInvalidState)
	}
	event := struct {
		Type  string `json:"type"`
		Audio string `json:"audio"`
	}{Type: "input_audio_buffer.append", Audio: base64.StdEncoding.EncodeToString(audio)}
	if err := b.writeJSON(ctx, event); err != nil {
		b.Close()
		return err
	}
	return nil
}

func (b *openAIRealtimeBackend) Commit(ctx context.Context) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	b.stateMu.Lock()
	if b.closed || b.committed || b.terminal {
		b.stateMu.Unlock()
		return inference.NewLiveError(inference.LiveErrorInvalidState)
	}
	b.committed = true
	b.stateMu.Unlock()
	if err := b.writeJSON(ctx, struct {
		Type string `json:"type"`
	}{Type: "input_audio_buffer.commit"}); err != nil {
		b.Close()
		return err
	}
	if err := b.writeJSON(ctx, struct {
		Type string `json:"type"`
	}{Type: "response.create"}); err != nil {
		b.Close()
		return err
	}
	return nil
}

func (b *openAIRealtimeBackend) Read(ctx context.Context) (inference.LiveEvent, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	for {
		if b.ended(false) {
			return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorInvalidState)
		}
		typ, payload, err := b.conn.Read(ctx)
		if err != nil {
			b.Close()
			return inference.LiveEvent{}, liveReadError(ctx, err)
		}
		if typ != websocket.MessageText {
			b.Close()
			return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
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
			b.Close()
			return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
		}
		switch event.Type {
		case "response.output_audio.delta":
			if event.Delta == "" || base64.StdEncoding.DecodedLen(len(event.Delta)) > maxLiveAudioBytes {
				b.Close()
				return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorTooLarge)
			}
			audio, err := base64.StdEncoding.Strict().DecodeString(event.Delta)
			if err != nil {
				b.Close()
				return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
			}
			if len(audio) == 0 || len(audio) > maxLiveAudioBytes {
				b.Close()
				return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorTooLarge)
			}
			return inference.LiveEvent{Kind: inference.LiveEventAudio, Audio: audio}, nil
		case "response.output_audio_transcript.delta":
			if event.Delta == "" || len(event.Delta) > maxLiveTranscriptBytes {
				b.Close()
				return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorTooLarge)
			}
			return inference.LiveEvent{Kind: inference.LiveEventTranscript, Transcript: event.Delta}, nil
		case "response.output_audio_transcript.done":
			// The done event can repeat the complete transcript. Deltas already
			// carry that text, so expose only the completion marker.
			return inference.LiveEvent{Kind: inference.LiveEventTranscript, Final: true}, nil
		case "response.done":
			terminal, err := terminalLiveEvent(event.Response)
			if err != nil {
				b.Close()
				return inference.LiveEvent{}, err
			}
			b.stateMu.Lock()
			b.terminal = true
			b.stateMu.Unlock()
			b.Close()
			return terminal, nil
		case "error":
			b.Close()
			return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorRefused)
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
}) (inference.LiveEvent, error) {
	if response == nil || response.Status == "" {
		return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
	}
	e := inference.LiveEvent{Kind: inference.LiveEventTerminal, Final: true, Reason: "upstream:" + response.Status}
	switch response.Status {
	case "completed":
		e.Outcome = inference.LiveCompleted
		e.Reason = ""
	case "cancelled":
		e.Outcome = inference.LiveCancelled
	case "failed":
		e.Outcome = inference.LiveFailed
	case "incomplete":
		e.Outcome = inference.LiveIncomplete
	default:
		return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
	}
	if response.Usage != nil {
		u := response.Usage
		if u.InputTokens < 0 || u.OutputTokens < 0 || u.InputTokenDetails.AudioTokens < 0 || u.OutputTokenDetails.AudioTokens < 0 {
			return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorMalformed)
		}
		e.Usage = inference.LiveUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			InputAudioTokens: u.InputTokenDetails.AudioTokens, OutputAudioTokens: u.OutputTokenDetails.AudioTokens}
	}
	return e, nil
}

func (b *openAIRealtimeBackend) writeJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return inference.NewLiveError(inference.LiveErrorInvalid)
	}
	if err := b.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		return errorForContext(ctx, inference.LiveErrorUnavailable)
	}
	return nil
}

func (b *openAIRealtimeBackend) ended(includeCommitted bool) bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.closed || b.terminal || includeCommitted && b.committed
}

func (b *openAIRealtimeBackend) Close() {
	b.closeOnce.Do(func() {
		b.stateMu.Lock()
		b.closed = true
		b.stateMu.Unlock()
		//unchecked: best-effort close in a sync.Once teardown helper that returns nothing; no caller left to report the failure to
		_ = b.conn.CloseNow()
	})
}

func liveReadError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return inference.NewLiveError(inference.LiveErrorCancelled)
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return inference.NewLiveError(inference.LiveErrorTooLarge)
	}
	return inference.NewLiveError(inference.LiveErrorUnavailable)
}

func errorForContext(ctx context.Context, fallback inference.LiveErrorCode) error {
	if ctx.Err() != nil {
		return inference.NewLiveError(inference.LiveErrorCancelled)
	}
	return inference.NewLiveError(fallback)
}
