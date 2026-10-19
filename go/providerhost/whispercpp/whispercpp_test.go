package whispercpp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// wav builds a RIFF/WAVE file with one fmt chunk, optional extra chunks
// before the data chunk, and the given PCM bytes.
func wav(tag, channels uint16, rate uint32, bits uint16, pcm []byte, extra ...[]byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(0))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, tag)
	binary.Write(&b, binary.LittleEndian, channels)
	binary.Write(&b, binary.LittleEndian, rate)
	binary.Write(&b, binary.LittleEndian, rate*uint32(channels)*uint32(bits/8))
	binary.Write(&b, binary.LittleEndian, channels*bits/8)
	binary.Write(&b, binary.LittleEndian, bits)
	for _, chunk := range extra {
		b.Write(chunk)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	out := b.Bytes()
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

func chunk(id string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(id)
	binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	if len(body)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// Condition 7: the provider re-emits the caller's PCM samples behind its own
// 44-byte header. Extra chunks (LIST metadata here) never reach the engine.
func TestNormalizeWAVReemitsSamplesBehindOwnHeader(t *testing.T) {
	pcm := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	in := wav(1, 2, 16000, 16, pcm, chunk("LIST", []byte("INFOISFT\x05\x00\x00\x00Lavf\x00\x00")))
	out, err := NormalizeWAV(in)
	if err != nil {
		t.Fatal(err)
	}
	want := wav(1, 2, 16000, 16, pcm)
	if !bytes.Equal(out, want) || len(out) != 44+len(pcm) {
		t.Fatalf("normalized % x\nwant        % x", out, want)
	}
	// W1: the bytes start RIFF and a size whose high byte is zero, so read
	// as a path they end within seven characters.
	if out[7] != 0 {
		t.Fatalf("RIFF size high byte %d", out[7])
	}
}

// Condition 7: only 16-bit PCM WAV, mono or stereo, with a RIFF size equal
// to the length minus 8 and at most 16 MiB, is accepted.
func TestNormalizeWAVRefusesOtherAudio(t *testing.T) {
	pcm := []byte{1, 0, 2, 0}
	good := wav(1, 1, 16000, 16, pcm)
	wrongSize := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(wrongSize[4:8], uint32(len(good)))
	trailing := append(append([]byte(nil), good...), 0, 0)
	extensible := wav(0xFFFE, 1, 16000, 16, pcm)
	float := wav(3, 1, 16000, 32, []byte{0, 0, 0, 0})
	eightBit := wav(1, 1, 16000, 8, []byte{1, 2})
	surround := wav(1, 6, 16000, 16, make([]byte, 12))
	oddData := wav(1, 2, 16000, 16, []byte{1, 0, 2, 0, 3, 0})
	noData := append([]byte(nil), good[:36]...)
	binary.LittleEndian.PutUint32(noData[4:8], uint32(len(noData)-8))
	huge := wav(1, 1, 16000, 16, make([]byte, MaxAudioBytes))
	for name, in := range map[string][]byte{
		"empty": nil, "mp3": []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), "flac": []byte("fLaC\x00\x00\x00\x22"),
		"path": []byte(`C:\Users\someone\secret.wav`), "unc": []byte(`\\host\share\x.wav`),
		"riff size": wrongSize, "trailing bytes": trailing, "extensible": extensible, "float": float, "8-bit": eightBit,
		"six channels": surround, "partial frame": oddData, "no data": noData, "over 16 MiB": huge,
	} {
		if _, err := NormalizeWAV(in); !errors.Is(err, ErrAudio) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Condition 7: the provider's multipart body carries the declared model, the
// constant file name audio.wav, the provider's WAV and bounded fields only.
func TestFormIsProviderBuilt(t *testing.T) {
	pcm := []byte{9, 0, 8, 0}
	temperature := 0.2
	r, err := NewForm(LargeV3Turbo, wav(1, 1, 22050, 16, pcm, chunk("junk", []byte{1})), "EN-us", "lighthouse", &temperature, "verbose_json")
	if err != nil {
		t.Fatal(err)
	}
	body, contentType := r.Body()
	fields, file, name := readMultipart(t, body, contentType)
	if name != FileName || !bytes.Equal(file, wav(1, 1, 22050, 16, pcm)) {
		t.Fatalf("file %q % x", name, file)
	}
	want := map[string]string{"model": LargeV3Turbo, "language": "en", "prompt": "lighthouse", "temperature": "0.2", "response_format": "verbose_json"}
	if len(fields) != len(want) {
		t.Fatalf("fields %v", fields)
	}
	for k, v := range want {
		if fields[k] != v {
			t.Fatalf("field %s = %q, want %q", k, fields[k], v)
		}
	}
	r, err = NewForm(LargeV3Turbo, wav(1, 1, 16000, 16, pcm), "", "", nil, "json")
	if err != nil {
		t.Fatal(err)
	}
	body, contentType = r.Body()
	fields, _, _ = readMultipart(t, body, contentType)
	if fields["language"] != "auto" || len(fields) != 3 {
		t.Fatalf("defaults %v", fields)
	}
}

func TestNewFormRefusesUnboundedFields(t *testing.T) {
	audio := wav(1, 1, 16000, 16, []byte{1, 0})
	hot, cold, nan := 1.5, -0.1, math.NaN()
	cases := map[string]func() error{
		"model":         func() error { _, err := NewForm("Whisper-Tiny", audio, "", "", nil, "json"); return err },
		"language":      func() error { _, err := NewForm(LargeV3Turbo, audio, "xx", "", nil, "json"); return err },
		"language path": func() error { _, err := NewForm(LargeV3Turbo, audio, "../en", "", nil, "json"); return err },
		"prompt length": func() error {
			_, err := NewForm(LargeV3Turbo, audio, "", strings.Repeat("a", 1025), nil, "json")
			return err
		},
		"prompt NUL":       func() error { _, err := NewForm(LargeV3Turbo, audio, "", "a\x00b", nil, "json"); return err },
		"prompt UTF-8":     func() error { _, err := NewForm(LargeV3Turbo, audio, "", "\xff", nil, "json"); return err },
		"temperature high": func() error { _, err := NewForm(LargeV3Turbo, audio, "", "", &hot, "json"); return err },
		"temperature low":  func() error { _, err := NewForm(LargeV3Turbo, audio, "", "", &cold, "json"); return err },
		"temperature NaN":  func() error { _, err := NewForm(LargeV3Turbo, audio, "", "", &nan, "json"); return err },
		"format":           func() error { _, err := NewForm(LargeV3Turbo, audio, "", "", nil, "srt"); return err },
		"audio":            func() error { _, err := NewForm(LargeV3Turbo, []byte("RIFF"), "", "", nil, "json"); return err },
	}
	for name, run := range cases {
		if err := run(); !errors.Is(err, ErrForm) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewForm(LargeV3Turbo, audio, "", strings.Repeat("é", 512), nil, "text"); err != nil {
		t.Fatalf("1024-byte prompt: %v", err)
	}
}

// The language table is whisper's own: 100 codes, large-v3's yue included.
func TestLanguageTable(t *testing.T) {
	if len(languages) != 100 || !languages["yue"] || !languages["haw"] || !languages["lv"] {
		t.Fatalf("%d languages", len(languages))
	}
}

// ParseForm reads inference's OpenAI-compatible multipart request and
// refuses every whisper-server field the review keeps out of reach.
func TestParseFormRefusesEngineFields(t *testing.T) {
	audio := wav(1, 1, 16000, 16, []byte{1, 0, 2, 0})
	build := func(fields [][2]string, filename string) ([]byte, string) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		part, _ := mw.CreateFormFile("file", filename)
		part.Write(audio)
		for _, f := range fields {
			mw.WriteField(f[0], f[1])
		}
		mw.Close()
		return b.Bytes(), mw.FormDataContentType()
	}
	body, contentType := build([][2]string{{"response_format", "verbose_json"}, {"model", LargeV3Turbo}, {"language", "lv"},
		{"timestamp_granularities[]", "segment"}, {"timestamp_granularities[]", "word"}}, `..\..\evil.mp3`)
	r, err := ParseForm(body, contentType)
	if err != nil {
		t.Fatal(err)
	}
	if r.Language != "lv" || r.ResponseFormat != "verbose_json" || !bytes.Equal(r.Audio, audio) {
		t.Fatalf("parsed %+v", r)
	}
	for _, field := range []string{"translate", "offset_t", "duration", "diarize", "vad", "detect_language", "token_timestamps", "tinydiarize", "file2"} {
		body, contentType := build([][2]string{{"model", LargeV3Turbo}, {field, "true"}}, "a.wav")
		if _, err := ParseForm(body, contentType); !errors.Is(err, ErrForm) {
			t.Errorf("%s: %v", field, err)
		}
	}
	body, contentType = build([][2]string{{"model", LargeV3Turbo}, {"model", LargeV3Turbo}}, "a.wav")
	if _, err := ParseForm(body, contentType); !errors.Is(err, ErrForm) {
		t.Errorf("duplicate model: %v", err)
	}
	if _, err := ParseForm([]byte(`{"model":"x"}`), "application/json"); !errors.Is(err, ErrForm) {
		t.Errorf("JSON body: %v", err)
	}
}

// Review condition 3: whisper-server's options that widen its reach are
// refused alone and as --option=value; the command line is exactly the
// pinned model and Lemonade's port.
func TestForbiddenFlagsAndArguments(t *testing.T) {
	for _, name := range []string{"--host", "--public", "--request-path", "--inference-path", "--convert", "--tmp-dir", "-vm", "--vad-model", "-oved", "--ov-e-device"} {
		for _, word := range []string{name, name + "=x"} {
			if err := CheckFlag(word); !errors.Is(err, ErrForbiddenFlag) {
				t.Fatalf("%q: %v", word, err)
			}
		}
	}
	if err := CheckFlag("--threads"); err != nil {
		t.Fatal(err)
	}
	model := `C:\p\models\ggml-large-v3-turbo.bin`
	if err := CheckArgs([]string{"-m", model, "--port", "8123"}, model, 8123); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-m", `C:\other.bin`, "--port", "8123"}, {"--model", model, "--port", "8123"}, {"-m", model, "--port", "9"},
		{"-m", model, "--port", "8123", "--convert"}, {"-m", model}, {"-m", model, "--port", "8123", "--threads", "4"},
	} {
		if err := CheckArgs(args, model, 8123); !errors.Is(err, ErrForbiddenFlag) {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// Review condition 4: WHISPER_*, OVERRIDE_VITISAI_*, KMP_*, GGML_*, HF_* and
// the proxy variables never reach whisper-server, in any letter case.
func TestForbiddenEnvironmentNames(t *testing.T) {
	for _, name := range []string{"WHISPER_ARG_DEVICE", "whisper_arg_device", "OVERRIDE_VITISAI_MODEL_PATH", "KMP_BLOCKTIME", "GGML_DISABLE_VULKAN",
		"GGML_BACKEND_PATH", "HF_TOKEN", "HTTP_PROXY", "https_proxy", "ALL_PROXY", "NO_PROXY"} {
		if !ForbiddenEnvironment(name) {
			t.Errorf("%s admitted", name)
		}
	}
	for _, name := range []string{"PATH", "SystemRoot", "TEMP", "LEMONADE_API_KEY", "KMPX"} {
		if ForbiddenEnvironment(name) {
			t.Errorf("%s refused", name)
		}
	}
}

// Condition 8: the answer is read with a 4 MiB cap and must be JSON for
// json and verbose_json and plain text for text.
func TestReadTranscript(t *testing.T) {
	reply := func(status int, contentType string, body []byte) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}
	}
	got, err := ReadTranscript(reply(200, "application/json; charset=utf-8", []byte(`{"text":"hi"}`)), "verbose_json")
	if err != nil || string(got) != `{"text":"hi"}` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ReadTranscript(reply(200, "text/plain", []byte("hi")), "text"); err != nil {
		t.Fatal(err)
	}
	for name, resp := range map[string]*http.Response{
		"html":         reply(200, "text/html", []byte("<p>")),
		"text as json": reply(200, "text/plain", []byte("hi")),
		"too large":    reply(200, "application/json", bytes.Repeat([]byte(" "), MaxTranscriptBytes+1)),
		"status":       reply(500, "application/json", []byte(`{}`)),
		"no type":      reply(200, "", []byte(`{}`)),
	} {
		format := "json"
		if _, err := ReadTranscript(resp, format); !errors.Is(err, ErrTranscript) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func readMultipart(t *testing.T, body []byte, contentType string) (map[string]string, []byte, string) {
	t.Helper()
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil || media != "multipart/form-data" {
		t.Fatalf("content type %q", contentType)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	var file []byte
	var name string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		if part.FormName() == "file" {
			file, name = data, part.FileName()
			if part.Header.Get("Content-Type") != "audio/wav" {
				t.Fatalf("file part type %q", part.Header.Get("Content-Type"))
			}
			continue
		}
		fields[part.FormName()] = string(data)
	}
	return fields, file, name
}
