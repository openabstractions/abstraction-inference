// Package whispercpp holds the conditions of the whisper.cpp v1.8.4 security
// review (research/security-reviews/whisper.cpp-v1.8.4-win.md) that every
// provider forwarding transcription toward whisper-server enforces the same
// way: the options it never passes (condition 3), the environment names the
// engine never receives (condition 4), the request the provider builds itself
// from audio it decoded (condition 7) and the answer it accepts (condition 8).
// Inference CONTRACT.md INF-WC1..INF-WC4 state them. A provider may refuse
// more on top.
package whispercpp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openabstractions/abstraction-inference/go/providerhost/wavfile"
)

// LargeV3Turbo is the only model the review approves: Lemonade's
// Whisper-Large-v3-Turbo, ggml-large-v3-turbo.bin at the pinned SHA-256.
const LargeV3Turbo = "Whisper-Large-v3-Turbo"

// FileName is the constant multipart file name of every request. cpp-httplib
// 0.20.0 parses the caller's file name with a std::regex that overflows the
// stack on crafted input (CVE-2026-29076).
const FileName = "audio.wav"

// MaxAudioBytes bounds the caller's WAV, transcription@1's input cap (INF-M5).
const MaxAudioBytes = 16 << 20

// MaxPromptBytes bounds the prompt field.
const MaxPromptBytes = 1024

// MaxTranscriptBytes bounds the answer the provider reads (condition 8).
const MaxTranscriptBytes = 4 << 20

var (
	// ErrForbiddenFlag reports a whisper-server option the review forbids.
	ErrForbiddenFlag = errors.New("whisper.cpp: option forbidden by the v1.8.4 review")
	// ErrAudio reports input audio other than 16-bit PCM WAV within the cap.
	ErrAudio = errors.New("whisper.cpp: audio must be 16-bit PCM WAV of at most 16 MiB")
	// ErrForm reports a request field outside the review's bounds.
	ErrForm = errors.New("whisper.cpp: request outside the v1.8.4 review")
	// ErrTranscript reports an answer the provider refuses.
	ErrTranscript = errors.New("whisper.cpp: answer outside the v1.8.4 review")
)

// forbiddenFlags are review condition 3's options: the listener address, the
// static file folder, the routes, ffmpeg conversion and its temporary folder,
// the VAD model path and the OpenVINO device.
var forbiddenFlags = map[string]bool{
	"--host": true, "--public": true, "--request-path": true, "--inference-path": true, "--convert": true,
	"--tmp-dir": true, "-vm": true, "--vad-model": true, "-oved": true, "--ov-e-device": true,
}

// CheckFlag refuses a word that names a forbidden option, alone or as
// `--option=value`, wrapping ErrForbiddenFlag.
func CheckFlag(word string) error {
	name, _, _ := strings.Cut(word, "=")
	if forbiddenFlags[name] {
		return fmt.Errorf("%w: %s", ErrForbiddenFlag, name)
	}
	return nil
}

// CheckArgs admits exactly `-m <model> --port <port>`, the command line
// Lemonade gives whisper-server when every whispercpp `*_args` value is empty.
func CheckArgs(args []string, model string, port int) error {
	for _, word := range args {
		if err := CheckFlag(word); err != nil {
			return err
		}
	}
	if len(args) != 4 || args[0] != "-m" || args[1] != model || args[2] != "--port" || args[3] != strconv.Itoa(port) {
		return fmt.Errorf("%w: the command line is not -m <pinned model> --port %d", ErrForbiddenFlag, port)
	}
	return nil
}

// forbiddenPrefixes and forbiddenNames are review condition 4: WHISPER_* (the
// GPU index), OVERRIDE_VITISAI_* (the NPU model path), KMP_* (OpenMP),
// GGML_*, HF_* and the proxy variables.
var (
	forbiddenPrefixes = []string{"WHISPER_", "OVERRIDE_VITISAI_", "KMP_", "GGML_", "HF_"}
	forbiddenNames    = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true}
)

// ForbiddenEnvironment reports whether whisper-server must never receive the
// variable name, in any letter case.
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

// NormalizeWAV decodes the caller's audio and returns the provider's own WAV
// of the same samples: RIFF/WAVE with format tag 1, 16-bit, mono or stereo,
// a RIFF size equal to the length minus 8 and at most MaxAudioBytes, re-emitted
// behind the canonical 44-byte header. Only dr_wav then parses the bytes, and
// they begin `RIFF` and a size whose high byte is zero, so read as a file
// path they end within seven characters (W1).
func NormalizeWAV(data []byte) ([]byte, error) {
	format, samples, err := wavfile.Decode(data, MaxAudioBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAudio, err)
	}
	out, err := wavfile.Encode(format, samples)
	if err != nil || len(out)-8 >= 1<<24 {
		return nil, fmt.Errorf("%w: re-encoding", ErrAudio)
	}
	return out, nil
}

// languages is whisper.cpp's language table (g_lang in src/whisper.cpp),
// large-v3's Cantonese included.
var languages = map[string]bool{}

func init() {
	for _, code := range strings.Fields(`en zh de es ru ko fr ja pt tr pl ca nl ar sv it id hi fi vi he uk el ms cs ro da hu ta
		no th ur hr bg lt la mi ml cy sk te fa lv bn sr az sl kn et mk br eu is hy ne mn bs kk sq sw gl mr pa si km sn
		yo so af oc ka be tg sd gu am yi lo uz fo ht ps tk nn mt sa lb my bo tl mg as tt haw ln ha ba jw su yue`) {
		languages[code] = true
	}
}

// Language maps a request language to whisper's: "" is "auto", and a tag
// such as en-US becomes its primary subtag, which must be in whisper's table.
func Language(tag string) (string, error) {
	if tag == "" || strings.EqualFold(tag, "auto") {
		return "auto", nil
	}
	primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
	if !languages[primary] {
		return "", fmt.Errorf("%w: language %q is not in whisper's table", ErrForm, tag)
	}
	return primary, nil
}

// Response formats the provider asks for.
var responseFormats = map[string]bool{"json": true, "verbose_json": true, "text": true}

// Form is one transcription the provider sends, every field checked.
type Form struct {
	Model          string
	Audio          []byte // the provider's WAV (NormalizeWAV)
	Language       string // auto or a code of whisper's table
	Prompt         string
	Temperature    *float64
	ResponseFormat string
}

// NewForm checks every field and re-encodes audio: the model must be
// LargeV3Turbo, the prompt at most MaxPromptBytes of UTF-8 without NUL, the
// temperature a finite number in 0..1, and the response format json,
// verbose_json or text.
func NewForm(model string, audio []byte, language, prompt string, temperature *float64, responseFormat string) (Form, error) {
	if model != LargeV3Turbo {
		return Form{}, fmt.Errorf("%w: model %q", ErrForm, model)
	}
	lang, err := Language(language)
	if err != nil {
		return Form{}, err
	}
	if len(prompt) > MaxPromptBytes || !utf8.ValidString(prompt) || strings.ContainsRune(prompt, 0) {
		return Form{}, fmt.Errorf("%w: prompt", ErrForm)
	}
	if temperature != nil && (math.IsNaN(*temperature) || *temperature < 0 || *temperature > 1) {
		return Form{}, fmt.Errorf("%w: temperature", ErrForm)
	}
	if !responseFormats[responseFormat] {
		return Form{}, fmt.Errorf("%w: response_format %q", ErrForm, responseFormat)
	}
	wav, err := NormalizeWAV(audio)
	if err != nil {
		return Form{}, fmt.Errorf("%w: %w", ErrForm, err)
	}
	r := Form{Model: model, Audio: wav, Language: lang, Prompt: prompt, ResponseFormat: responseFormat}
	if temperature != nil {
		t := *temperature
		r.Temperature = &t
	}
	return r, nil
}

// Body is the multipart form the provider sends: model, the WAV as file
// FileName of type audio/wav, response_format, language, and prompt and
// temperature when set. Nothing else reaches whisper-server through Lemonade.
func (r Form) Body() ([]byte, string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	//unchecked: every write below goes into the bytes.Buffer, whose Write never fails
	_ = mw.WriteField("model", r.Model)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+FileName+`"`)
	h.Set("Content-Type", "audio/wav")
	part, _ := mw.CreatePart(h)
	_, _ = part.Write(r.Audio)
	_ = mw.WriteField("response_format", r.ResponseFormat)
	_ = mw.WriteField("language", r.Language)
	if r.Prompt != "" {
		_ = mw.WriteField("prompt", r.Prompt)
	}
	if r.Temperature != nil {
		_ = mw.WriteField("temperature", strconv.FormatFloat(*r.Temperature, 'f', -1, 64))
	}
	_ = mw.Close()
	return body.Bytes(), mw.FormDataContentType()
}

// parsedFields are the fields ParseForm reads; timestamp_granularities[] is
// read and dropped, since whisper-server's verbose_json carries segments.
var parsedFields = map[string]bool{"model": true, "language": true, "prompt": true, "temperature": true, "response_format": true}

// ParseForm reads an OpenAI-compatible multipart transcription request (as
// inference sends it) and returns the Form built from it. Any field
// whisper-server reads beyond these (translate, offset_t, diarize, vad and
// the rest), a second file, a repeated field and any other body are refused.
func ParseForm(body []byte, contentType string) (Form, error) {
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/form-data" || params["boundary"] == "" {
		return Form{}, fmt.Errorf("%w: the body is not multipart/form-data", ErrForm)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	var audio []byte
	seenFile := false
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Form{}, fmt.Errorf("%w: multipart: %v", ErrForm, err)
		}
		name := part.FormName()
		limit := int64(4096)
		if name == "file" {
			limit = MaxAudioBytes
		}
		data, err := io.ReadAll(io.LimitReader(part, limit+1))
		if err != nil || int64(len(data)) > limit {
			return Form{}, fmt.Errorf("%w: field %q exceeds its bound", ErrForm, name)
		}
		switch {
		case name == "file":
			if seenFile {
				return Form{}, fmt.Errorf("%w: two files", ErrForm)
			}
			seenFile, audio = true, data
		case name == "timestamp_granularities[]":
			if v := string(data); v != "segment" && v != "word" {
				return Form{}, fmt.Errorf("%w: timestamp granularity", ErrForm)
			}
		case parsedFields[name]:
			if _, dup := fields[name]; dup {
				return Form{}, fmt.Errorf("%w: field %q given twice", ErrForm, name)
			}
			fields[name] = string(data)
		default:
			return Form{}, fmt.Errorf("%w: field %q", ErrForm, name)
		}
	}
	if !seenFile {
		return Form{}, fmt.Errorf("%w: no file", ErrForm)
	}
	var temperature *float64
	if raw, ok := fields["temperature"]; ok {
		t, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Form{}, fmt.Errorf("%w: temperature", ErrForm)
		}
		temperature = &t
	}
	format := fields["response_format"]
	if format == "" {
		format = "json"
	}
	return NewForm(fields["model"], audio, fields["language"], fields["prompt"], temperature, format)
}

// ReadTranscript reads an answer of at most MaxTranscriptBytes with status 200
// whose media type is application/json for json and verbose_json and
// text/plain for text, and closes its body.
func ReadTranscript(resp *http.Response, responseFormat string) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		//unchecked: best-effort drain; the status decides the refusal
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("%w: status %d", ErrTranscript, resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	want := "application/json"
	if responseFormat == "text" {
		want = "text/plain"
	}
	if err != nil || media != want {
		return nil, fmt.Errorf("%w: content type %q for %s", ErrTranscript, resp.Header.Get("Content-Type"), responseFormat)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxTranscriptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTranscript, err)
	}
	if len(data) > MaxTranscriptBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrTranscript, MaxTranscriptBytes)
	}
	return data, nil
}
