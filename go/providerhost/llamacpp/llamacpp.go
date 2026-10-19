// Package llamacpp holds the conditions of the llama.cpp b10825 security
// review (research/security-reviews/llama.cpp-b10825-win-vulkan-x64.md) that
// every provider starting llama-server enforces the same way: the options it
// never passes (condition 2), the environment names the engine never receives
// (condition 3) and the chat media it forwards (condition 6). Inference
// CONTRACT.md INF-LC1..INF-LC3 states them. A provider may refuse more on top,
// such as an allowlist of the options a person may add.
package llamacpp

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrForbiddenFlag reports a llama-server option the b10825 review forbids.
var ErrForbiddenFlag = errors.New("llama.cpp: option forbidden by the b10825 review")

// forbiddenFlags are the options of review condition 2 in every spelling
// b10825's common/arg.cpp accepts, with the two options the review's network
// section names as widening reach: --video-ffmpeg-dir (no reviewed FFmpeg
// exists, condition 6) and --cors-origins. Each has a LLAMA_ARG_* environment
// form, which ForbiddenEnvironment refuses.
var forbiddenFlags = map[string]bool{
	"--sleep-idle-seconds": true, "--host": true, "--rpc": true, "--tools": true, "--tools-runtime": true,
	"-ag": true, "--agent": true, "--mcp-servers-config": true, "--mcp-servers-json": true,
	"--webui-mcp-proxy": true, "--ui-mcp-proxy": true,
	"-hf": true, "-hfr": true, "--hf-repo": true, "-hff": true, "--hf-file": true, "-hft": true, "--hf-token": true,
	"-mu": true, "--model-url": true, "-mmu": true, "--mmproj-url": true, "-dr": true, "--docker-repo": true,
	"--spec-draft-hf": true, "-hfd": true, "-hfrd": true, "--hf-repo-draft": true,
	"--models-dir": true, "--models-preset": true, "--media-path": true, "--path": true,
	"--ui-config-file": true, "--webui-config-file": true, "--slot-save-path": true, "--log-file": true,
	"--log-prompts-dir": true, "--video-ffmpeg-dir": true, "--cors-origins": true,
}

// ForbiddenFlag reports whether one command-line word names a forbidden
// option, alone or as `--option=value`, and returns the option as the word
// spells it.
func ForbiddenFlag(word string) (string, bool) {
	name, _, _ := strings.Cut(word, "=")
	return name, forbiddenFlags[name]
}

// CheckFlag refuses a word that names a forbidden option, wrapping
// ErrForbiddenFlag and naming the option.
func CheckFlag(word string) error {
	if name, forbidden := ForbiddenFlag(word); forbidden {
		return fmt.Errorf("%w: %s", ErrForbiddenFlag, name)
	}
	return nil
}

// ForbiddenSpellings lists every forbidden spelling in order, for a provider's
// test that its own allowlist admits none of them.
func ForbiddenSpellings() []string {
	names := make([]string, 0, len(forbiddenFlags))
	for name := range forbiddenFlags {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ownKey is the variable llama-server reads its API key from. A provider
// that makes a per-launch key passes it under this name; ForbiddenEnvironment
// leaves the name to the provider's allowlist.
const ownKey = "LLAMA_API_KEY"

// forbiddenPrefixes and forbiddenNames are review condition 3:
// GGML_BACKEND_PATH loads the DLL it names as a backend, MTMD_* reach the
// multimodal helper, LLAMA_* carry the LLAMA_ARG_* options and LLAMA_CACHE,
// HF_* the Hugging Face token and endpoint, and the proxy variables redirect
// the engine's fetches.
var (
	forbiddenPrefixes = []string{"GGML_", "MTMD_", "LLAMA_", "HF_"}
	forbiddenNames    = map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true}
)

// ForbiddenEnvironment reports whether llama-server must never receive the
// variable name, in any letter case. LLAMA_API_KEY is the one LLAMA_* name
// left to the provider, which sets it to its own per-launch key or omits it;
// a provider's allowlist decides whether it may appear at all.
func ForbiddenEnvironment(name string) bool {
	upper := strings.ToUpper(name)
	if forbiddenNames[upper] {
		return true
	}
	if upper == ownKey {
		return false
	}
	for _, prefix := range forbiddenPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}
