package kokoro

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

// Condition 6: only the English voices of the pinned voices file are
// admitted, never a mix, never an OpenAI name Kokoros would map.
func TestVoicesAreEnglishOnly(t *testing.T) {
	if len(englishVoices) != 28 {
		t.Fatalf("%d English voices", len(englishVoices))
	}
	for voice := range englishVoices {
		if !strings.HasPrefix(voice, "af_") && !strings.HasPrefix(voice, "am_") && !strings.HasPrefix(voice, "bf_") && !strings.HasPrefix(voice, "bm_") {
			t.Fatalf("%s is not an English voice", voice)
		}
	}
	for _, voice := range []string{"af_sky", "am_echo", "bf_emma", "bm_george"} {
		if err := CheckVoice(voice); err != nil {
			t.Fatalf("%s: %v", voice, err)
		}
	}
	for _, voice := range []string{"alloy", "af_sky+af_bella", "af_sky.3+bm_lewis.7", "jf_alpha", "zm_yunyang", "ef_dora", "hf_alpha", "AF_SKY", "af_sky ", "../voices/x", ""} {
		if err := CheckVoice(voice); !errors.Is(err, ErrSynthesis) {
			t.Fatalf("%q: %v", voice, err)
		}
	}
}

// Condition 6: input is 1..4096 characters of NFC text without NUL or other
// control characters except line feed. Carriage returns and tabs become line
// feeds and spaces; variation selectors and other format characters carry no
// speech and are dropped.
func TestText(t *testing.T) {
	got, err := Text("Line one\r\nline\ttwo\rthree ✔\uFE0F\u200Bdone")
	if err != nil || got != "Line one\nline two\nthree ✔done" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := Text("café"); err != nil || got != "café" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Text(strings.Repeat("a", 4096)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty": "", "only format": "\u200B\uFE0F", "too long": strings.Repeat("é", 4097), "NUL": "a\x00b", "escape": "a\x1bb",
		"DEL": "a\x7fb", "C1": "a\u0085b", "invalid": "\xff", "combining": "cafe\u0301", "jamo": "\u1100\u1161",
		"singleton": "\u212B", "exclusion": "\u0958", "Tamil vowel sign": "\u0BC6\u0BBE",
	} {
		if _, err := Text(in); !errors.Is(err, ErrSynthesis) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Condition 6: the provider's JSON body names exactly model, input, voice,
// response_format pcm, speed and stream false.
func TestSynthesisBody(t *testing.T) {
	r, err := NewSynthesis(KokoroV1, "Hello\tthere", "af_sky", 1.25)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body(), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": KokoroV1, "input": "Hello there", "voice": "af_sky", "response_format": "pcm", "speed": 1.25, "stream": false}
	if len(body) != len(want) {
		t.Fatalf("body %v", body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("%s = %v, want %v", k, body[k], v)
		}
	}
	for _, speed := range []float64{0.49, 2.01, 0, -1, math.NaN(), math.Inf(1)} {
		if _, err := NewSynthesis(KokoroV1, "hi", "af_sky", speed); !errors.Is(err, ErrSynthesis) {
			t.Fatalf("speed %v: %v", speed, err)
		}
	}
	if _, err := NewSynthesis("kokoro", "hi", "af_sky", 1); !errors.Is(err, ErrSynthesis) {
		t.Fatalf("model: %v", err)
	}
}

// Conditions 6 (K1, K2): ParseBody reads inference's OpenAI-compatible body
// and refuses initial_silence, lang_code, streaming, load options and every
// other field.
func TestParseSynthesisRefusesEngineFields(t *testing.T) {
	r, format, err := ParseSynthesis([]byte(`{"model":"kokoro-v1","voice":"bm_lewis","input":"Hi","response_format":"wav"}`))
	if err != nil || format != "wav" || r.Voice != "bm_lewis" || r.Input != "Hi" || r.Speed != 1 {
		t.Fatalf("%+v %q %v", r, format, err)
	}
	if _, format, err := ParseSynthesis([]byte(`{"model":"kokoro-v1","voice":"bm_lewis","input":"Hi","response_format":"pcm","speed":0.5}`)); err != nil || format != "pcm" {
		t.Fatalf("pcm: %q %v", format, err)
	}
	for _, body := range []string{
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","initial_silence":100000}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","lang_code":"../../x"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","stream":true}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","stream_format":"audio"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","normalization_options":{}}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","return_download_link":true}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","download_format":"mp3"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","volume_multiplier":9}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","kokoro_args":"-x"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"mp3"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","speed":"fast"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav","model":"other"}`,
		`{"model":"kokoro-v1","voice":"af_sky","input":"Hi","response_format":"wav"} {}`,
		`[]`,
	} {
		if _, _, err := ParseSynthesis([]byte(body)); !errors.Is(err, ErrSynthesis) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// Condition 7: the answer is PCM by content type, of an even byte count, at
// most the cap; the provider writes the WAV header itself.
func TestReadPCMAndWAV(t *testing.T) {
	reply := func(status int, contentType string, body []byte) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}
	}
	for _, contentType := range []string{"audio/pcm", "audio/l16;rate=24000;endianness=little-endian", "audio/L16; rate=24000; channels=1"} {
		pcm, err := ReadPCM(reply(200, contentType, []byte{1, 0, 2, 0}))
		if err != nil || !bytes.Equal(pcm, []byte{1, 0, 2, 0}) {
			t.Fatalf("%s: % x %v", contentType, pcm, err)
		}
	}
	for name, resp := range map[string]*http.Response{
		"wav":        reply(200, "audio/wav", []byte{1, 0}),
		"mp3":        reply(200, "audio/mpeg", []byte{1, 0}),
		"rate":       reply(200, "audio/l16;rate=44100", []byte{1, 0}),
		"big endian": reply(200, "audio/l16;rate=24000;endianness=big-endian", []byte{1, 0}),
		"stereo":     reply(200, "audio/l16;rate=24000;channels=2", []byte{1, 0}),
		"odd":        reply(200, "audio/pcm", []byte{1, 0, 2}),
		"empty":      reply(200, "audio/pcm", nil),
		"status":     reply(500, "audio/pcm", []byte{1, 0}),
		"too large":  reply(200, "audio/pcm", make([]byte, MaxPCMBytes+2)),
	} {
		if _, err := ReadPCM(resp); !errors.Is(err, ErrPCM) {
			t.Errorf("%s: %v", name, err)
		}
	}
	wav, err := WAV([]byte{1, 0, 2, 0})
	if err != nil || len(wav) != 48 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" ||
		binary.LittleEndian.Uint32(wav[4:8]) != 40 || binary.LittleEndian.Uint32(wav[24:28]) != SampleRate ||
		binary.LittleEndian.Uint16(wav[22:24]) != 1 || binary.LittleEndian.Uint32(wav[40:44]) != 4 {
		t.Fatalf("% x %v", wav, err)
	}
	if MaxPCMBytes+44 > 64<<20 {
		t.Fatal("the WAV of the largest answer exceeds speech@1's 64 MiB delivery")
	}
}

// Condition 8: the log names the input by digest, never by text.
func TestInputDigest(t *testing.T) {
	d := InputDigest("secret words")
	if !strings.HasPrefix(d, "sha256:") || len(d) != 71 || strings.Contains(d, "secret") {
		t.Fatal(d)
	}
}

// Review condition 3: koko's command line is Lemonade's openai mode on
// loopback; the file, text and stream modes, another address and more than
// two instances are refused.
func TestCheckArgs(t *testing.T) {
	onnx, voices := `C:\m\kokoro-v1.0.onnx`, `C:\m\voices-v1.0.bin`
	if err := CheckArgs([]string{"-m", onnx, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", "9000"}, onnx, voices, 9000); err != nil {
		t.Fatal(err)
	}
	if err := CheckArgs([]string{"-m", onnx, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", "9000", "--instances", "2"}, onnx, voices, 9000); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-m", onnx, "-d", voices, "openai", "--ip", "0.0.0.0", "--port", "9000"},
		{"-m", onnx, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", "9000", "--instances", "3"},
		{"-m", onnx, "-d", voices, "text", "hello"},
		{"-m", onnx, "-d", voices, "file", "in.txt"},
		{"-m", onnx, "-d", voices, "stream"},
		{"-m", `C:\other.onnx`, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", "9000"},
		{"-m", onnx, "-d", voices, "openai", "--port", "9000"},
		{"-m", onnx, "-d", voices, "openai", "--ip", "127.0.0.1", "--port", "9001"},
	} {
		if err := CheckArgs(args, onnx, voices, 9000); !errors.Is(err, ErrForbiddenFlag) {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// Review condition 4: PIPER_*, ESPEAK*, ORT_*, SSL_CERT_* and the proxy
// variables never reach koko, in any letter case.
func TestForbiddenEnvironmentNames(t *testing.T) {
	for _, name := range []string{"PIPER_ESPEAKNG_DATA_DIRECTORY", "ESPEAK_DATA_PATH", "espeak_data_path", "ESPEAKNG_X", "ORT_LOGGING_LEVEL", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "https_proxy", "ALL_PROXY", "NO_PROXY"} {
		if !ForbiddenEnvironment(name) {
			t.Errorf("%s admitted", name)
		}
	}
	for _, name := range []string{"PATH", "SystemRoot", "TEMP", "LEMONADE_API_KEY", "PIPERX"} {
		if ForbiddenEnvironment(name) {
			t.Errorf("%s refused", name)
		}
	}
}
