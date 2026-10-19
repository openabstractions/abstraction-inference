package inference

import (
	"context"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type testImageEngine struct {
	unsupported string
	runs        int
	started     chan struct{}
	waitCancel  bool
	result      ImageEngineReply
}

func (e *testImageEngine) Unsupported(wire.ImageRequest) string { return e.unsupported }

func (e *testImageEngine) Run(ctx context.Context, _ string, _ wire.ImageRequest, _ map[string]ImageInput, progress func(int64, string)) ImageEngineReply {
	e.runs++
	if e.started != nil {
		close(e.started)
	}
	if e.waitCancel {
		<-ctx.Done()
		return ImageEngineReply{Outcome: wire.ReplyOutcomeCancelled, Reason: "caller"}
	}
	progress(25, "running")
	return e.result
}

func imageEngineFixture(t *testing.T, engine ImageEngine, decision string) (*Provider, *imageStore) {
	t.Helper()
	host, err := router.NewImageEngine("fixture-engine", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	routes := router.New(host)
	routes.Survey()
	store := &imageStore{}
	p, err := New(Config{Router: routes, ImageEngine: engine, ImageEngineHost: host,
		Decide: func(context.Context, Subject, string, string) (string, error) { return decision, nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		},
		PrepareContentWrite: store.prepare})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, store
}

func TestLocalImageEngineRequiresSelectedHostBinding(t *testing.T) {
	engine := &testImageEngine{result: ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: pngOne, MediaType: "image/png"}}}}
	p, _ := imageEngineFixture(t, engine, "permitted")
	other, err := router.NewImageEngine("other-engine", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	p.cfg.ImageEngineHost = other
	a := p.StartImage(context.Background(), caller, imageEngineRequest())
	if a.Outcome != wire.StartOutcomeUnsupportedFeature || engine.runs != 0 {
		t.Fatalf("mismatched host admission %+v, runs %d", a, engine.runs)
	}
}

func TestLocalImageEngineInvalidReplies(t *testing.T) {
	cases := []struct {
		name   string
		result ImageEngineReply
	}{
		{"negative usage", ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Usage: wire.Usage{Input: -1}}},
		{"oversized usage", ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Usage: wire.Usage{Input: 1 << 54}}},
		{"negative cost", ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Cost: &wire.Cost{Micros: -1}}},
		{"invalid reason", ImageEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: string([]byte{0xff})}},
		{"long reason", ImageEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: strings.Repeat("x", 4097)}},
		{"oversized image", ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: pngOne, MediaType: "image/png"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &testImageEngine{result: tc.result}
			p, store := imageEngineFixture(t, engine, "permitted")
			if tc.name == "oversized image" {
				p.cfg.MaxGeneratedImageBytes = int64(len(pngOne) - 1)
			}
			a := p.StartImage(context.Background(), caller, imageEngineRequest())
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			_, end := drainImage(t, p, caller, a.Operation)
			if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "engine:invalid_reply" || len(store.data) != 1 || store.data[0] != nil {
				t.Fatalf("invalid reply end %+v, saved %d", end, len(store.data))
			}
		})
	}
}

func imageEngineRequest() wire.ImageRequest {
	return wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
}

func TestLocalImageEngineUsesInferencePublication(t *testing.T) {
	engine := &testImageEngine{result: ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: pngOne, MediaType: "image/png"}}}}
	p, store := imageEngineFixture(t, engine, "permitted")
	a := p.StartImage(context.Background(), caller, imageEngineRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	_, end := drainImage(t, p, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || len(end.Deliveries) != 1 || end.Deliveries[0].Location != "local" || engine.runs != 1 || len(store.data) != 1 {
		t.Fatalf("end %+v, runs %d, saved %d", end, engine.runs, len(store.data))
	}
}

func TestLocalImageEnginePreflightAndOutputBounds(t *testing.T) {
	engine := &testImageEngine{unsupported: "mode:edit"}
	p, _ := imageEngineFixture(t, engine, "permitted")
	if a := p.StartImage(context.Background(), caller, imageEngineRequest()); a.Outcome != wire.StartOutcomeUnsupportedFeature || engine.runs != 0 {
		t.Fatalf("unsupported admission %+v, runs %d", a, engine.runs)
	}
	engine.unsupported = ""
	denied, _ := imageEngineFixture(t, engine, "denied")
	if a := denied.StartImage(context.Background(), caller, imageEngineRequest()); a.Outcome != wire.StartOutcomeNotPermitted || engine.runs != 0 {
		t.Fatalf("denied admission %+v, runs %d", a, engine.runs)
	}
	engine.result = ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []ImageOutput{{Data: []byte("invalid"), MediaType: "image/png"}}}
	a := p.StartImage(context.Background(), caller, imageEngineRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("invalid output admission %+v", a)
	}
	_, end := drainImage(t, p, caller, a.Operation)
	if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "upstream:image" {
		t.Fatalf("invalid output end %+v", end)
	}
}

func TestLocalImageEngineCancellationStopsCallback(t *testing.T) {
	engine := &testImageEngine{started: make(chan struct{}), waitCancel: true}
	p, _ := imageEngineFixture(t, engine, "permitted")
	a := p.StartImage(context.Background(), caller, imageEngineRequest())
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	<-engine.started
	result := p.CancelImage(caller, a.Operation)
	if result.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel %+v", result)
	}
}
