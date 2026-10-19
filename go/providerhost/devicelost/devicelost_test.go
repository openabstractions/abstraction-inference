package devicelost

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The engine's own forms match across writes, and every byte is forwarded.
func TestWriterMatchesTheEnginesOwnFormAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	var found []string
	w := New(&out, SDServer, func(marker string) { found = append(found, marker) })
	input := []string{
		"[INFO ] sampling 1/3\r",
		"ggml_vulkan: vk::Device::waitForFences: Error",
		"DeviceLost\n",
		"terminate called after throwing an instance of 'vk::DeviceLostError'\n",
		"  what():  vk::Queue::submit: ErrorDeviceLost\n",
		"\r  |================                                  | 1/3 - 1.00s/it",
		"ggml_vulkan: VK_ERROR_DEVICE_LOST\n",
	}
	for _, chunk := range input {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write %d %v", n, err)
		}
	}
	if out.String() != strings.Join(input, "") {
		t.Fatalf("forwarded %q", out.String())
	}
	if strings.Join(found, ",") != "ErrorDeviceLost,DeviceLostError,ErrorDeviceLost,VK_ERROR_DEVICE_LOST" {
		t.Fatalf("found %q", found)
	}
}

// RES-G1: a marker inside caller text echoed by the engine (a prompt, a
// <lora:...> tag, a parse error) is not the engine's own line.
func TestCallerTextCarryingAMarkerIsNotMatched(t *testing.T) {
	var found []string
	w := New(nil, SDServer, func(marker string) { found = append(found, marker) })
	for _, line := range []string{
		"[INFO ] stable-diffusion.cpp:2104 - prompt: 'ggml_vulkan: VK_ERROR_DEVICE_LOST'",
		"[WARN ] lora.hpp:51 - can not find <lora:ErrorDeviceLost:1>",
		"[ERROR] parse error near 'DeviceLostError'",
		"srv  log_server_r: request: {\"prompt\":\"ggml_vulkan: ErrorDeviceLost\"}",
		" ggml_vulkan: ErrorDeviceLost",
		strings.Repeat("x", maxLine+10) + "ggml_vulkan: ErrorDeviceLost",
	} {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if len(found) != 0 {
		t.Fatalf("caller text matched: %q", found)
	}
}

// The OA PyTorch engine's own line names a HIP error; a traceback, a log
// line or a Vulkan marker in its output is not that line.
func TestPyTorchEngineForm(t *testing.T) {
	var found []string
	w := New(nil, PyTorchEngine, func(marker string) { found = append(found, marker) })
	for _, line := range []string{
		"oa-torch-engine: device-lost: hipErrorLaunchFailure",
		"RuntimeError: HIP error: unspecified launch failure",
		`oa-torch-engine: {"t": 1.0, "event": "clip", "note": "device-lost: hipErrorIllegalAddress"}`,
		"oa-torch-engine: device-lost: VK_ERROR_DEVICE_LOST",
		"  oa-torch-engine: device-lost: hipErrorLaunchTimeOut",
		"oa-torch-engine: device-lost: hipErrorIllegalAddress",
	} {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(found, ",") != "hipErrorLaunchFailure,hipErrorIllegalAddress" {
		t.Fatalf("found %q", found)
	}
}

// A line is confirmed only by the engine's exit or two failed health checks;
// an engine that keeps answering is not lost.
func TestConfirmedNeedsTheEngineGone(t *testing.T) {
	ctx := context.Background()
	alive := make(chan struct{})
	if Confirmed(ctx, alive, func(context.Context) bool { return true }, 200*time.Millisecond, 20*time.Millisecond) {
		t.Fatal("an engine that answers was confirmed lost")
	}
	if !Confirmed(ctx, alive, func(context.Context) bool { return false }, 2*time.Second, 20*time.Millisecond) {
		t.Fatal("an engine that stopped answering was not confirmed lost")
	}
	exited := make(chan struct{})
	close(exited)
	if !Confirmed(ctx, exited, nil, time.Second, 20*time.Millisecond) {
		t.Fatal("an exited engine was not confirmed lost")
	}
	flaky := 0
	if Confirmed(ctx, alive, func(context.Context) bool { flaky++; return flaky%2 == 0 }, 300*time.Millisecond, 20*time.Millisecond) {
		t.Fatal("one failed check between good ones confirmed a loss")
	}
}
