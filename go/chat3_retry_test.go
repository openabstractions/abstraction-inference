package inference

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3RetryAfterHTTPFormsUseSourceClockAndBound(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 500000, time.UTC)
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"2", 2000}, {" 3 ", 3000}, {"86401", 86400000}, {strings.Repeat("9", 100), 86400000},
		{now.Add(3 * time.Second).UTC().Format(http.TimeFormat), 3000},
		{now.Add(48 * time.Hour).UTC().Format(http.TimeFormat), 86400000},
		{"", 0}, {"0", 0}, {"-1", 0}, {"+1", 0}, {"1.5", 0}, {"2, 3", 0}, {"tomorrow", 0},
		{now.Add(-time.Second).UTC().Format(http.TimeFormat), 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got := chat3RetryAfter(tc.value, func() time.Time { return now })
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("unexpected hint %d", *got)
				}
				return
			}
			if got == nil || *got != tc.want {
				t.Fatalf("hint %v want %d", got, tc.want)
			}
		})
	}
}

func TestChat3RetryAfterActualResponseOnlyUnavailableNoRetry(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, origin := range []string{router.WireOpenAICompatible, router.WireAnthropicMessages, "anthropic-opaque"} {
		for _, tc := range []struct {
			name         string
			status       int
			header, body string
			want         int64
		}{
			{"rate", 429, "2", "busy", 2000},
			{"date", 503, now.Add(5 * time.Second).Format(http.TimeFormat), "busy", 5000},
			{"capped", 500, "999999999999999999999", "busy", 86400000},
			{"invalid", 503, "not-a-duration", "busy", 0},
			{"absent", 503, "", "busy", 0},
			{"authentication", 401, "2", "denied", 0},
			{"authority", 403, "2", "denied", 0},
			{"budget", 402, "2", "payment required", 0},
			{"invalid-request", 400, "2", "bad request", 0},
			{"refined-context", 503, "2", `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`, 0},
			{"successful", 200, "2", "", 0},
		} {
			t.Run(origin+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("X-API-Key") != "fixture" || r.Header.Get("Accept") != "text/event-stream" {
						t.Error("request headers changed")
					}
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(tc.status)
					if tc.status != 200 {
						fmt.Fprint(w, tc.body)
						return
					}
					if origin == router.WireOpenAICompatible {
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
					}
				}))
				defer server.Close()
				ctx := context.Background()
				req := wire.Request{}
				if origin == "anthropic-opaque" {
					req.Messages = []wire.Message{{Role: wire.RoleAssistant, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "prior answer"}}}}
					ctx = withChat3Replay(ctx, []wire.ChatMessage{{Role: wire.RoleAssistant, Reasoning: []wire.ChatReplay{{Seal: wire.ReasoningSeal{Origin: router.WireAnthropicMessages, Bytes: []byte(`{"type":"redacted_thinking","data":"opaque"}`)}}}}}, nil)
				}
				sink := chat3Sink{Now: func() time.Time { return now }}
				host := &router.Host{Base: server.URL, Chat: "/chat"}
				headers := map[string]string{"X-API-Key": "fixture"}
				var result chat3UpstreamResult
				if origin == router.WireOpenAICompatible {
					result = runChat3OpenAI(ctx, server.Client(), host, "m", req, nil, nil, nil, headers, sink)
				} else {
					result = runChat3Anthropic(ctx, server.Client(), host, "m", req, nil, nil, nil, headers, sink)
				}
				if calls.Load() != 1 {
					t.Fatalf("automatic retry: %d calls", calls.Load())
				}
				if tc.want == 0 {
					if result.RetryAfterMs != nil {
						t.Fatalf("unexpected hint %+v", result)
					}
				} else if result.RetryAfterMs == nil || *result.RetryAfterMs != tc.want {
					t.Fatalf("result %+v want %d", result, tc.want)
				}
			})
		}
	}
}

func TestChat3RetryObserverLeavesLegacyPostReplyUnchanged(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(503)
	}))
	defer server.Close()
	response, reply := post(context.Background(), server.Client(), server.URL, map[string]any{"model": "m"}, nil)
	if response != nil || reply == nil || reply.Outcome != wire.ReplyOutcomeUnavailable || reply.Reason != "upstream:503" || calls.Load() != 1 {
		t.Fatalf("legacy result %+v calls %d", reply, calls.Load())
	}
}
