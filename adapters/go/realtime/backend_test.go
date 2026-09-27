package realtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

func TestLiveBackendHandshakeAudioAndCommit(t *testing.T) {
	t.Parallel()
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			serverErr <- errors.New("authorization header missing")
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`)); err != nil {
			serverErr <- err
			return
		}
		_, update, err := conn.Read(ctx)
		if err != nil {
			serverErr <- err
			return
		}
		var got struct {
			Type    string `json:"type"`
			Session struct {
				Type             string   `json:"type"`
				Model            string   `json:"model"`
				OutputModalities []string `json:"output_modalities"`
				Audio            struct {
					Input struct {
						Format        liveAudioFormat  `json:"format"`
						TurnDetection *json.RawMessage `json:"turn_detection"`
					} `json:"input"`
					Output struct {
						Format liveAudioFormat `json:"format"`
						Voice  string          `json:"voice"`
					} `json:"output"`
				} `json:"audio"`
			} `json:"session"`
		}
		if json.Unmarshal(update, &got) != nil || got.Type != "session.update" || got.Session.Type != "realtime" ||
			got.Session.Model != "gpt-realtime" || len(got.Session.OutputModalities) != 1 || got.Session.OutputModalities[0] != "audio" ||
			got.Session.Audio.Input.Format != (liveAudioFormat{Type: "audio/pcm", Rate: 24000}) || got.Session.Audio.Input.TurnDetection != nil ||
			got.Session.Audio.Output.Format != (liveAudioFormat{Type: "audio/pcm", Rate: 24000}) || got.Session.Audio.Output.Voice != "marin" {
			serverErr <- errors.New("wrong session update")
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated"}`)); err != nil {
			serverErr <- err
			return
		}

		wantTypes := []string{"input_audio_buffer.append", "input_audio_buffer.commit", "response.create"}
		for i, want := range wantTypes {
			_, payload, err := conn.Read(ctx)
			if err != nil {
				serverErr <- err
				return
			}
			var event struct{ Type, Audio string }
			if json.Unmarshal(payload, &event) != nil || event.Type != want {
				serverErr <- errors.New("wrong client event order")
				return
			}
			if i == 0 {
				audio, err := base64.StdEncoding.Strict().DecodeString(event.Audio)
				if err != nil || string(audio) != "input-pcm" {
					serverErr <- errors.New("wrong input audio")
					return
				}
			}
		}
		out := base64.StdEncoding.EncodeToString([]byte("output-pcm"))
		messages := []string{
			`{"type":"response.created"}`,
			`{"type":"response.output_audio.delta","delta":"` + out + `"}`,
			`{"type":"response.output_audio_transcript.delta","delta":"hello"}`,
			`{"type":"response.output_audio_transcript.done","transcript":"hello"}`,
			`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":7,"input_token_details":{"audio_tokens":3},"output_token_details":{"audio_tokens":4}}}}`,
		}
		for _, message := range messages {
			if err := conn.Write(ctx, websocket.MessageText, []byte(message)); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}))
	defer server.Close()

	headers := map[string]string{"Authorization": "Bearer test-secret"}
	b, err := Dial(context.Background(), server.Client(), websocketURL(server.URL), "gpt-realtime", "marin", livePCM16At24KHz, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if headers["Authorization"] != "Bearer test-secret" || len(headers) != 1 {
		t.Fatal("dial mutated caller headers")
	}
	if err := b.Append(context.Background(), []byte("input-pcm")); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Append(context.Background(), []byte("late")); !inference.IsLiveError(err, inference.LiveErrorInvalidState) {
		t.Fatalf("append after commit: %v", err)
	}
	audio, err := b.Read(context.Background())
	if err != nil || audio.Kind != inference.LiveEventAudio || string(audio.Audio) != "output-pcm" {
		t.Fatalf("audio = %#v, %v", audio, err)
	}
	transcript, err := b.Read(context.Background())
	if err != nil || transcript.Kind != inference.LiveEventTranscript || transcript.Transcript != "hello" {
		t.Fatalf("transcript = %#v, %v", transcript, err)
	}
	transcriptDone, err := b.Read(context.Background())
	if err != nil || transcriptDone.Kind != inference.LiveEventTranscript || transcriptDone.Transcript != "" || !transcriptDone.Final {
		t.Fatalf("transcript done = %#v, %v", transcriptDone, err)
	}
	end, err := b.Read(context.Background())
	if err != nil || end.Kind != inference.LiveEventTerminal || !end.Final || end.Outcome != inference.LiveCompleted || end.Reason != "" ||
		end.Usage.InputTokens != 5 || end.Usage.OutputTokens != 7 || end.Usage.InputAudioTokens != 3 || end.Usage.OutputAudioTokens != 4 {
		t.Fatalf("terminal = %#v, %v", end, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLiveBackendRejectsInvalidBeforeDial(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached network")
		return nil, errors.New("unexpected")
	})}
	for _, tc := range []struct {
		address string
		format  wire.LiveFormat
	}{
		{"https://example.test/realtime", livePCM16At24KHz},
		{"ws://example.test/realtime", wire.LiveFormat(0)},
	} {
		if _, err := Dial(context.Background(), client, tc.address, "model", "voice", tc.format, nil); !inference.IsLiveError(err, inference.LiveErrorInvalid) {
			t.Fatalf("dial(%q, %q) = %v", tc.address, tc.format, err)
		}
	}
}

func TestLiveBackendMalformedAndOversizedEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, message string
		code          inference.LiveErrorCode
	}{
		{"malformed json", `{`, inference.LiveErrorMalformed},
		{"bad audio", `{"type":"response.output_audio.delta","delta":"%%%"}`, inference.LiveErrorMalformed},
		{"oversized decoded audio", `{"type":"response.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(make([]byte, maxLiveAudioBytes+1)) + `"}`, inference.LiveErrorTooLarge},
		{"oversized transcript", `{"type":"response.output_audio_transcript.delta","delta":"` + strings.Repeat("x", maxLiveTranscriptBytes+1) + `"}`, inference.LiveErrorTooLarge},
		{"malformed terminal", `{"type":"response.done","response":{"status":"surprising"}}`, inference.LiveErrorMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := liveTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
				_ = conn.Write(ctx, websocket.MessageText, []byte(tc.message))
			})
			defer server.Close()
			b := dialTestLiveBackend(t, server)
			defer b.Close()
			if _, err := b.Read(context.Background()); !inference.IsLiveError(err, tc.code) {
				t.Fatalf("read = %v, want code %d", err, tc.code)
			}
		})
	}
}

func TestLiveBackendReadCancellationClosesConnection(t *testing.T) {
	t.Parallel()
	closed := make(chan struct{})
	server := liveTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_, _, _ = conn.Read(ctx)
		close(closed)
	})
	defer server.Close()
	b := dialTestLiveBackend(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.Read(ctx); !inference.IsLiveError(err, inference.LiveErrorCancelled) {
		t.Fatalf("read = %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled read did not close upstream connection")
	}
}

func TestLiveBackendHandshakeCancellationIsSafe(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, server.Client(), websocketURL(server.URL), "model", "voice", livePCM16At24KHz, nil); !inference.IsLiveError(err, inference.LiveErrorCancelled) {
		t.Fatalf("dial = %v", err)
	}
}

func liveTestServer(t *testing.T, afterHandshake func(context.Context, *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated"}`)); err != nil {
			return
		}
		afterHandshake(ctx, conn)
	}))
}

func dialTestLiveBackend(t *testing.T, server *httptest.Server) inference.LiveConnection {
	t.Helper()
	b, err := Dial(context.Background(), server.Client(), websocketURL(server.URL), "model", "voice", livePCM16At24KHz, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func websocketURL(httpURL string) string { return "ws" + strings.TrimPrefix(httpURL, "http") }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLiveBackendConcurrentClose(t *testing.T) {
	t.Parallel()
	server := liveTestServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_, _, _ = conn.Read(ctx)
	})
	defer server.Close()
	b := dialTestLiveBackend(t, server)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); b.Close() }()
	}
	wg.Wait()
}
