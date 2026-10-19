package providerhost

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/mediationcontent"
	router "github.com/openabstractions/abstraction-router/go"
)

var examplePNG = []byte("\x89PNG\r\n\x1a\nproviderhost-example")

type exampleImageEngine struct{}

func (exampleImageEngine) Unsupported(wire.ImageRequest) string { return "" }
func (exampleImageEngine) Run(context.Context, string, wire.ImageRequest, map[string]inference.ImageInput, func(int64, string)) inference.ImageEngineReply {
	return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []inference.ImageOutput{{Data: examplePNG, MediaType: "image/png"}}}
}

func TestNewImageComposesDesignatedImageAndVerifiedContent(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	host, err := router.NewImageEngine("fixture", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var endpoint string
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\providerhost-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	} else {
		endpoint = filepath.Join(t.TempDir(), "provider.sock")
	}
	var admits atomic.Int64
	provider, err := NewImage(ImageConfig{Endpoint: endpoint, RuntimePrograms: []string{exe}, Host: host, Engine: exampleImageEngine{}, StoreRoot: t.TempDir(), RecordPath: filepath.Join(t.TempDir(), "uploads.json"),
		Admit: func(context.Context, *router.Host, string) (string, bool) { admits.Add(1); return "", true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	transport := listen.FrameClient{Endpoint: endpoint, Timeout: 35 * time.Second, MaxFrame: 1 << 20, Sessions: true}
	client := wire.NewImageClient(transport)
	a, err := client.Start(wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}})
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v, %v", a, err)
	}
	var end *wire.ImageReply
	for cursor := int64(0); end == nil; {
		page, err := client.Observe(a.Operation, cursor, 256, 65536, 20000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v, %v", page, err)
		}
		for _, delta := range page.Deltas {
			if delta.ImageEnd != nil {
				end = delta.ImageEnd
			}
		}
		if page.AtEnd && end == nil {
			t.Fatalf("missing end %+v", page)
		}
		cursor = page.Next
	}
	if end.Outcome != wire.ReplyOutcomeCompleted || len(end.Deliveries) != 1 || admits.Load() != 1 {
		t.Fatalf("end %+v, admits %d", end, admits.Load())
	}
	d := end.Deliveries[0]
	data, err := mediationcontent.Download(context.Background(), transport, d.Digest, d.MediaType, d.Size, imageLimit)
	if err != nil || !bytes.Equal(data, examplePNG) {
		t.Fatalf("verified output %d bytes, %v", len(data), err)
	}
}

// The designated runtime's claim reaches the provider's persistent journal,
// which a reopened journal reads back from disk.
func TestNewImageJournalsDesignatedClaim(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	host, err := router.NewImageEngine("fixture", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(t.TempDir(), "provider.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\providerhost-audit-%d-%d`, os.Getpid(), time.Now().UnixNano())
	}
	recordPath := filepath.Join(t.TempDir(), "uploads.json")
	provider, err := NewImage(ImageConfig{Endpoint: endpoint, RuntimePrograms: []string{exe}, Host: host, Engine: exampleImageEngine{}, StoreRoot: t.TempDir(), RecordPath: recordPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := wire.NewImageClient(listen.FrameClient{Endpoint: endpoint, Timeout: 35 * time.Second, MaxFrame: 1 << 20, Sessions: true})
	a, err := client.Start(wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}, Extensions: map[string]string{inference.ClaimExtension: "app-program"}})
	if err != nil || a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v, %v", a, err)
	}
	for cursor, ended := int64(0), false; !ended; {
		page, err := client.Observe(a.Operation, cursor, 256, 65536, 20000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe %+v, %v", page, err)
		}
		for _, delta := range page.Deltas {
			ended = ended || delta.ImageEnd != nil
		}
		cursor = page.Next
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		reopened, err := inference.OpenJournal(DefaultAuditPath(recordPath), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		page := reopened.Page(0, 16)
		if len(page.Entries) == 1 {
			entry := page.Entries[0]
			if entry.Operation != a.Operation || entry.Outcome != "completed" || entry.Claim != "app-program" || !identity.SameSubjectProgram(entry.Program, exe) {
				t.Fatalf("journal entry %+v", entry)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal page %+v", page)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewImageRefusesUndesignatedRuntimeBeforeAdmit(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	host, err := router.NewImageEngine("fixture", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	var endpoint string
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\providerhost-denied-%d-%d`, os.Getpid(), time.Now().UnixNano())
	} else {
		endpoint = filepath.Join(t.TempDir(), "provider.sock")
	}
	var admits atomic.Int64
	provider, err := NewImage(ImageConfig{Endpoint: endpoint, RuntimePrograms: []string{filepath.Join(t.TempDir(), "different-runtime")}, Host: host, Engine: exampleImageEngine{}, StoreRoot: t.TempDir(), RecordPath: filepath.Join(t.TempDir(), "uploads.json"),
		Admit: func(context.Context, *router.Host, string) (string, bool) { admits.Add(1); return "", true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- provider.Serve(ctx) }()
	defer func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	client := wire.NewImageClient(listen.FrameClient{Endpoint: endpoint, Timeout: 5 * time.Second, MaxFrame: 1 << 20, Sessions: true})
	a, err := client.Start(wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}})
	if err != nil || a.Outcome != wire.StartOutcomeForbidden || admits.Load() != 0 {
		t.Fatalf("undesignated runtime admission %+v, %v, admits %d", a, err, admits.Load())
	}
}
