// Package devicelost reads an engine's own output for the line its GPU
// backend prints when the device is lost, in the exact form that engine
// prints it, and confirms the loss against the engine itself before a
// provider counts it as a driver reset (resource CONTRACT.md RES-G1). It
// forwards every byte unchanged and parses nothing else.
//
// A line alone is never a reset. Engine output also carries caller text: a
// prompt, a LoRA tag or a parse error echoed back. A marker inside such text
// either fails the anchored form or, if a prompt's own line break puts it at
// the start of a line, fails Confirmed while the engine keeps answering.
package devicelost

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LineForm is how one engine prints its device-lost line: a line that starts
// with one of Prefixes and carries one of Markers after it. Lines are split
// on carriage return and line feed, as progress bars redraw with a carriage
// return.
type LineForm struct {
	Engine   string
	Prefixes []string
	Markers  []string
}

// ggmlVulkan is ggml's Vulkan backend as llama-server and sd-server link it:
// its own log lines start "ggml_vulkan: ", and a vulkan-hpp exception that
// reaches the C++ runtime prints libstdc++'s terminate line and its what()
// line. VK_ERROR_DEVICE_LOST is the Vulkan result; ErrorDeviceLost and
// DeviceLostError are vulkan-hpp's names for it.
var ggmlVulkan = LineForm{
	Prefixes: []string{
		"ggml_vulkan: ",
		"terminate called after throwing an instance of 'vk::",
		"  what():  vk::",
	},
	Markers: []string{"VK_ERROR_DEVICE_LOST", "ErrorDeviceLost", "DeviceLostError"},
}

// LlamaServer and SDServer are the two engines modelhost runs; both report a
// lost device through ggml's Vulkan backend.
var (
	LlamaServer = LineForm{Engine: "llama-server", Prefixes: ggmlVulkan.Prefixes, Markers: ggmlVulkan.Markers}
	SDServer    = LineForm{Engine: "sd-server", Prefixes: ggmlVulkan.Prefixes, Markers: ggmlVulkan.Markers}
)

// PyTorchEngine is modelhost's OA PyTorch engine (abstraction-provider-
// modelhost oa_torch_engine.py). PyTorch on ROCm reports a lost device as a
// HIP error on a Python exception; the engine catches it, prints exactly
// "oa-torch-engine: device-lost: <HIP error name>" on its own log and exits.
// The names are HIP's (hip_runtime_api.h): a kernel that faulted, an illegal
// address, a launch the driver timed out, a destroyed context, an illegal
// state.
var PyTorchEngine = LineForm{
	Engine:   "oa-torch-engine",
	Prefixes: []string{"oa-torch-engine: device-lost: "},
	Markers: []string{"hipErrorLaunchFailure", "hipErrorIllegalAddress", "hipErrorLaunchTimeOut",
		"hipErrorContextIsDestroyed", "hipErrorIllegalState"},
}

// progressBar is the bar ggml-based engines redraw on one line while they
// sample or decode ("  |=====     | 1/3 - 1.00s/it"). It ends with no line
// break, so the backend's next line of its own follows it on the same line;
// the bar is the engine's own text and is set aside before the prefix is
// matched.
var progressBar = regexp.MustCompile(`^\s*\|[=>\- ]*\|\s*\d+/\d+\s*-\s*[0-9.]+(s/it|it/s)`)

// Match is the marker line carries in this form, or "".
func (f LineForm) Match(line string) string {
	for {
		bar := progressBar.FindStringIndex(line)
		if bar == nil {
			break
		}
		line = line[bar[1]:]
	}
	for _, prefix := range f.Prefixes {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		for _, marker := range f.Markers {
			if strings.Contains(rest, marker) {
				return marker
			}
		}
	}
	return ""
}

// maxLine bounds how much of one unfinished line is kept for matching.
const maxLine = 64 << 10

// Writer forwards to next and calls found once per line that matches its
// form, with the marker. next may be nil.
type Writer struct {
	next  io.Writer
	form  LineForm
	found func(marker string)
	mu    sync.Mutex
	line  []byte
}

// New wraps next. found runs on the writing goroutine and must not block; a
// provider confirms the loss with Confirmed on a goroutine of its own.
func New(next io.Writer, form LineForm, found func(marker string)) *Writer {
	return &Writer{next: next, form: form, found: found}
}

func (w *Writer) Write(p []byte) (int, error) {
	w.scan(p)
	if w.next == nil {
		return len(p), nil
	}
	return w.next.Write(p)
}

func (w *Writer) scan(p []byte) {
	w.mu.Lock()
	var hits []string
	for len(p) > 0 {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			w.line = append(w.line, p...)
			if len(w.line) > maxLine {
				// An overlong line is no engine's own; drop it whole so a
				// cut-off tail never reads as a fresh line start.
				w.line = append(w.line[:0], 0)
			}
			break
		}
		w.line = append(w.line, p[:i]...)
		if marker := w.form.Match(string(w.line)); marker != "" {
			hits = append(hits, marker)
		}
		w.line = w.line[:0]
		p = p[i+1:]
	}
	w.mu.Unlock()
	if w.found != nil {
		for _, marker := range hits {
			w.found(marker)
		}
	}
}

// Confirmed reports whether the engine that printed a device-lost line is
// gone: its process exited, or its own readiness or health check failed
// twice in a row, within the bound. An engine that keeps answering was not
// lost, whatever its output said. poll zero reads one second.
func Confirmed(ctx context.Context, exited <-chan struct{}, healthy func(context.Context) bool, within, poll time.Duration) bool {
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	failed := 0
	for {
		select {
		case <-exited:
			return true
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
		if healthy == nil {
			continue
		}
		check, cancel := context.WithTimeout(ctx, poll)
		ok := healthy(check)
		cancel()
		select {
		case <-exited:
			return true
		default:
		}
		if ok {
			failed = 0
			continue
		}
		failed++
		if failed >= 2 {
			return true
		}
	}
}
