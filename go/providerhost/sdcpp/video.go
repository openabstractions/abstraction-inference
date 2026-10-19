package sdcpp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The request and result conditions of the video addendum: every body a
// provider sends to /sdcpp/v1/vid_gen is built from a typed preset with
// allow-listed keys and bounds (condition 7), and every result is read with
// caps and checked against what was asked (condition 9). Inference
// CONTRACT.md INF-SD6 and INF-SD7 state them.

// ErrVideoRequest reports a video request outside the reviewed keys or
// bounds.
var ErrVideoRequest = errors.New("sd.cpp: video request outside the reviewed bounds")

// ErrVideoResult reports a video result the provider refuses.
var ErrVideoResult = errors.New("sd.cpp: video result refused")

// Bounds of condition 7 and 9.
const (
	minVideoFrames = 5
	MaxVideoFrames = 121
	minVideoFPS    = 1
	MaxVideoFPS    = 30
	minVideoSteps  = 1
	maxVideoSteps  = 8
	maxTxtCfg      = 10
	maxFlowShift   = 20
	// MaxPromptBytes bounds prompt and negative_prompt each (finding V2).
	MaxPromptBytes = 2048
	// maxJobResponseBytes caps one read of GET /sdcpp/v1/jobs/{id}.
	maxJobResponseBytes = 100 << 20
	// maxContainerBytes caps one decoded container, the job executor's
	// output bound (inference MaxImageOutputBytes).
	maxContainerBytes = 64 << 20
	// MaxJobErrorBytes caps one error body the provider reads.
	MaxJobErrorBytes = 64 << 10
)

// jobResponseLimit and containerLimit are the caps ReadVideoJob and Container
// apply; tests lower them.
var (
	jobResponseLimit int64 = maxJobResponseBytes
	containerLimit         = maxContainerBytes
)

// videoSizes are the sizes a video request may name: Wan 2.2 TI2V 5B at
// 480p and 720p, landscape and portrait, and 512x512 for Hummingbird-XT
// image to video (video addendum amendment 1). A provider's model set
// narrows them to its own.
var videoSizes = [][2]int{{832, 480}, {480, 832}, {1280, 704}, {704, 1280}, {512, 512}}

// CheckVideoSize refuses a size outside videoSizes, wrapping ErrImageSize.
func CheckVideoSize(width, height int) error {
	for _, s := range videoSizes {
		if s[0] == width && s[1] == height {
			return nil
		}
	}
	return fmt.Errorf("%w: %dx%d is no reviewed video size", ErrImageSize, width, height)
}

// AlignFrames is the frame count sd-server produces for Wan: 4n+1, the
// nearest at or above frames, at least minVideoFrames.
func AlignFrames(frames int) int {
	if frames < minVideoFrames {
		return minVideoFrames
	}
	return (frames-1+3)/4*4 + 1
}

// Output formats and their MIME types as sd-server reports them.
const (
	FormatWebM = "webm"
	FormatAVI  = "avi"
)

// FormatMIME is the mime_type sd-server reports for a format.
func FormatMIME(format string) string {
	switch format {
	case FormatWebM:
		return "video/webm"
	case FormatAVI:
		return "video/x-msvideo"
	}
	return ""
}

// VideoPreset is a provider's fixed generation settings for one model
// (inference CONTRACT.md INF-G8). No caller field overrides any of them.
type VideoPreset struct {
	Steps        int
	TxtCfg       float64
	FlowShift    float64
	SampleMethod string
	Scheduler    string
	// FPS is the rate a request that names none gets.
	FPS int
	// Width and Height are the size a request of size auto gets.
	Width, Height int
	// NegativePrompt is sent with every request; empty omits it.
	NegativePrompt string
	// OutputFormat is webm or avi; OutputCompression is 0..100.
	OutputFormat      string
	OutputCompression int
	// Sigmas, when set, is the provider's own sigma table, sent as
	// custom_sigmas in place of the schedule Scheduler and FlowShift give:
	// Steps+1 values in [0, 1], strictly decreasing, the last 0 (video
	// addendum amendment 1). Only a preset sets it; no caller does.
	Sigmas []float64
}

// sampleMethods and schedulers are the names master-843 accepts
// (the library's sample_method_to_str and scheduler_to_str tables).
var (
	sampleMethods = map[string]bool{"euler": true, "euler_a": true, "heun": true, "dpm2": true, "dpm++2s_a": true,
		"dpm++2m": true, "dpm++2mv2": true, "ipndm": true, "ipndm_v": true, "lcm": true, "ddim_trailing": true,
		"tcd": true, "res_multistep": true, "res_2s": true, "er_sde": true, "euler_cfg_pp": true,
		"euler_a_cfg_pp": true, "euler_ge": true, "dpm++2m_sde": true, "dpm++2m_sde_bt": true, "lms": true}
	schedulers = map[string]bool{"discrete": true, "karras": true, "exponential": true, "ays": true, "gits": true,
		"sgm_uniform": true, "simple": true, "smoothstep": true, "kl_optimal": true, "lcm": true,
		"bong_tangent": true, "ltx2": true, "logit_normal": true, "flux2": true, "flux": true, "beta": true}
)

// Validate refuses a preset outside condition 7's bounds.
func (p VideoPreset) Validate() error {
	switch {
	case p.Steps < minVideoSteps || p.Steps > maxVideoSteps:
		return fmt.Errorf("%w: preset steps %d", ErrVideoRequest, p.Steps)
	case !finiteIn(p.TxtCfg, 0, maxTxtCfg):
		return fmt.Errorf("%w: preset guidance %v", ErrVideoRequest, p.TxtCfg)
	case !finiteIn(p.FlowShift, 0, maxFlowShift):
		return fmt.Errorf("%w: preset flow shift %v", ErrVideoRequest, p.FlowShift)
	case !sampleMethods[p.SampleMethod]:
		return fmt.Errorf("%w: preset sample method %q", ErrVideoRequest, p.SampleMethod)
	case !schedulers[p.Scheduler]:
		return fmt.Errorf("%w: preset scheduler %q", ErrVideoRequest, p.Scheduler)
	case p.FPS < minVideoFPS || p.FPS > MaxVideoFPS:
		return fmt.Errorf("%w: preset fps %d", ErrVideoRequest, p.FPS)
	case FormatMIME(p.OutputFormat) == "":
		return fmt.Errorf("%w: preset output format %q", ErrVideoRequest, p.OutputFormat)
	case p.OutputCompression < 0 || p.OutputCompression > 100:
		return fmt.Errorf("%w: preset compression %d", ErrVideoRequest, p.OutputCompression)
	}
	if err := CheckVideoSize(p.Width, p.Height); err != nil {
		return err
	}
	if p.Sigmas != nil {
		if err := checkSigmas(p.Sigmas, p.Steps); err != nil {
			return err
		}
	}
	return checkPrompt("negative_prompt", p.NegativePrompt, true)
}

// checkSigmas bounds a sigma table: steps+1 finite values in [0, 1],
// strictly decreasing, ending at 0. sd-server bounds neither the table's
// length nor its values, and takes its length as the step count.
func checkSigmas(sigmas []float64, steps int) error {
	if len(sigmas) != steps+1 || len(sigmas) < minVideoSteps+1 || len(sigmas) > maxVideoSteps+1 || sigmas[len(sigmas)-1] != 0 {
		return fmt.Errorf("%w: custom_sigmas %v for %d steps", ErrVideoRequest, sigmas, steps)
	}
	for i, v := range sigmas {
		if !finiteIn(v, 0, 1) || i > 0 && v >= sigmas[i-1] {
			return fmt.Errorf("%w: custom_sigmas %v", ErrVideoRequest, sigmas)
		}
	}
	return nil
}

func finiteIn(v, low, high float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= low && v <= high
}

// CheckPrompt bounds a video prompt as every request body this package
// builds bounds it (finding V2): not empty, at most MaxPromptBytes of UTF-8,
// and no control character, U+2028 or U+2029. Any engine that writes the
// prompt into its log applies it, so no prompt forges a log line.
func CheckPrompt(text string) error { return checkPrompt("prompt", text, false) }

// checkPrompt bounds one prompt field (finding V2).
func checkPrompt(field, text string, empty bool) error {
	if !empty && text == "" {
		return fmt.Errorf("%w: %s is empty", ErrVideoRequest, field)
	}
	if len(text) > MaxPromptBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrVideoRequest, field, MaxPromptBytes)
	}
	if !utf8.ValidString(text) {
		return fmt.Errorf("%w: %s is not UTF-8 text", ErrVideoRequest, field)
	}
	// sd-server writes the prompt into its log; a line break or other
	// control character there could split a log line and forge one,
	// including the progress bar the provider reads.
	if strings.IndexFunc(text, func(r rune) bool {
		return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
	}) >= 0 {
		return fmt.Errorf("%w: %s carries a control character or line break", ErrVideoRequest, field)
	}
	return nil
}

// VideoParams are the per-request values: the caller's prompt, the size and
// frame count the provider resolved, the rate, the seed, and an optional
// first frame the provider made with FirstFrame.
type VideoParams struct {
	Prompt        string
	Width, Height int
	Frames, FPS   int
	Seed          int64
	// InitImage is the provider's own PNG of exactly Width × Height; nil
	// sends none.
	InitImage []byte
}

// videoBody is the whole set of keys a provider sends (condition 7).
type videoBody struct {
	Prompt            string       `json:"prompt"`
	NegativePrompt    string       `json:"negative_prompt,omitempty"`
	Width             int          `json:"width"`
	Height            int          `json:"height"`
	VideoFrames       int          `json:"video_frames"`
	FPS               int          `json:"fps"`
	Seed              int64        `json:"seed"`
	SampleParams      sampleParams `json:"sample_params"`
	InitImage         string       `json:"init_image,omitempty"`
	OutputFormat      string       `json:"output_format"`
	OutputCompression int          `json:"output_compression"`
}

type sampleParams struct {
	SampleSteps  int       `json:"sample_steps"`
	SampleMethod string    `json:"sample_method"`
	Scheduler    string    `json:"scheduler"`
	FlowShift    float64   `json:"flow_shift"`
	Guidance     guidance  `json:"guidance"`
	CustomSigmas []float64 `json:"custom_sigmas,omitempty"`
}

type guidance struct {
	TxtCfg float64 `json:"txt_cfg"`
}

// MaxSeed is the largest seed a request carries (video@1's bound).
const MaxSeed = 1 << 53

// BuildVideoRequest encodes one /sdcpp/v1/vid_gen body from preset and
// params, refusing any value outside condition 7. The body carries only the
// allow-listed keys; CheckVideoRequest accepts every body this returns.
func BuildVideoRequest(preset VideoPreset, params VideoParams) ([]byte, error) {
	if err := preset.Validate(); err != nil {
		return nil, err
	}
	body := videoBody{Prompt: params.Prompt, NegativePrompt: preset.NegativePrompt, Width: params.Width, Height: params.Height,
		VideoFrames: params.Frames, FPS: params.FPS, Seed: params.Seed,
		SampleParams: sampleParams{SampleSteps: preset.Steps, SampleMethod: preset.SampleMethod, Scheduler: preset.Scheduler,
			FlowShift: preset.FlowShift, Guidance: guidance{TxtCfg: preset.TxtCfg}, CustomSigmas: preset.Sigmas},
		OutputFormat: preset.OutputFormat, OutputCompression: preset.OutputCompression}
	if params.InitImage != nil {
		body.InitImage = base64.StdEncoding.EncodeToString(params.InitImage)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVideoRequest, err)
	}
	raw := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if err := CheckVideoRequest(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// videoKeys and sampleKeys are the keys condition 7 admits.
var (
	videoKeys = map[string]bool{"prompt": true, "negative_prompt": true, "width": true, "height": true,
		"video_frames": true, "fps": true, "seed": true, "sample_params": true, "init_image": true,
		"output_format": true, "output_compression": true}
	sampleKeys = map[string]bool{"sample_steps": true, "sample_method": true, "scheduler": true,
		"flow_shift": true, "guidance": true, "custom_sigmas": true}
)

// CheckVideoRequest checks one body before it leaves the provider: one JSON
// object with only the admitted keys, each once and of its type, and every
// value within condition 7's bounds; an init_image is a base64 PNG whose
// header names exactly the request's width and height (condition 8).
// sample_params.custom_sigmas, a preset's own table, is admitted only within
// checkSigmas. Any other key, lora, hires, vae_tiling_params, cache options,
// extra_*, a top-level custom_sigmas, high_noise_*, the other image fields
// and embed_image_metadata among them, is refused by name.
func CheckVideoRequest(raw []byte) error {
	fields, err := strictObject(raw)
	if err != nil {
		return err
	}
	for key := range fields {
		if !videoKeys[key] {
			return fmt.Errorf("%w: key %q", ErrVideoRequest, key)
		}
	}
	var body videoBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("%w: %v", ErrVideoRequest, err)
	}
	sample, err := strictObject(fields["sample_params"])
	if err != nil {
		return err
	}
	for key := range sample {
		if !sampleKeys[key] {
			return fmt.Errorf("%w: sample_params key %q", ErrVideoRequest, key)
		}
	}
	guide, err := strictObject(sample["guidance"])
	if err != nil {
		return err
	}
	for key := range guide {
		if key != "txt_cfg" {
			return fmt.Errorf("%w: guidance key %q", ErrVideoRequest, key)
		}
	}
	for _, key := range []string{"prompt", "width", "height", "video_frames", "fps", "seed", "output_format", "output_compression"} {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("%w: key %q is missing", ErrVideoRequest, key)
		}
	}
	for _, key := range []string{"sample_steps", "sample_method", "scheduler", "flow_shift"} {
		if _, ok := sample[key]; !ok {
			return fmt.Errorf("%w: sample_params key %q is missing", ErrVideoRequest, key)
		}
	}
	if _, ok := guide["txt_cfg"]; !ok {
		return fmt.Errorf("%w: guidance key txt_cfg is missing", ErrVideoRequest)
	}
	if err := checkPrompt("prompt", body.Prompt, false); err != nil {
		return err
	}
	if err := checkPrompt("negative_prompt", body.NegativePrompt, true); err != nil {
		return err
	}
	if err := CheckVideoSize(body.Width, body.Height); err != nil {
		return err
	}
	s := body.SampleParams
	switch {
	case body.VideoFrames < minVideoFrames || body.VideoFrames > MaxVideoFrames || (body.VideoFrames-1)%4 != 0:
		return fmt.Errorf("%w: video_frames %d", ErrVideoRequest, body.VideoFrames)
	case body.FPS < minVideoFPS || body.FPS > MaxVideoFPS:
		return fmt.Errorf("%w: fps %d", ErrVideoRequest, body.FPS)
	case body.Seed < 0 || body.Seed > MaxSeed:
		return fmt.Errorf("%w: seed %d", ErrVideoRequest, body.Seed)
	case s.SampleSteps < minVideoSteps || s.SampleSteps > maxVideoSteps:
		return fmt.Errorf("%w: sample_steps %d", ErrVideoRequest, s.SampleSteps)
	case !finiteIn(s.Guidance.TxtCfg, 0, maxTxtCfg):
		return fmt.Errorf("%w: txt_cfg %v", ErrVideoRequest, s.Guidance.TxtCfg)
	case !finiteIn(s.FlowShift, 0, maxFlowShift):
		return fmt.Errorf("%w: flow_shift %v", ErrVideoRequest, s.FlowShift)
	case !sampleMethods[s.SampleMethod]:
		return fmt.Errorf("%w: sample_method %q", ErrVideoRequest, s.SampleMethod)
	case !schedulers[s.Scheduler]:
		return fmt.Errorf("%w: scheduler %q", ErrVideoRequest, s.Scheduler)
	case FormatMIME(body.OutputFormat) == "":
		return fmt.Errorf("%w: output_format %q", ErrVideoRequest, body.OutputFormat)
	case body.OutputCompression < 0 || body.OutputCompression > 100:
		return fmt.Errorf("%w: output_compression %d", ErrVideoRequest, body.OutputCompression)
	}
	if table, present := sample["custom_sigmas"]; present {
		// encoding/json reads a null element as 0, where nlohmann's
		// conversion to float throws; every element must be a number.
		var values []any
		if err := json.Unmarshal(table, &values); err != nil {
			return fmt.Errorf("%w: custom_sigmas is not an array", ErrVideoRequest)
		}
		for _, v := range values {
			if _, number := v.(float64); !number {
				return fmt.Errorf("%w: custom_sigmas holds %v", ErrVideoRequest, v)
			}
		}
		if err := checkSigmas(s.CustomSigmas, s.SampleSteps); err != nil {
			return err
		}
	}
	if _, present := fields["init_image"]; present {
		data, err := base64.StdEncoding.DecodeString(body.InitImage)
		if err != nil || len(data) == 0 {
			return fmt.Errorf("%w: init_image is not base64", ErrVideoRequest)
		}
		config, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || format != "png" || config.Width != body.Width || config.Height != body.Height {
			return fmt.Errorf("%w: init_image is not a PNG of exactly %dx%d", ErrVideoRequest, body.Width, body.Height)
		}
	}
	return nil
}

// strictObject decodes one JSON object and refuses a repeated key, which
// nlohmann's parser resolves differently from encoding/json.
func strictObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%w: not one JSON object", ErrVideoRequest)
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: not one JSON object", ErrVideoRequest)
		}
		key, _ := token.(string)
		if _, repeated := fields[key]; repeated {
			return nil, fmt.Errorf("%w: key %q is repeated", ErrVideoRequest, key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("%w: not one JSON object", ErrVideoRequest)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: not one JSON object", ErrVideoRequest)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data after the object", ErrVideoRequest)
	}
	return fields, nil
}

// VideoJob states sd-server reports.
const (
	JobQueued     = "queued"
	JobGenerating = "generating"
	JobCompleted  = "completed"
	JobFailed     = "failed"
	JobCancelled  = "cancelled"
)

// VideoJob is one GET /sdcpp/v1/jobs/{id} answer as the provider reads it. The
// error message is never read: engine text is not forwarded (condition 12).
type VideoJob struct {
	ID            string         `json:"id"`
	Status        string         `json:"status"`
	QueuePosition int            `json:"queue_position"`
	Result        *VideoResult   `json:"result"`
	Error         *VideoJobError `json:"error"`
}

// VideoResult is a completed video job's result.
type VideoResult struct {
	OutputFormat string `json:"output_format"`
	MimeType     string `json:"mime_type"`
	FPS          int    `json:"fps"`
	FrameCount   int    `json:"frame_count"`
	B64JSON      string `json:"b64_json"`
}

// VideoJobError carries only the engine's error code.
type VideoJobError struct {
	Code string `json:"code"`
}

// ReadVideoJob reads one job answer of at most maxJobResponseBytes.
func ReadVideoJob(r io.Reader) (VideoJob, error) {
	raw, err := io.ReadAll(io.LimitReader(r, jobResponseLimit+1))
	if err != nil {
		return VideoJob{}, err
	}
	if int64(len(raw)) > jobResponseLimit {
		return VideoJob{}, fmt.Errorf("%w: the job answer exceeds %d bytes", ErrVideoResult, jobResponseLimit)
	}
	var job VideoJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return VideoJob{}, fmt.Errorf("%w: the job answer is not JSON", ErrVideoResult)
	}
	switch job.Status {
	case JobQueued, JobGenerating, JobCompleted, JobFailed, JobCancelled:
	default:
		return VideoJob{}, fmt.Errorf("%w: job status %q", ErrVideoResult, job.Status)
	}
	return job, nil
}

// Container decodes and checks a completed job's container against the
// request (condition 9): format and mime_type as asked, frame_count and fps
// the aligned values sent, at most maxContainerBytes, and magic bytes of
// the format (EBML 1A 45 DF A3 for WebM, RIFF....AVI  for AVI).
func (r *VideoResult) Container(format string, frames, fps int) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: a completed job carries no result", ErrVideoResult)
	}
	mime := FormatMIME(format)
	switch {
	case mime == "" || r.OutputFormat != format || r.MimeType != mime:
		return nil, fmt.Errorf("%w: the result is %q (%q), not %s", ErrVideoResult, r.OutputFormat, r.MimeType, format)
	case r.FrameCount != frames:
		return nil, fmt.Errorf("%w: %d frames, not %d", ErrVideoResult, r.FrameCount, frames)
	case r.FPS != fps:
		return nil, fmt.Errorf("%w: %d fps, not %d", ErrVideoResult, r.FPS, fps)
	case base64.StdEncoding.DecodedLen(len(r.B64JSON)) > containerLimit+3:
		return nil, fmt.Errorf("%w: the container exceeds %d bytes", ErrVideoResult, containerLimit)
	}
	data, err := base64.StdEncoding.DecodeString(r.B64JSON)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("%w: the container is not base64", ErrVideoResult)
	}
	if len(data) > containerLimit {
		return nil, fmt.Errorf("%w: the container exceeds %d bytes", ErrVideoResult, containerLimit)
	}
	if !magic(format, data) {
		return nil, fmt.Errorf("%w: the container's magic bytes are not %s", ErrVideoResult, format)
	}
	return data, nil
}

// magic reports whether data starts as a container of format does.
func magic(format string, data []byte) bool {
	switch format {
	case FormatWebM:
		return len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1A, 0x45, 0xDF, 0xA3})
	case FormatAVI:
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "AVI "
	}
	return false
}
