package providerhost

import (
	"context"
	"os"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type inertEngine struct{}

func (inertEngine) Unsupported(wire.ImageRequest) string { return "" }
func (inertEngine) Run(context.Context, string, wire.ImageRequest, map[string]inference.ImageInput, func(int64, string)) inference.ImageEngineReply {
	return inference.ImageEngineReply{}
}

func TestNewRequiresRuntimeAndBoundImageEngine(t *testing.T) {
	host, err := router.NewImageEngine("fixture", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Endpoint: "unused", RuntimePrograms: []string{"runtime"}, Inference: inference.Config{
		Router: router.New(host),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "denied", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		ImageEngine: inertEngine{},
	}}
	if _, err := New(base); err == nil {
		t.Fatal("unbound engine accepted")
	}
	base.Inference.ImageEngineHost = host
	base.RuntimePrograms = nil
	if _, err := New(base); err == nil {
		t.Fatal("missing runtime designation accepted")
	}
	base.RuntimePrograms = []string{os.Args[0]}
	base.Endpoint = ""
	if _, err := New(base); err == nil {
		t.Fatal("missing endpoint accepted")
	}
}
