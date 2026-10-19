// Package kokoro holds the conditions of the Kokoros b17 security review
// (research/security-reviews/kokoros-b17-win.md) that every provider
// forwarding speech toward koko enforces the same way: the command line it
// admits (condition 3), the environment names the engine never receives
// (condition 4), the request the provider builds itself (condition 6) and the
// answer it accepts (condition 7). Inference CONTRACT.md INF-KO1..INF-KO4
// state them. A provider may refuse more on top.
package kokoro

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openabstractions/abstraction-inference/go/providerhost/wavfile"
)

// KokoroV1 is the only model the review approves: Lemonade's kokoro-v1
// (mikkoph/kokoro-onnx at the pinned revision).
const KokoroV1 = "kokoro-v1"

// SampleRate is the rate of Kokoros' 16-bit little-endian mono PCM.
const SampleRate = 24000

// MaxCharacters bounds the input, speech@1's own bound (INF-S6).
const MaxCharacters = 4096

// MaxPCMBytes bounds the PCM the provider reads (condition 7). The WAV
// the provider writes from it, 44 bytes longer, stays within speech@1's
// 64 MiB delivery.
const MaxPCMBytes = 64<<20 - wavfile.HeaderBytes

// Speed bounds (condition 6).
const (
	MinSpeed = 0.5
	MaxSpeed = 2.0
)

var (
	// ErrForbiddenFlag reports a koko command line the review refuses.
	ErrForbiddenFlag = errors.New("kokoros: command line outside the b17 review")
	// ErrSynthesis reports a request field outside the review's bounds.
	ErrSynthesis = errors.New("kokoros: request outside the b17 review")
	// ErrPCM reports an answer the provider refuses.
	ErrPCM = errors.New("kokoros: answer outside the b17 review")
)

// englishVoices are the American and British voices of the pinned
// voices-v1.0.bin (54 entries, Kokoro-82M v1.0): af_*, am_*, bf_* and bm_*.
// Their first letter fixes espeak-ng's language to en-us or en-gb, which
// keeps the other languages' dictionaries and the Vietnamese path of
// espeak-ng #2103 out of reach (K3).
var englishVoices = map[string]bool{}

func init() {
	for _, v := range strings.Fields(`af_alloy af_aoede af_bella af_heart af_jessica af_kore af_nicole af_nova af_river af_sarah af_sky
		am_adam am_echo am_eric am_fenrir am_liam am_michael am_onyx am_puck am_santa
		bf_alice bf_emma bf_isabella bf_lily bm_daniel bm_fable bm_george bm_lewis`) {
		englishVoices[v] = true
	}
}

// CheckVoice admits exactly one English voice name, never a `+` mix.
func CheckVoice(voice string) error {
	if !englishVoices[voice] {
		return fmt.Errorf("%w: voice %q is not an English voice of the pinned voices file", ErrSynthesis, voice)
	}
	return nil
}

// Voices lists the admitted voices in no particular order.
func Voices() []string {
	out := make([]string, 0, len(englishVoices))
	for v := range englishVoices {
		out = append(out, v)
	}
	return out
}

// Text returns the input the provider sends: carriage returns become line
// feeds, tabs become spaces, and format characters and variation selectors
// are dropped. It refuses invalid UTF-8, NUL and every other control
// character except line feed, and text NFC would change (combining marks,
// conjoining jamo and the characters NFC replaces), so the input is in NFC.
// The result holds 1..MaxCharacters characters.
func Text(input string) (string, error) {
	if !utf8.ValidString(input) {
		return "", fmt.Errorf("%w: input is not UTF-8", ErrSynthesis)
	}
	var b strings.Builder
	b.Grow(len(input))
	count := 0
	for i, r := range input {
		switch {
		case r == '\r':
			if i+1 < len(input) && input[i+1] == '\n' {
				continue
			}
			r = '\n'
		case r == '\t':
			r = ' '
		case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF, unicode.Is(unicode.Cf, r):
			continue
		}
		if r != '\n' && (r < 0x20 || r >= 0x7f && r <= 0x9f) {
			return "", fmt.Errorf("%w: input holds control character %U", ErrSynthesis, r)
		}
		if unicode.In(r, unicode.Mn, unicode.Mc, unicode.Me) || nfcChanges(r) {
			return "", fmt.Errorf("%w: input is not in NFC or holds combining mark %U", ErrSynthesis, r)
		}
		b.WriteRune(r)
		count++
	}
	if count < 1 || count > MaxCharacters {
		return "", fmt.Errorf("%w: input holds %d characters, not 1..%d", ErrSynthesis, count, MaxCharacters)
	}
	return b.String(), nil
}

// nfcChanges reports a starter NFC replaces or composes with a neighbour:
// conjoining jamo, the singleton decompositions and the composition
// exclusions of Unicode's CompositionExclusions.txt.
func nfcChanges(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x11FF, r >= 0xA960 && r <= 0xA97F, r >= 0xD7B0 && r <= 0xD7FF:
		return true
	case r >= 0xF900 && r <= 0xFAFF, r >= 0x2F800 && r <= 0x2FA1F:
		return true
	case r >= 0x0958 && r <= 0x095F, r >= 0x0A59 && r <= 0x0A5B, r >= 0xFB2A && r <= 0xFB4E, r >= 0x1D15E && r <= 0x1D164, r >= 0x1D1BB && r <= 0x1D1C0:
		return true
	case r >= 0x1F71 && r <= 0x1FFD && greekOxia[r]:
		return true
	}
	return nfcSingles[r]
}

var greekOxia = map[rune]bool{0x1F71: true, 0x1F73: true, 0x1F75: true, 0x1F77: true, 0x1F79: true, 0x1F7B: true, 0x1F7D: true,
	0x1FBB: true, 0x1FBE: true, 0x1FC9: true, 0x1FCB: true, 0x1FD3: true, 0x1FDB: true, 0x1FE3: true, 0x1FEB: true, 0x1FEE: true,
	0x1FEF: true, 0x1FF9: true, 0x1FFB: true, 0x1FFD: true}

var nfcSingles = map[rune]bool{0x0340: true, 0x0341: true, 0x0343: true, 0x0344: true, 0x0374: true, 0x037E: true, 0x0387: true,
	0x09DC: true, 0x09DD: true, 0x09DF: true, 0x0A33: true, 0x0A36: true, 0x0A5E: true, 0x0B5C: true, 0x0B5D: true,
	0x0F43: true, 0x0F4D: true, 0x0F52: true, 0x0F57: true, 0x0F5C: true, 0x0F69: true, 0x0F73: true, 0x0F75: true, 0x0F76: true,
	0x0F78: true, 0x0F81: true, 0x0F93: true, 0x0F9D: true, 0x0FA2: true, 0x0FA7: true, 0x0FAC: true, 0x0FB9: true,
	0x2000: true, 0x2001: true, 0x2126: true, 0x212A: true, 0x212B: true, 0x2329: true, 0x232A: true, 0x2ADC: true,
	0xFB1D: true, 0xFB1F: true}

// Synthesis is one speech request the provider sends, every field checked.
type Synthesis struct {
	Input string
	Voice string
	Speed float64
}

// NewSynthesis checks model, input (Text), voice (CheckVoice) and speed, a
// finite number in MinSpeed..MaxSpeed.
func NewSynthesis(model, input, voice string, speed float64) (Synthesis, error) {
	if model != KokoroV1 {
		return Synthesis{}, fmt.Errorf("%w: model %q", ErrSynthesis, model)
	}
	text, err := Text(input)
	if err != nil {
		return Synthesis{}, err
	}
	if err := CheckVoice(voice); err != nil {
		return Synthesis{}, err
	}
	if math.IsNaN(speed) || speed < MinSpeed || speed > MaxSpeed {
		return Synthesis{}, fmt.Errorf("%w: speed %v", ErrSynthesis, speed)
	}
	return Synthesis{Input: text, Voice: voice, Speed: speed}, nil
}

// Body is the JSON the provider sends, and the only fields it ever sends:
// model, input, voice, response_format pcm, speed and stream false. Never
// initial_silence (K1), lang_code (K2), stream_format, normalization or
// download options, or load options.
func (r Synthesis) Body() []byte {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	//unchecked: the value holds strings, a finite number and a bool, which always encode
	_ = enc.Encode(struct {
		Model          string  `json:"model"`
		Input          string  `json:"input"`
		Voice          string  `json:"voice"`
		ResponseFormat string  `json:"response_format"`
		Speed          float64 `json:"speed"`
		Stream         bool    `json:"stream"`
	}{KokoroV1, r.Input, r.Voice, "pcm", r.Speed, false})
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// ParseSynthesis reads an OpenAI-compatible speech body (as inference sends it):
// one JSON object with model, input, voice, response_format wav or pcm, and
// optionally speed, each once. It returns the request and the format the
// caller asked for. Every other field is refused, initial_silence,
// lang_code, stream, stream_format and load options included.
func ParseSynthesis(body []byte) (Synthesis, string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return Synthesis{}, "", fmt.Errorf("%w: the body is no JSON object", ErrSynthesis)
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return Synthesis{}, "", fmt.Errorf("%w: malformed object", ErrSynthesis)
		}
		switch key {
		case "model", "input", "voice", "response_format", "speed":
		default:
			return Synthesis{}, "", fmt.Errorf("%w: field %q", ErrSynthesis, key)
		}
		if _, dup := fields[key]; dup {
			return Synthesis{}, "", fmt.Errorf("%w: field %q given twice", ErrSynthesis, key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return Synthesis{}, "", fmt.Errorf("%w: field %q", ErrSynthesis, key)
		}
		fields[key] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return Synthesis{}, "", fmt.Errorf("%w: malformed object", ErrSynthesis)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Synthesis{}, "", fmt.Errorf("%w: the body holds more than one JSON value", ErrSynthesis)
	}
	str := func(key string) (string, error) {
		var s string
		if err := json.Unmarshal(fields[key], &s); err != nil {
			return "", fmt.Errorf("%w: %s is not a string", ErrSynthesis, key)
		}
		return s, nil
	}
	model, err := str("model")
	if err != nil {
		return Synthesis{}, "", err
	}
	input, err := str("input")
	if err != nil {
		return Synthesis{}, "", err
	}
	voice, err := str("voice")
	if err != nil {
		return Synthesis{}, "", err
	}
	format, err := str("response_format")
	if err != nil {
		return Synthesis{}, "", err
	}
	if format != "wav" && format != "pcm" {
		return Synthesis{}, "", fmt.Errorf("%w: response_format %q; the provider writes wav or pcm", ErrSynthesis, format)
	}
	speed := 1.0
	if raw, ok := fields["speed"]; ok {
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return Synthesis{}, "", fmt.Errorf("%w: speed is not a number", ErrSynthesis)
		}
		if speed, err = n.Float64(); err != nil {
			return Synthesis{}, "", fmt.Errorf("%w: speed", ErrSynthesis)
		}
	}
	r, err := NewSynthesis(model, input, voice, speed)
	return r, format, err
}

// ReadPCM reads an answer with status 200 whose media type is audio/pcm,
// or Lemonade's audio/l16 with rate 24000, little-endian and one channel when
// those parameters are present, and returns its PCM: an even, non-zero byte
// count of at most MaxPCMBytes. It closes the body.
func ReadPCM(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		//unchecked: best-effort drain; the status decides the refusal
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("%w: status %d", ErrPCM, resp.StatusCode)
	}
	media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "audio/pcm" && media != "audio/l16" {
		return nil, fmt.Errorf("%w: content type %q", ErrPCM, resp.Header.Get("Content-Type"))
	}
	if rate, ok := params["rate"]; ok && rate != strconv.Itoa(SampleRate) {
		return nil, fmt.Errorf("%w: rate %s", ErrPCM, rate)
	}
	if order, ok := params["endianness"]; ok && order != "little-endian" {
		return nil, fmt.Errorf("%w: %s samples", ErrPCM, order)
	}
	if channels, ok := params["channels"]; ok && channels != "1" {
		return nil, fmt.Errorf("%w: %s channels", ErrPCM, channels)
	}
	pcm, err := io.ReadAll(io.LimitReader(resp.Body, MaxPCMBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPCM, err)
	}
	if len(pcm) > MaxPCMBytes || len(pcm) == 0 || len(pcm)%2 != 0 {
		return nil, fmt.Errorf("%w: %d bytes of PCM", ErrPCM, len(pcm))
	}
	return pcm, nil
}

// WAV is the delivery the provider writes from koko's PCM: its own 44-byte
// header for 16-bit mono at SampleRate, since Kokoros' WAV header carries
// 0xFFFFFFFF sizes.
func WAV(pcm []byte) ([]byte, error) {
	return wavfile.Encode(wavfile.Format{Channels: 1, Rate: SampleRate}, pcm)
}

// InputDigest names an input in the provider's log (condition 8): its
// SHA-256, never its text.
func InputDigest(input string) string {
	sum := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CheckArgs admits koko's command line as Lemonade builds it:
// `-m <model> -d <voices> openai --ip 127.0.0.1 --port <port>`, optionally
// followed by `--instances 1` or `2`. The text, file and stream modes, which
// read standard input or write files, another address and any other option
// are refused.
func CheckArgs(args []string, model, voices string, port int) error {
	want := []string{"-m", model, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", strconv.Itoa(port)}
	if len(args) == len(want)+2 && args[len(want)] == "--instances" && (args[len(want)+1] == "1" || args[len(want)+1] == "2") {
		args = args[:len(want)]
	}
	if len(args) != len(want) {
		return fmt.Errorf("%w: %d words", ErrForbiddenFlag, len(args))
	}
	for i := range want {
		if args[i] != want[i] {
			return fmt.Errorf("%w: word %d is %q", ErrForbiddenFlag, i, args[i])
		}
	}
	return nil
}

// forbiddenPrefixes and forbiddenNames are review condition 4: PIPER_*
// (espeak-rs's data folder), ESPEAK* (espeak-ng's), ORT_* (ONNX Runtime),
// SSL_CERT_* (the TLS trust of K4's download) and the proxy variables.
var (
	forbiddenPrefixes = []string{"PIPER_", "ESPEAK", "ORT_", "SSL_CERT_"}
	forbiddenNames    = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true}
)

// ForbiddenEnvironment reports whether koko must never receive the variable
// name, in any letter case.
func ForbiddenEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	if forbiddenNames[upper] {
		return true
	}
	for _, prefix := range forbiddenPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
