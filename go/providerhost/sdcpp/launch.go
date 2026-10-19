package sdcpp

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The launch conditions of the video addendum: the sd-server command line a
// provider builds (condition 3), the options it never passes (condition 4)
// and the environment names the engine never receives (condition 5).
// Inference CONTRACT.md INF-SD4 and INF-SD5 state them.

// ErrForbiddenFlag reports an sd-server option the video addendum forbids by
// name (condition 4).
var ErrForbiddenFlag = errors.New("sd.cpp: option forbidden by the master-843 video addendum")

// ErrArgument reports an sd-server argument outside the reviewed list, a
// value outside its reviewed form, or an option the provider sets itself
// (condition 3).
var ErrArgument = errors.New("sd.cpp: sd-server argument outside the reviewed list")

// Loopback is the only address sd-server is told to listen on.
const Loopback = "127.0.0.1"

// forbiddenFlags are condition 4's options in every spelling master-843's
// common option table and server runtime accept. -l and
// --listen-ip are refused separately unless their value is Loopback.
var forbiddenFlags = map[string]bool{
	"-M": true, "--mode": true, "--rpc-servers": true, "--serve-html-path": true, "--lora-model-dir": true,
	"--hires-upscalers-dir": true, "--embd-dir": true, "--upscale-model": true, "--control-net": true,
	"--ip-adapter": true, "--clip_vision": true, "--photo-maker": true, "--pulid-weights": true,
	"--motion-module": true, "-m": true, "--model": true, "--vae": true, "--clip_l": true, "--clip_g": true,
	"--llm": true, "--llm_vision": true, "--qwen2vl": true, "--qwen2vl_vision": true,
	"--high-noise-diffusion-model": true, "--uncond-diffusion-model": true, "--embeddings-connectors": true,
	"--audio-vae": true, "--mmap": true, "--params-backend": true, "--tensor-type-rules": true,
	"--model-args": true, "--prompt-file": true, "--negative-prompt-file": true, "-i": true, "--init-img": true,
	"--end-img": true, "--mask": true, "--control-image": true, "--ip-adapter-image": true,
	"--control-video": true, "-r": true, "--ref-image": true, "--ref-video": true, "--ref-video-audio": true,
	"--ref-audio": true, "--pm-id-images-dir": true, "--pm-id-embed-path": true, "--pulid-id-embedding": true,
	"--ad-model": true, "--extra-ad-args": true, "--hires": true, "--hires-upscaler": true,
	"--extra-sample-args": true, "--extra-tiling-args": true, "--ref-image-args": true, "--sigmas": true,
	"--hires-sigmas": true, "--cache-mode": true, "--cache-option": true, "--scm-mask": true, "--scm-policy": true,
}

// ForbiddenFlag reports whether one command-line word names a condition 4
// option, alone or as `--option=value`, and returns the option as the word
// spells it. `-l=V` and `--listen-ip=V` are forbidden unless V is Loopback.
func ForbiddenFlag(word string) (string, bool) {
	name, value, inline := strings.Cut(word, "=")
	if name == "-l" || name == "--listen-ip" {
		return name, inline && value != Loopback
	}
	return name, forbiddenFlags[name]
}

// CheckFlag refuses a word that names a forbidden option, wrapping
// ErrForbiddenFlag and naming the option, never its value.
func CheckFlag(word string) error {
	if name, forbidden := ForbiddenFlag(word); forbidden {
		return fmt.Errorf("%w: %s", ErrForbiddenFlag, name)
	}
	return nil
}

// ForbiddenSpellings lists every forbidden spelling in order, for a
// provider's test that its own list admits none of them.
func ForbiddenSpellings() []string {
	names := make([]string, 0, len(forbiddenFlags))
	for name := range forbiddenFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// forbiddenPrefixes and forbiddenNames are condition 5: GGML_BACKEND_PATH
// loads a DLL and GGML_VK_* change device and memory behaviour; SD_VK_DEVICE
// and SD_MMAP_FLAGS change the device and the mapping; VK_ICD_FILENAMES,
// VK_DRIVER_FILES, VK_LAYER_PATH, VK_ADD_LAYER_PATH, VK_INSTANCE_LAYERS and
// VK_LOADER_* make the Vulkan loader load DLLs; HF_* carry a Hugging Face
// token; the proxy variables redirect traffic.
var (
	forbiddenPrefixes = []string{"GGML_", "SD_", "VK_", "HF_"}
	forbiddenNames    = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "FTP_PROXY": true}
)

// ForbiddenEnvironment reports whether sd-server must never receive the
// variable name, in any letter case (condition 5).
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

// Args is one sd-server command line for video, built from typed fields
// (condition 3). The provider sets the port and the three model paths; a
// person may set the device, memory and logging fields through ParseArgs.
type Args struct {
	// ListenPort is the loopback port the provider chose. --listen-ip
	// 127.0.0.1 is always passed.
	ListenPort int
	// DiffusionModel, T5XXL and TAE are the absolute paths of the pinned
	// model files (condition 2).
	DiffusionModel, T5XXL, TAE string

	FlashAttention, DiffusionFlashAttention bool // --fa, --diffusion-fa
	DiffusionConvDirect, VAEConvDirect      bool // --diffusion-conv-direct, --vae-conv-direct
	OffloadToCPU, AutoFit                   bool // --offload-to-cpu, --auto-fit
	VAETiling, TemporalTiling               bool // --vae-tiling, --temporal-tiling
	Verbose, Color                          bool // -v, --color
	// MaxVRAM is --max-vram's one decimal number of GiB; empty omits it.
	MaxVRAM string
	// Backend is --backend: a device (cpu, vulkan0..vulkan7) or module
	// assignments such as diffusion=vulkan0,te=cpu; empty omits it.
	Backend string
	// Threads is -t; zero omits it.
	Threads int
	// VAETileSize is --vae-tile-size as WIDTHxHEIGHT; empty omits it.
	VAETileSize string
}

// Build returns the command line, or the first field outside the reviewed
// list, wrapping ErrArgument.
func (a Args) Build() ([]string, error) {
	if a.ListenPort < 1 || a.ListenPort > 65535 {
		return nil, fmt.Errorf("%w: --listen-port %d", ErrArgument, a.ListenPort)
	}
	for _, p := range []struct{ flag, path string }{{"--diffusion-model", a.DiffusionModel}, {"--t5xxl", a.T5XXL}, {"--tae", a.TAE}} {
		if !filepath.IsAbs(p.path) || strings.ContainsAny(p.path, "\x00\r\n") {
			return nil, fmt.Errorf("%w: %s needs the absolute path of its pinned file", ErrArgument, p.flag)
		}
	}
	out := []string{"--listen-ip", Loopback, "--listen-port", strconv.Itoa(a.ListenPort),
		"--diffusion-model", a.DiffusionModel, "--t5xxl", a.T5XXL, "--tae", a.TAE}
	for _, s := range []struct {
		on   bool
		flag string
	}{{a.FlashAttention, "--fa"}, {a.DiffusionFlashAttention, "--diffusion-fa"}, {a.DiffusionConvDirect, "--diffusion-conv-direct"},
		{a.VAEConvDirect, "--vae-conv-direct"}, {a.OffloadToCPU, "--offload-to-cpu"}, {a.AutoFit, "--auto-fit"},
		{a.VAETiling, "--vae-tiling"}, {a.TemporalTiling, "--temporal-tiling"}, {a.Verbose, "-v"}, {a.Color, "--color"}} {
		if s.on {
			out = append(out, s.flag)
		}
	}
	if a.MaxVRAM != "" {
		if !maxVRAM(a.MaxVRAM) {
			return nil, fmt.Errorf("%w: --max-vram %q", ErrArgument, a.MaxVRAM)
		}
		out = append(out, "--max-vram", a.MaxVRAM)
	}
	if a.Backend != "" {
		if !backend(a.Backend) {
			return nil, fmt.Errorf("%w: --backend %q", ErrArgument, a.Backend)
		}
		out = append(out, "--backend", a.Backend)
	}
	if a.Threads != 0 {
		if a.Threads < -1 || a.Threads > 1024 {
			return nil, fmt.Errorf("%w: -t %d", ErrArgument, a.Threads)
		}
		out = append(out, "-t", strconv.Itoa(a.Threads))
	}
	if a.VAETileSize != "" {
		if !tileSize(a.VAETileSize) {
			return nil, fmt.Errorf("%w: --vae-tile-size %q", ErrArgument, a.VAETileSize)
		}
		out = append(out, "--vae-tile-size", a.VAETileSize)
	}
	// Every word passes the forbidden list too, so a later field can never
	// add one of its names.
	for _, word := range out {
		if err := CheckFlag(word); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// switches and valued are the options a person may add with ParseArgs, by
// every spelling sd-server accepts.
var (
	switches = map[string]func(*Args){
		"--fa": func(a *Args) { a.FlashAttention = true }, "--diffusion-fa": func(a *Args) { a.DiffusionFlashAttention = true },
		"--diffusion-conv-direct": func(a *Args) { a.DiffusionConvDirect = true }, "--vae-conv-direct": func(a *Args) { a.VAEConvDirect = true },
		"--offload-to-cpu": func(a *Args) { a.OffloadToCPU = true }, "--auto-fit": func(a *Args) { a.AutoFit = true },
		"--vae-tiling": func(a *Args) { a.VAETiling = true }, "--temporal-tiling": func(a *Args) { a.TemporalTiling = true },
		"-v": func(a *Args) { a.Verbose = true }, "--verbose": func(a *Args) { a.Verbose = true }, "--color": func(a *Args) { a.Color = true },
	}
	valued = map[string]func(*Args, string) bool{
		"--max-vram": func(a *Args, v string) bool { a.MaxVRAM = v; return maxVRAM(v) },
		"--backend":  func(a *Args, v string) bool { a.Backend = v; return backend(v) },
		"-t": func(a *Args, v string) bool {
			n, err := strconv.Atoi(v)
			a.Threads = n
			return err == nil && n >= -1 && n <= 1024 && n != 0
		},
		"--vae-tile-size": func(a *Args, v string) bool { a.VAETileSize = v; return tileSize(v) },
	}
	// providerOwned are the options the provider sets itself.
	providerOwned = map[string]bool{"-l": true, "--listen-ip": true, "--listen-port": true,
		"--diffusion-model": true, "--t5xxl": true, "--tae": true}
)

// ParseArgs reads the sd-server arguments a person configured into Args.
// A forbidden option is refused by name first, then an option the provider
// sets itself, an option outside the reviewed list, a repeated option and a
// value outside its reviewed form, each wrapping ErrArgument or
// ErrForbiddenFlag and naming the option.
func ParseArgs(words []string) (Args, error) {
	var a Args
	seen := map[string]bool{}
	for i := 0; i < len(words); i++ {
		word := words[i]
		if err := CheckFlag(word); err != nil {
			return Args{}, err
		}
		flag, value, inline := strings.Cut(word, "=")
		if flag == "--threads" {
			flag = "-t"
		}
		if providerOwned[flag] {
			return Args{}, fmt.Errorf("%w: %s is set by the provider", ErrArgument, flag)
		}
		if seen[flag] {
			return Args{}, fmt.Errorf("%w: %s is given more than once", ErrArgument, flag)
		}
		seen[flag] = true
		if set, ok := switches[flag]; ok {
			if inline {
				return Args{}, fmt.Errorf("%w: %s takes no value", ErrArgument, flag)
			}
			set(&a)
			continue
		}
		set, ok := valued[flag]
		if !ok {
			return Args{}, fmt.Errorf("%w: %s is not on the reviewed sd-server list", ErrArgument, flag)
		}
		if !inline {
			if i+1 >= len(words) {
				return Args{}, fmt.Errorf("%w: %s needs a value", ErrArgument, flag)
			}
			i++
			value = words[i]
		}
		if !set(&a, value) {
			return Args{}, fmt.Errorf("%w: %s does not take the value %q", ErrArgument, flag, value)
		}
	}
	return a, nil
}

// maxVRAM accepts one decimal number of GiB in -1024..1024.
func maxVRAM(v string) bool {
	if v == "" || len(v) > 12 || strings.ContainsAny(v, "eE+xX_") {
		return false
	}
	n, err := strconv.ParseFloat(v, 64)
	return err == nil && n >= -1024 && n <= 1024
}

// backendDevice accepts cpu and vulkan0..vulkan7.
func backendDevice(v string) bool {
	if v == "cpu" {
		return true
	}
	d, ok := strings.CutPrefix(v, "vulkan")
	return ok && len(d) == 1 && d[0] >= '0' && d[0] <= '7'
}

// backend accepts one device or comma-separated module=device assignments
// for the modules a Wan pipeline has.
func backend(v string) bool {
	if backendDevice(v) {
		return true
	}
	modules := map[string]bool{"diffusion": true, "te": true, "clip": true, "vae": true}
	seen := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		module, device, ok := strings.Cut(part, "=")
		if !ok || !modules[module] || seen[module] || !backendDevice(device) {
			return false
		}
		seen[module] = true
	}
	return true
}

// tileSize accepts WIDTHxHEIGHT, each 1..4096.
func tileSize(v string) bool {
	w, h, ok := strings.Cut(v, "x")
	width, okW := decimal(w)
	height, okH := decimal(h)
	return ok && okW && okH && width >= 1 && height >= 1 && width <= MaxDimension && height <= MaxDimension
}
