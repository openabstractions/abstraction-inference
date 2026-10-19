package inference

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

// overflowReasonPrefix starts the reason of a reply the model server refused
// because the request exceeds its context window (INF-Q7). The reason ends
// with the window and the counted input tokens, each 0 when unknown.
const overflowReasonPrefix = "upstream:context_length_exceeded:"

// maxOverflowTokens bounds a token count read from an upstream error. A
// larger or malformed value is unknown.
const maxOverflowTokens = 1 << 40

// overflowCodes are the error type and code words model servers set on a
// request that exceeds the loaded context window: OpenAI and Lemonade
// (context_length_exceeded), llama.cpp (exceed_context_size_error) and
// OpenAI Responses (model_context_window_exceeded).
var overflowCodes = map[string]bool{
	"context_length_exceeded":       true,
	"exceed_context_size_error":     true,
	"model_context_window_exceeded": true,
}

// overflowPhrase recognises an overflow message from a server that sets no
// code, following the wording of llama.cpp, vLLM, OpenAI, Anthropic, LM
// Studio and Ollama, and the phrases OpenCode's own classifier accepts.
var overflowPhrase = regexp.MustCompile(`(?i)exceeds the available context size|context[_ ]length[_ ]exceeded|maximum context length|` +
	`prompt is too long|exceeds the context window|longer than the model'?s context length|configured context size|` +
	`longer than the context length|input length exceeds the context length|` +
	`greater than the context length|context length is only \d+ tokens|input is too long for requested model|model_context_window_exceeded`)

// overflowExclusion keeps rate and quota messages that mention tokens out of
// the overflow category; a retry after a wait can succeed for them.
var overflowExclusion = regexp.MustCompile(`(?i)rate limit|too many requests`)

// overflowNumbers read the counted input and the window from the messages
// that state them. input and window are submatch indexes, 0 when absent.
var overflowNumbers = []struct {
	re            *regexp.Regexp
	input, window int
}{
	{regexp.MustCompile(`(?i)\(([\d,]+) tokens\) exceeds the available context size \(([\d,]+) tokens\)`), 1, 2},
	{regexp.MustCompile(`(?i)prompt is too long: ([\d,]+) tokens > ([\d,]+) maximum`), 1, 2},
	{regexp.MustCompile(`(?i)input \(([\d,]+) tokens\) is longer than the model'?s context length \(([\d,]+) tokens\)`), 1, 2},
	{regexp.MustCompile(`(?i)prompt has ([\d,]+) tokens?, but the configured context size is ([\d,]+) tokens?`), 1, 2},
	{regexp.MustCompile(`(?i)maximum context length is ([\d,]+) tokens`), 0, 1},
	{regexp.MustCompile(`(?i)messages resulted in ([\d,]+) tokens`), 1, 0},
	{regexp.MustCompile(`(?i)\(([\d,]+) in the messages`), 1, 0},
}

// overflowFacts collects what one upstream error payload says.
type overflowFacts struct {
	window, input int64
	coded         bool
	messages      []string
}

// contextOverflow reports whether an upstream error payload refuses a
// request for exceeding the context window, with the window and counted
// input tokens it states (0 when unknown). Only those two numbers leave this
// function; the caller discards the payload.
func contextOverflow(payload []byte) (window, input int64, ok bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(payload, &top) != nil {
		return 0, 0, false
	}
	var facts overflowFacts
	facts.read(top, 0)
	phrased := false
	for _, message := range facts.messages {
		if overflowExclusion.MatchString(message) {
			phrased = false
			break
		}
		if overflowPhrase.MatchString(message) {
			phrased = true
		}
	}
	if !facts.coded && !phrased {
		return 0, 0, false
	}
	for _, message := range facts.messages {
		for _, n := range overflowNumbers {
			m := n.re.FindStringSubmatch(message)
			if m == nil {
				continue
			}
			if n.input > 0 && facts.input == 0 {
				facts.input = overflowCount(strings.ReplaceAll(m[n.input], ",", ""))
			}
			if n.window > 0 && facts.window == 0 {
				facts.window = overflowCount(strings.ReplaceAll(m[n.window], ",", ""))
			}
		}
	}
	return facts.window, facts.input, true
}

// read walks one error object: its message, type and code words, the
// llama.cpp n_ctx and n_prompt_tokens fields, a nested error object, and
// the backend response Lemonade wraps under details.response.
func (f *overflowFacts) read(o map[string]json.RawMessage, depth int) {
	if depth > 3 {
		return
	}
	for _, key := range []string{"message", "error"} {
		var text string
		if json.Unmarshal(o[key], &text) == nil && text != "" {
			f.messages = append(f.messages, text)
		}
	}
	for _, key := range []string{"type", "code"} {
		var word string
		if json.Unmarshal(o[key], &word) == nil && overflowCodes[word] {
			f.coded = true
		}
	}
	if f.window == 0 {
		f.window = overflowCount(string(o["n_ctx"]))
	}
	if f.input == 0 {
		f.input = overflowCount(string(o["n_prompt_tokens"]))
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(o["error"], &nested) == nil {
		f.read(nested, depth+1)
	}
	var details struct {
		Response map[string]json.RawMessage `json:"response"`
	}
	if json.Unmarshal(o["details"], &details) == nil && details.Response != nil {
		f.read(details.Response, depth+1)
	}
}

// overflowCount accepts a positive decimal integer within maxOverflowTokens.
func overflowCount(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || n > maxOverflowTokens {
		return 0
	}
	return n
}

func overflowReason(window, input int64) string {
	return overflowReasonPrefix + strconv.FormatInt(window, 10) + ":" + strconv.FormatInt(input, 10)
}

// overflowed is the reply of a request the model server refused for its
// context window.
func overflowed(window, input int64) wire.Reply {
	return failed(wire.ReplyOutcomeRefused, overflowReason(window, input))
}

// withObservedWindow fills an overflow reason's unknown window with the
// router's latest effective-context observation of the model server and
// model that refused the request. It reads the cached survey and sends
// nothing upstream.
func (p *Provider) withObservedWindow(reason, host, model string) string {
	rest, ok := strings.CutPrefix(reason, overflowReasonPrefix+"0:")
	if !ok {
		return reason
	}
	families, _ := p.cfg.Router.Models(false)
	for _, family := range families {
		for _, alias := range family.Names {
			if alias.Host != host || alias.Name != model || alias.EffectiveContextLength <= 0 ||
				alias.ContextLength > 0 && alias.EffectiveContextLength > alias.ContextLength {
				continue
			}
			return overflowReasonPrefix + strconv.FormatInt(alias.EffectiveContextLength, 10) + ":" + rest
		}
	}
	return reason
}
