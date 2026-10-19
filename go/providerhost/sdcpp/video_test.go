package sdcpp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func fastwanPreset() VideoPreset {
	return VideoPreset{Steps: 3, TxtCfg: 1, FlowShift: 3, SampleMethod: "euler", Scheduler: "discrete", FPS: 24,
		Width: 832, Height: 480, OutputFormat: FormatWebM, OutputCompression: 90}
}

func encodedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Condition 7: the body carries only the allow-listed keys, with the
// preset's sampling settings and the request's checked values.
func TestBuildVideoRequestSendsOnlyAllowListedKeys(t *testing.T) {
	frame := encodedPNG(t, 832, 480)
	raw, err := BuildVideoRequest(fastwanPreset(), VideoParams{Prompt: "a lighthouse", Width: 832, Height: 480, Frames: 49, FPS: 24, Seed: 7, InitImage: frame})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for key := range body {
		if !videoKeys[key] {
			t.Fatalf("unexpected key %s", key)
		}
	}
	sample := body["sample_params"].(map[string]any)
	if sample["sample_steps"] != float64(3) || sample["sample_method"] != "euler" || sample["flow_shift"] != float64(3) ||
		sample["guidance"].(map[string]any)["txt_cfg"] != float64(1) || body["video_frames"] != float64(49) || body["output_format"] != "webm" {
		t.Fatalf("body %s", raw)
	}
	if _, present := body["negative_prompt"]; present {
		t.Fatal("an empty negative prompt was sent")
	}
}

// Condition 7: every bound refuses, and every key the addendum names is
// refused by name when it reaches the check.
func TestVideoRequestBoundsAndForbiddenKeys(t *testing.T) {
	good := VideoParams{Prompt: "p", Width: 832, Height: 480, Frames: 49, FPS: 24, Seed: 1}
	for _, bad := range []func(*VideoParams){
		func(p *VideoParams) { p.Width, p.Height = 1024, 1024 },
		func(p *VideoParams) { p.Width, p.Height = 65536, 21846 },
		func(p *VideoParams) { p.Frames = 48 },
		func(p *VideoParams) { p.Frames = 125 },
		func(p *VideoParams) { p.Frames = 1 },
		func(p *VideoParams) { p.FPS = 31 },
		func(p *VideoParams) { p.FPS = 0 },
		func(p *VideoParams) { p.Prompt = "" },
		func(p *VideoParams) { p.Prompt = strings.Repeat("a", MaxPromptBytes+1) },
		// A prompt sd-server echoes into its log cannot split a line and
		// forge the progress bar the provider reads.
		func(p *VideoParams) { p.Prompt = "a cat\n  |" + strings.Repeat("=", 50) + "| 59/60 - 1.00s/it" },
		func(p *VideoParams) { p.Prompt = "a cat\r  |=>| 2/3" },
		func(p *VideoParams) { p.Prompt = "a cat next line" },
		func(p *VideoParams) { p.Prompt = "a cat\x1b[2K" },
		func(p *VideoParams) { p.Prompt = "a\tcat" },
		func(p *VideoParams) { p.Seed = -1 },
		func(p *VideoParams) { p.InitImage = encodedPNG(t, 64, 64) },
		func(p *VideoParams) { p.InitImage = []byte("GIF89a") },
	} {
		p := good
		bad(&p)
		if _, err := BuildVideoRequest(fastwanPreset(), p); err == nil {
			t.Fatalf("%+v built", p)
		} else if Reason(err) == "" {
			t.Fatalf("%+v: %v has no reason word", p, err)
		}
	}
	if _, err := BuildVideoRequest(fastwanPreset(), VideoParams{Prompt: strings.Repeat("é", MaxPromptBytes/2), Width: 480, Height: 832, Frames: 121, FPS: 30}); err != nil {
		t.Fatalf("the largest admitted request: %v", err)
	}
	for _, preset := range []func(*VideoPreset){
		func(p *VideoPreset) { p.Steps = 9 }, func(p *VideoPreset) { p.TxtCfg = 11 }, func(p *VideoPreset) { p.FlowShift = -1 },
		func(p *VideoPreset) { p.SampleMethod = "nope" }, func(p *VideoPreset) { p.OutputFormat = "webp" },
		func(p *VideoPreset) { p.OutputCompression = 101 },
	} {
		p := fastwanPreset()
		preset(&p)
		if _, err := BuildVideoRequest(p, good); !errors.Is(err, ErrVideoRequest) {
			t.Fatalf("preset %+v built: %v", p, err)
		}
	}
	raw, err := BuildVideoRequest(fastwanPreset(), good)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"lora", "hires", "vae_tiling_params", "cache_mode", "extra_sample_args", "custom_sigmas",
		"high_noise_sample_params", "ad_model", "end_image", "ref_images", "control_frames", "mask_image", "control_image",
		"ip_adapter_image", "embed_image_metadata", "batch_count"} {
		extended := append(bytes.Clone(bytes.TrimSuffix(raw, []byte("}"))), []byte(`,"`+key+`":[]}`)...)
		if err := CheckVideoRequest(extended); !errors.Is(err, ErrVideoRequest) || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: %v", key, err)
		}
	}
	nested := bytes.Replace(raw, []byte(`"sample_steps":3`), []byte(`"sample_steps":3,"custom_sigmas":[1]`), 1)
	if err := CheckVideoRequest(nested); !errors.Is(err, ErrVideoRequest) {
		t.Fatalf("nested key: %v", err)
	}
	repeated := bytes.Replace(raw, []byte(`"fps":24`), []byte(`"fps":24,"fps":120`), 1)
	if err := CheckVideoRequest(repeated); !errors.Is(err, ErrVideoRequest) {
		t.Fatalf("repeated key: %v", err)
	}
}

func TestAlignFrames(t *testing.T) {
	for in, want := range map[int]int{0: 5, 5: 5, 6: 9, 48: 49, 49: 49, 120: 121, 121: 121} {
		if got := AlignFrames(in); got != want {
			t.Fatalf("AlignFrames(%d) = %d", in, got)
		}
	}
}

func webm() []byte { return append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 60)...) }

// Condition 9: format, MIME, frame count, fps, size and magic bytes.
func TestJobResultContainerChecks(t *testing.T) {
	lowerLimits(t)
	ok := VideoResult{OutputFormat: "webm", MimeType: "video/webm", FPS: 24, FrameCount: 49, B64JSON: base64.StdEncoding.EncodeToString(webm())}
	if data, err := ok.Container("webm", 49, 24); err != nil || !bytes.Equal(data, webm()) {
		t.Fatalf("%v", err)
	}
	riff := append([]byte("RIFF\x00\x00\x00\x00AVI LIST"), make([]byte, 32)...)
	for _, bad := range []func(*VideoResult){
		func(r *VideoResult) { r.MimeType = "video/mp4" },
		func(r *VideoResult) { r.OutputFormat = "avi" },
		func(r *VideoResult) { r.FrameCount = 48 },
		func(r *VideoResult) { r.FPS = 16 },
		func(r *VideoResult) { r.B64JSON = "!!" },
		func(r *VideoResult) { r.B64JSON = base64.StdEncoding.EncodeToString(riff) },
		func(r *VideoResult) { r.B64JSON = strings.Repeat("A", (containerLimit/3+2)*4) },
	} {
		r := ok
		bad(&r)
		if _, err := r.Container("webm", 49, 24); !errors.Is(err, ErrVideoResult) {
			t.Fatalf("%+v accepted: %v", r.MimeType, err)
		}
	}
	avi := VideoResult{OutputFormat: "avi", MimeType: "video/x-msvideo", FPS: 24, FrameCount: 49, B64JSON: base64.StdEncoding.EncodeToString(riff)}
	if _, err := avi.Container("avi", 49, 24); err != nil {
		t.Fatal(err)
	}
	var nilResult *VideoResult
	if _, err := nilResult.Container("webm", 49, 24); !errors.Is(err, ErrVideoResult) {
		t.Fatal("nil result accepted")
	}
}

// lowerLimits caps answers and containers at 1 MiB for one test.
func lowerLimits(t *testing.T) {
	answers, containers := jobResponseLimit, containerLimit
	jobResponseLimit, containerLimit = 1<<20, 1<<20
	t.Cleanup(func() { jobResponseLimit, containerLimit = answers, containers })
}

func TestReadJobBoundsAndStates(t *testing.T) {
	lowerLimits(t)
	job, err := ReadVideoJob(strings.NewReader(`{"id":"job_1","status":"queued","queue_position":2,"result":null,"error":null}`))
	if err != nil || job.QueuePosition != 2 {
		t.Fatalf("%+v %v", job, err)
	}
	if _, err := ReadVideoJob(strings.NewReader(`{"status":"exploded"}`)); !errors.Is(err, ErrVideoResult) {
		t.Fatal("unknown state accepted")
	}
	huge := strings.NewReader(`{"status":"completed","result":{"b64_json":"` + strings.Repeat("A", int(jobResponseLimit)) + `"}}`)
	if _, err := ReadVideoJob(huge); !errors.Is(err, ErrVideoResult) {
		t.Fatal("oversized answer accepted")
	}
}

// Condition 8: the provider decodes PNG or JPEG itself and sends its own PNG
// of exactly the request's size.
func TestFirstFrameIsTheProvidersOwnPNGOfTheExactSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	for y := 0; y < 1000; y++ {
		for x := 0; x < 1000; x++ {
			src.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, src, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		media string
		data  []byte
		w, h  int
	}{{"image/jpeg", jpg.Bytes(), 832, 480}, {"image/png", encodedPNG(t, 64, 40), 1280, 704}, {"image/png", encodedPNG(t, 300, 2000), 480, 832}} {
		out, err := FirstFrame(tc.media, tc.data, tc.w, tc.h)
		if err != nil {
			t.Fatal(err)
		}
		config, format, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || format != "png" || config.Width != tc.w || config.Height != tc.h {
			t.Fatalf("%s: %v %s %dx%d", tc.media, err, format, config.Width, config.Height)
		}
		raw, err := BuildVideoRequest(fastwanPreset(), VideoParams{Prompt: "p", Width: tc.w, Height: tc.h, Frames: 49, FPS: 24, InitImage: out})
		if err != nil || CheckVideoRequest(raw) != nil {
			t.Fatalf("the provider's own frame was refused: %v", err)
		}
	}
	// A whole GIF, decodable here: this test binary registers Go's GIF
	// decoder, so only the media check refuses it.
	var gifData bytes.Buffer
	if err := gif.Encode(&gifData, image.NewPaletted(image.Rect(0, 0, 8, 8), color.Palette{color.Black, color.White}), nil); err != nil {
		t.Fatal(err)
	}
	gifBytes := gifData.Bytes()
	for _, bad := range []struct {
		media string
		data  []byte
	}{
		{"image/webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")},
		{"image/gif", gifBytes},
		{"image/png", gifBytes},
		{"image/png", jpg.Bytes()},
		{"image/png", encodedPNG(t, 4097, 1)},
		{"image/png", nil},
		{"image/png", make([]byte, MaxFirstFrameBytes+1)},
	} {
		if _, err := FirstFrame(bad.media, bad.data, 832, 480); !errors.Is(err, ErrFirstFrame) || Reason(err) != ReasonFirstFrame {
			t.Fatalf("%s accepted: %v", bad.media, err)
		}
	}
	if _, err := FirstFrame("image/png", encodedPNG(t, 8, 8), 1024, 1024); !errors.Is(err, ErrImageSize) {
		t.Fatal("an unreviewed size was accepted")
	}
}

// Video addendum amendment 1: a preset's own sigma table travels as
// sample_params.custom_sigmas, bounded to steps+1 values in [0, 1],
// strictly decreasing and ending at 0; any other table is refused, and
// 512x512 is a reviewed size.
func TestVideoPresetSigmaTable(t *testing.T) {
	preset := VideoPreset{Steps: 3, TxtCfg: 1, FlowShift: 10, SampleMethod: "lcm", Scheduler: "lcm", FPS: 16,
		Width: 512, Height: 512, OutputFormat: FormatWebM, OutputCompression: 90, Sigmas: []float64{1, 0.967742, 0.909091, 0}}
	raw, err := BuildVideoRequest(preset, VideoParams{Prompt: "a lighthouse", Width: 512, Height: 512, Frames: 81, FPS: 16, Seed: 7,
		InitImage: encodedPNG(t, 512, 512)})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Sample struct {
			Steps  int       `json:"sample_steps"`
			Method string    `json:"sample_method"`
			Sigmas []float64 `json:"custom_sigmas"`
		} `json:"sample_params"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Sample.Steps != 3 || body.Sample.Method != "lcm" || len(body.Sample.Sigmas) != 4 || body.Sample.Sigmas[1] != 0.967742 {
		t.Fatalf("body %s", raw)
	}
	if without, err := BuildVideoRequest(fastwanPreset(), VideoParams{Prompt: "p", Width: 832, Height: 480, Frames: 49, FPS: 24}); err != nil || bytes.Contains(without, []byte("custom_sigmas")) {
		t.Fatalf("a preset without a table sent one: %s %v", without, err)
	}
	for _, sigmas := range [][]float64{
		{1, 0.9, 0},           // one value short of steps+1
		{1, 0.9, 0.8, 0.7, 0}, // one value over
		{1, 0.9, 0.95, 0},     // not decreasing
		{1, 0.9, 0.8, 0.1},    // does not end at 0
		{1.5, 0.9, 0.8, 0},    // above 1
		{1, 0.9, -0.1, 0},     // below 0
		{1, 0.9, 0.8, 0, 0},   // repeated 0
		{},                    // empty
	} {
		p := preset
		p.Sigmas = sigmas
		if _, err := BuildVideoRequest(p, VideoParams{Prompt: "p", Width: 512, Height: 512, Frames: 81, FPS: 16}); !errors.Is(err, ErrVideoRequest) {
			t.Fatalf("sigmas %v built: %v", sigmas, err)
		}
	}
	for _, table := range []string{`[1,0.5,0]`, `[1,0.9,0.8,0.7,0.6,0.5,0.4,0.3,0.2,0]`, `"1,0.5,0"`, `[1,0.9,0.8,null]`} {
		forged := bytes.Replace(raw, []byte(`"custom_sigmas":[1,0.967742,0.909091,0]`), []byte(`"custom_sigmas":`+table), 1)
		if bytes.Equal(forged, raw) {
			t.Fatal("the table was not replaced")
		}
		if err := CheckVideoRequest(forged); err == nil {
			t.Fatalf("custom_sigmas %s passed", table)
		}
	}
}
