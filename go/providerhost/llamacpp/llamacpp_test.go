package llamacpp

import (
	"errors"
	"slices"
	"testing"
)

// Review condition 2: every listed option is refused alone and in its
// `--option=value` form, by the spelling the word uses.
func TestForbiddenFlagsAreRefusedInEveryForm(t *testing.T) {
	for _, name := range []string{"--host", "--rpc", "--tools", "--agent", "-ag", "-hf", "--hf-repo", "--model-url",
		"--media-path", "--path", "--sleep-idle-seconds", "--video-ffmpeg-dir", "--cors-origins", "--log-prompts-dir"} {
		for _, word := range []string{name, name + "=x"} {
			got, forbidden := ForbiddenFlag(word)
			if !forbidden || got != name {
				t.Fatalf("%q: forbidden %v as %q", word, forbidden, got)
			}
			if err := CheckFlag(word); !errors.Is(err, ErrForbiddenFlag) {
				t.Fatalf("%q: %v", word, err)
			}
		}
	}
	for _, word := range []string{"--ctx-size", "--ctx-size=4096", "-c", "--temp", "--hostname", "host"} {
		if _, forbidden := ForbiddenFlag(word); forbidden {
			t.Fatalf("%q refused", word)
		}
		if err := CheckFlag(word); err != nil {
			t.Fatalf("%q: %v", word, err)
		}
	}
	names := ForbiddenSpellings()
	if !slices.IsSorted(names) || len(names) != len(forbiddenFlags) || !slices.Contains(names, "--hf-token") {
		t.Fatalf("ForbiddenSpellings is not the sorted list: %q", names)
	}
}

// Review condition 3: the GGML_*, MTMD_*, LLAMA_* and HF_* names and the
// proxy variables are refused in any letter case; LLAMA_API_KEY is the
// provider's own and is left to its allowlist.
func TestForbiddenEnvironmentNames(t *testing.T) {
	for _, name := range []string{"GGML_BACKEND_PATH", "ggml_backend_path", "GGML_VK_VISIBLE_DEVICES", "MTMD_BACKEND_DEVICE",
		"LLAMA_ARG_HOST", "LLAMA_ARG_MEDIA_PATH", "LLAMA_CACHE", "llama_arg_rpc", "HF_TOKEN", "HF_ENDPOINT",
		"HTTP_PROXY", "https_proxy", "ALL_PROXY", "NO_PROXY"} {
		if !ForbiddenEnvironment(name) {
			t.Fatalf("%s admitted", name)
		}
	}
	for _, name := range []string{"LLAMA_API_KEY", "llama_api_key", "PATH", "SystemRoot", "TEMP", "LOCALAPPDATA", "LEMONADE_API_KEY"} {
		if ForbiddenEnvironment(name) {
			t.Fatalf("%s refused", name)
		}
	}
}
