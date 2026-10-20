package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestChat3LeafServedSetsAndCallerBoundaryOverIPC(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program := identity.CanonicalProgramPath(filepath.Clean(self))
	base := []string{inference.Contract, "abstraction.inference/chat@2", inference.EmbedContract, inference.PreloadContract,
		inference.TranscriptionContract, inference.SpeechContract, inference.ImageContract, "abstraction.inference/image@2", inference.LiveContract}
	for _, kind := range []string{"listen", "placement", "application-local", "mediated-local", "paired-remote", "leaf", "designated", "not-designated", "unbound"} {
		t.Run(kind, func(t *testing.T) {
			var effects atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { effects.Add(1) }))
			defer up.Close()
			cfg := inference.Config{Router: router.New(router.OpenAICompatible("direct", up.URL, "/v1", nil)),
				Decide: func(context.Context, inference.Subject, string, string) (string, error) {
					effects.Add(1)
					return "permitted", nil
				},
				Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
					effects.Add(1)
					return nil, "applied"
				}}
			if kind != "unbound" {
				cfg.ChatLogicalOwner = "fixture-owner"
			}
			p, err := inference.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := endpointFor(t)
			var h *Host
			switch kind {
			case "listen":
				h, err = Listen(endpoint, p)
			case "placement":
				h, err = ListenForPlacement(endpoint, p, inference.ExecutionLocal)
			case "application-local":
				h, err = ListenForPlacement(endpoint, p, inference.ExecutionLocal)
				if err == nil {
					h.ApplicationChatV3 = true
				}
			case "mediated-local":
				h, err = ListenMediated(endpoint, p, inference.ExecutionLocal, inference.ExecutionBinding{Host: "fixture", BindingID: "generation"})
			case "paired-remote":
				h, err = ListenMediated(endpoint, p, inference.ExecutionRemote, inference.ExecutionBinding{Host: "fixture", BindingID: "generation"})
			case "leaf":
				h, err = ListenLeaf(endpoint, p, inference.ExecutionLocal)
			default:
				designated := program
				if kind == "not-designated" {
					designated = filepath.Join(t.TempDir(), "other.exe")
				}
				h, err = ListenDesignatedLeaf(endpoint, p, inference.ExecutionLocal, designated)
			}
			if err != nil {
				p.Close()
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- h.Serve(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-done
				if err := p.Close(); err != nil {
					t.Error(err)
				}
			})
			transport := listen.FrameClient{Endpoint: endpoint}.WithDefaults(5*time.Second, MaxFrameBytes)
			reply, err := transport.ExchangeFrame([]byte(`{"version":1,"service":"abstraction.facade/endpoint@2","method":"Describe","arguments":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Payload struct{ Value json.RawMessage }
			}
			if err := json.Unmarshal(reply, &envelope); err != nil {
				t.Fatal(err)
			}
			var description wire.EndpointDescription
			if err := wire.FromJSON(envelope.Payload.Value, &description); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, service := range description.Services {
				got = append(got, service.Service)
			}
			want := slices.Clone(base)
			leaf := kind == "leaf" || kind == "designated" || kind == "not-designated" || kind == "unbound"
			chat3 := leaf || kind == "application-local"
			if chat3 {
				want = append(want, inference.ChatV3Contract)
			}
			if kind == "paired-remote" {
				want = []string{inference.Contract, inference.EmbedContract}
			}
			slices.Sort(got)
			slices.Sort(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("served=%v want=%v", got, want)
			}
			client := wire.NewChatV3Client(transport)
			window, err := client.GetHistoryWindow()
			if !chat3 {
				var refusal *wire.ServiceError
				if !errors.As(err, &refusal) || refusal.Code != wire.ServiceErrorCodeUnknownService {
					t.Fatalf("application/paired endpoint offered chat3: %+v %v", window, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "not-designated" {
				if window.Outcome != wire.ChatHistoryOutcomeForbidden || window.Window != nil {
					t.Fatalf("non-designated window=%+v", window)
				}
				return
			}
			if kind == "unbound" {
				if window.Outcome != wire.ChatHistoryOutcomeUnavailable || window.Window != nil {
					t.Fatalf("unbound window=%+v", window)
				}
				admission, err := client.Start(wire.RequestV3{Identity: acceptance.RequestIdentity{Key: "unbound", HistoryEpoch: "unknown"}, Target: wire.ChatTarget{Model: "model"}, Content: wire.ChatContent{Messages: []wire.ChatMessage{{Role: wire.RoleUser, Parts: []wire.ChatInputPart{{Kind: wire.PartKindText, Text: "title"}}}}}})
				if err != nil || admission.Outcome != wire.ChatStartOutcomeUnavailable || admission.Reason != "history:owner_unbound" || effects.Load() != 0 || len(cfg.Router.Asked()) != 0 {
					t.Fatalf("unbound=%+v %v effects=%d routed=%v", admission, err, effects.Load(), cfg.Router.Asked())
				}
				return
			}
			if window.Outcome != wire.ChatHistoryOutcomeAvailable || window.Window == nil || window.Window.LogicalOwner != cfg.ChatLogicalOwner || window.Window.HistoryEpoch == "" || effects.Load() != 0 {
				t.Fatalf("window=%+v effects=%d", window, effects.Load())
			}
		})
	}
}

type chat3AcceptCounter struct{ calls int }

func (l *chat3AcceptCounter) Accept() (listen.Conn, error) {
	l.calls++
	return nil, errors.New("unexpected accept")
}
func (*chat3AcceptCounter) Close() error { return nil }

func TestApplicationChat3RejectsInvalidHostBeforeAccept(t *testing.T) {
	for name, configure := range map[string]func(*Host){
		"leaf":             func(h *Host) { h.leaf = true },
		"designated":       func(h *Host) { h.designated = []string{"runtime"} },
		"bound-host":       func(h *Host) { h.binding.Host = "declared" },
		"bound-generation": func(h *Host) { h.binding.BindingID = "generation" },
		"any-placement":    func(h *Host) { h.placement = inference.ExecutionAny },
		"remote-placement": func(h *Host) { h.placement = inference.ExecutionRemote },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listener := &chat3AcceptCounter{}
			h := &Host{ctx: ctx, cancel: cancel, listener: listener, placement: inference.ExecutionLocal, ApplicationChatV3: true}
			configure(h)
			if err := h.Serve(ctx); err == nil || listener.calls != 0 {
				t.Fatal("invalid application endpoint reached Accept", err, listener.calls)
			}
		})
	}
}
