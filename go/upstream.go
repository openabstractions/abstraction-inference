package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// adapter speaks one upstream wire.
type adapter interface {
	// unsupported names the first request feature this wire cannot serve, as
	// feature:<name> or extension:<key>, or "".
	unsupported(req wire.Request) string
	// run sends the request with headers applied once, emits deltas through s,
	// and returns the reply's outcome, reason, stop reason, usage and cost.
	run(ctx context.Context, client *http.Client, host *router.Host, model string, req wire.Request, images map[string][]byte, headers map[string]string, s *stream) wire.Reply
}

func adapterFor(kind string) adapter {
	switch kind {
	case router.WireOpenAICompatible:
		return openAI{}
	case router.WireAnthropicMessages:
		return anthropic{}
	case router.WireRemote:
		return remoteChat{}
	case router.WireNative:
		return nativeChat{}
	}
	return nil
}

// usesFeatures returns the features a request uses, in catalogue order.
func usesFeatures(req wire.Request) []string {
	var out []string
	if len(req.Tools) > 0 {
		out = append(out, FeatureTools)
	}
	if req.Options != nil && req.Options.JSONSchema != "" {
		out = append(out, FeatureJSONSchema)
	}
	for _, m := range req.Messages {
		for _, part := range m.Parts {
			if part.Kind == wire.PartKindImage {
				return append(out, FeatureVision)
			}
		}
	}
	return out
}

// unsupportedBy checks features against a wire's set and required extensions
// against the keys it knows.
func unsupportedBy(req wire.Request, served map[string]bool, known map[string]bool) string {
	for _, f := range usesFeatures(req) {
		if !served[f] {
			return "feature:" + f
		}
	}
	for _, key := range req.RequiredExtensions {
		if !known[key] {
			return "extension:" + key
		}
	}
	return ""
}

func failed(outcome wire.ReplyOutcome, reason string) wire.Reply {
	return wire.Reply{Outcome: outcome, Reason: reason, StopReason: wire.StopReasonNoStop}
}

// post sends one request. The returned reply is set when no stream follows.
func post(ctx context.Context, client *http.Client, address string, body any, headers map[string]string) (*http.Response, *wire.Reply) {
	raw, err := json.Marshal(body)
	if err != nil {
		r := failed(wire.ReplyOutcomeInvalid, "upstream:encoding")
		return nil, &r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		r := failed(wire.ReplyOutcomeUnavailable, "upstream:address")
		return nil, &r
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		r := failed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
		return nil, &r
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		outcome := wire.ReplyOutcomeRefused
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			outcome = wire.ReplyOutcomeUnavailable
		}
		r := failed(outcome, "upstream:"+strconv.Itoa(resp.StatusCode))
		return nil, &r
	}
	return resp, nil
}

// events reads server-sent events, calling fn with each event name and data
// until fn returns false or the body ends. It reports a read failure.
func events(body io.Reader, fn func(event, data string) bool) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	event := ""
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) > 0 && !fn(event, strings.Join(data, "\n")) {
				return nil
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(data) > 0 {
		fn(event, strings.Join(data, "\n"))
	}
	return nil
}

// streamBroken is the reply of a stream that ended before its terminal event.
func streamBroken(ctx context.Context, err error) wire.Reply {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return failed(wire.ReplyOutcomeCancelled, "caller")
	}
	return failed(wire.ReplyOutcomeUnavailable, "upstream:stream")
}

// micros converts a JSON decimal number of currency units into millionths
// without a float: digits beyond the sixth fractional place are truncated.
func micros(n json.Number) (int64, bool) {
	s := string(n)
	if s == "" || strings.ContainsAny(s, "eE+") {
		return 0, false
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 6 {
		frac = frac[:6]
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || w > (1<<62)/1_000_000 {
		return 0, false
	}
	v := w*1_000_000 + f
	if negative {
		v = -v
	}
	return v, true
}

// temperature spells thousandths as a JSON decimal number.
func temperature(t *wire.Temperature) json.Number {
	return json.Number(fmt.Sprintf("%d.%03d", t.Milli/1000, t.Milli%1000))
}
