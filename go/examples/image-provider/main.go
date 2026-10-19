// A minimal independent image provider. Replace solidPixel.Run with an engine
// adapter; the native endpoint, content store, caller admission and operation
// lifecycle stay in providerhost.NewImage.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/providerhost"
	router "github.com/openabstractions/abstraction-router/go"
)

type solidPixel struct{}

func (solidPixel) Unsupported(req wire.ImageRequest) string {
	if req.Mode != wire.ImageModeGenerate {
		return "mode:edit"
	}
	if req.Count != 1 || req.Size != "auto" {
		return "image:request"
	}
	return ""
}

func (solidPixel) Run(ctx context.Context, _ string, _ wire.ImageRequest, _ map[string]inference.ImageInput, _ func(int64, string)) inference.ImageEngineReply {
	if ctx.Err() != nil {
		return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "caller"}
	}
	pixel := image.NewRGBA(image.Rect(0, 0, 1, 1))
	pixel.Set(0, 0, color.RGBA{R: 30, G: 120, B: 220, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, pixel); err != nil {
		return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "engine:encode"}
	}
	return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []inference.ImageOutput{{Data: buf.Bytes(), MediaType: "image/png"}}}
}

func main() {
	endpoint := flag.String("endpoint", "", "native provider endpoint")
	runtimeProgram := flag.String("runtime", "", "absolute path of the OA runtime executable")
	store := flag.String("store", "", "absolute private provider store directory")
	flag.Parse()
	if *endpoint == "" || *runtimeProgram == "" || *store == "" {
		fmt.Fprintln(os.Stderr, "endpoint, runtime and store are required")
		os.Exit(2)
	}
	host, err := router.NewImageEngine("sample-image", []string{"sample/solid-pixel"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	provider, err := providerhost.NewImage(providerhost.ImageConfig{Endpoint: *endpoint, RuntimePrograms: []string{*runtimeProgram}, Host: host, Engine: solidPixel{}, StoreRoot: *store, RecordPath: filepath.Join(*store, "uploads.json")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
