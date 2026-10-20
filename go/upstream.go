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
	"time"

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

type wireAdapterDeclaration struct {
	chat  adapter
	stall time.Duration
	relay bool
}

// One declaration per router wire_kinds member, plus the native IPC binding.
// Generation's stall belongs to its wire adapter (Architect Codex 268).
// Longer provider bounds must cite documented provider behaviour here.
var wireAdapters = map[string]wireAdapterDeclaration{
	router.WireOpenAICompatible:     {chat: openAI{}, stall: defaultGenerationStall},
	router.WireAnthropicMessages:    {chat: anthropic{}, stall: defaultGenerationStall},
	router.WireDeepgramPrerecorded:  {stall: defaultGenerationStall},
	router.WireElevenLabsStream:     {stall: defaultGenerationStall},
	router.WireStabilityV2Beta:      {stall: defaultGenerationStall},
	router.WireFalQueue:             {stall: defaultGenerationStall},
	router.WireReplicatePredictions: {stall: defaultGenerationStall},
	router.WireOpenAIRealtime:       {stall: defaultGenerationStall},
	router.WireRemote:               {chat: remoteChat{}, relay: true},
	router.WireNative:               {chat: nativeChat{}, relay: true},
}

func adapterFor(kind string) adapter {
	return wireAdapters[kind].chat
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
// against the keys it knows. A nil known set leaves extension checks to the
// selected native or remote host's registered profile inventory.
func unsupportedBy(req wire.Request, served map[string]bool, known map[string]bool) string {
	for _, f := range usesFeatures(req) {
		if !served[f] {
			return "feature:" + f
		}
	}
	for _, key := range req.RequiredExtensions {
		if known != nil && !known[key] {
			return "extension:" + key
		}
	}
	return ""
}

func failed(outcome wire.ReplyOutcome, reason string) wire.Reply {
	return wire.Reply{Outcome: outcome, Reason: reason, StopReason: wire.StopReasonNoStop}
}

// A credential is applied to the declared model server for one request.
// Refuse redirects before net/http copies a custom credential header to a
// target the credentials holder has not approved.
func credentialRequestClient(client *http.Client, headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return client
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("upstream credential redirect refused")
	}
	return &copy
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
	generationSent(ctx)
	resp, err := credentialRequestClient(client, headers).Do(req)
	if err != nil {
		r := failed(wire.ReplyOutcomeUnavailable, "upstream:unreachable")
		return nil, &r
	}
	postResponse(ctx, resp)
	if resp.StatusCode != http.StatusOK {
		// The body is read only to recognise a context-window refusal and its
		// two numbers (INF-Q7); nothing else from it is kept.
		//unchecked: a short or failed read leaves the status-code outcome below
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		//unchecked: cleanup before returning the more specific status-code error
		resp.Body.Close()
		window, input, overflow := contextOverflow(body)
		clear(body)
		if overflow && resp.StatusCode != http.StatusTooManyRequests {
			r := overflowed(window, input)
			return nil, &r
		}
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

// micros counts currency in millionths, rounded up. It accepts a bounded JSON
// decimal with at most 64 mantissa digits and an exponent in -30..30, using
// integer arithmetic. Invalid, negative and unrepresentable costs are absent.
func micros(n json.Number) (int64, bool) {
	s := string(n)
	if len(s) == 0 || len(s) > 80 || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 1 && s[0] == '0' {
		return 0, false
	}
	digits, fractional := s[:i], 0
	if i < len(s) && s[i] == '.' {
		i++
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0, false
		}
		fractional = i - start
		digits += s[start:i]
	}
	if len(digits) > 64 {
		return 0, false
	}
	exponent := 0
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		sign := 1
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			if s[i] == '-' {
				sign = -1
			}
			i++
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			exponent = exponent*10 + int(s[i]-'0')
			if exponent > 30 {
				return 0, false
			}
			i++
		}
		if i == start {
			return 0, false
		}
		exponent *= sign
	}
	if i != len(s) {
		return 0, false
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return 0, true
	}
	scale := exponent + 6 - fractional
	roundUp := false
	if scale >= 0 {
		if len(digits)+scale > 19 {
			return 0, false
		}
		digits += strings.Repeat("0", scale)
	} else {
		cut := len(digits) + scale
		if cut <= 0 {
			return 1, true
		}
		roundUp = strings.Trim(digits[cut:], "0") != ""
		digits = digits[:cut]
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	if roundUp {
		if v == int64(^uint64(0)>>1) {
			return 0, false
		}
		v++
	}
	return v, true
}

// temperature spells thousandths as a JSON decimal number.
func temperature(t *wire.Temperature) json.Number {
	return json.Number(fmt.Sprintf("%d.%03d", t.Milli/1000, t.Milli%1000))
}
