package sdcpp

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func pinnedArgs() Args {
	return Args{ListenPort: 41000, DiffusionModel: absPath("FastWan2.2-TI2V-5B-q8_0.gguf"),
		T5XXL: absPath("umt5-xxl-encoder-Q8_0.gguf"), TAE: absPath("taew2_2.safetensors")}
}

func absPath(name string) string {
	if strings.HasPrefix(testRoot, "/") {
		return testRoot + "/" + name
	}
	return testRoot + `\` + name
}

// Condition 3: the command line always names loopback, the chosen port and
// the three pinned paths, and nothing that is not a typed field.
func TestArgsBuildNamesLoopbackPortAndPinnedPaths(t *testing.T) {
	a := pinnedArgs()
	a.DiffusionFlashAttention, a.OffloadToCPU, a.Backend, a.Threads, a.MaxVRAM, a.VAETileSize = true, true, "diffusion=vulkan0,te=cpu", 8, "-2", "256x256"
	args, err := a.Build()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--listen-ip", "127.0.0.1", "--listen-port", "41000", "--diffusion-model", a.DiffusionModel,
		"--t5xxl", a.T5XXL, "--tae", a.TAE, "--diffusion-fa", "--offload-to-cpu", "--max-vram", "-2",
		"--backend", "diffusion=vulkan0,te=cpu", "-t", "8", "--vae-tile-size", "256x256"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q", args)
	}
	for _, bad := range []func(*Args){
		func(a *Args) { a.ListenPort = 0 },
		func(a *Args) { a.DiffusionModel = "relative.gguf" },
		func(a *Args) { a.TAE = "" },
		func(a *Args) { a.Backend = "cuda0" },
		func(a *Args) { a.Backend = "diffusion=vulkan0,diffusion=cpu" },
		func(a *Args) { a.MaxVRAM = "1e9" },
		func(a *Args) { a.Threads = 5000 },
		func(a *Args) { a.VAETileSize = "0x64" },
	} {
		a := pinnedArgs()
		bad(&a)
		if _, err := a.Build(); !errors.Is(err, ErrArgument) {
			t.Fatalf("%+v built: %v", a, err)
		}
	}
}

// Condition 4: every forbidden option is refused by name, alone or with an
// inline value, by CheckFlag and by ParseArgs before anything else.
func TestForbiddenOptionsAreRefusedByName(t *testing.T) {
	for _, name := range []string{"-M", "--mode", "--rpc-servers", "--serve-html-path", "--lora-model-dir", "--vae", "-m",
		"--model", "--mmap", "--params-backend", "-i", "--init-img", "-r", "--ref-image", "--hires", "--sigmas",
		"--cache-mode", "--scm-policy", "--high-noise-diffusion-model", "--prompt-file", "--ad-model", "--extra-sample-args"} {
		for _, word := range []string{name, name + "=x"} {
			if err := CheckFlag(word); !errors.Is(err, ErrForbiddenFlag) || !strings.HasSuffix(err.Error(), name) {
				t.Fatalf("CheckFlag(%q) = %v", word, err)
			}
		}
		if _, err := ParseArgs([]string{"--fa", name, "value"}); !errors.Is(err, ErrForbiddenFlag) {
			t.Fatalf("ParseArgs admitted %s: %v", name, err)
		}
	}
	if len(ForbiddenSpellings()) != len(forbiddenFlags) {
		t.Fatal("spelling list incomplete")
	}
	for _, word := range []string{"-l=0.0.0.0", "--listen-ip=::", "--listen-ip=192.168.1.2"} {
		if err := CheckFlag(word); !errors.Is(err, ErrForbiddenFlag) {
			t.Fatalf("%s: %v", word, err)
		}
	}
	if err := CheckFlag("--listen-ip=127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	// The reviewed list admits no forbidden spelling.
	for _, name := range ForbiddenSpellings() {
		if switches[name] != nil || valued[name] != nil {
			t.Fatalf("%s is both forbidden and admitted", name)
		}
	}
}

// Condition 3: a person may add the device, memory and logging options;
// the options the provider sets itself and anything unknown are refused.
func TestParseArgsAdmitsOnlyTheReviewedPersonOptions(t *testing.T) {
	a, err := ParseArgs([]string{"--fa", "--offload-to-cpu", "--backend=vulkan0", "--threads", "6", "--max-vram", "12", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.FlashAttention || !a.OffloadToCPU || a.Backend != "vulkan0" || a.Threads != 6 || a.MaxVRAM != "12" || !a.Verbose {
		t.Fatalf("parsed %+v", a)
	}
	for _, words := range [][]string{
		{"--listen-ip", "127.0.0.1"}, {"-l", "0.0.0.0"}, {"--listen-port", "80"}, {"--diffusion-model", "x"},
		{"--t5xxl", "x"}, {"--tae", "x"}, {"--steps", "20"}, {"--cfg-scale", "7"}, {"--fa", "--fa"},
		{"--fa=1"}, {"--backend"}, {"--backend", "cuda0"}, {"-t", "0"}, {"positional"},
	} {
		if _, err := ParseArgs(words); err == nil {
			t.Fatalf("%q admitted", words)
		}
	}
}

// Condition 5: GGML_, SD_, VK_, HF_ and the proxies are refused in any case.
func TestForbiddenEnvironment(t *testing.T) {
	for _, name := range []string{"GGML_BACKEND_PATH", "ggml_vk_visible_devices", "SD_VK_DEVICE", "sd_mmap_flags",
		"VK_ICD_FILENAMES", "vk_loader_layers_enable", "VK_INSTANCE_LAYERS", "HF_TOKEN", "http_proxy", "HTTPS_PROXY",
		"ALL_PROXY", "no_proxy"} {
		if !ForbiddenEnvironment(name) {
			t.Fatalf("%s admitted", name)
		}
	}
	for _, name := range []string{"PATH", "SystemRoot", "TEMP", "LOCALAPPDATA", "SDK"} {
		if ForbiddenEnvironment(name) {
			t.Fatalf("%s refused", name)
		}
	}
}
