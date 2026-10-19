package inference_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	casapi "github.com/openabstractions/abstraction-cas/go/api"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	service "github.com/openabstractions/abstraction-inference/go/service"
	router "github.com/openabstractions/abstraction-router/go"
	storage "github.com/openabstractions/abstraction-storage/go"
	storageService "github.com/openabstractions/abstraction-storage/go/service"
)

var processImagePNG = []byte("\x89PNG\r\n\x1a\nindependent-process-image")

type processImageEngine struct{}

func (processImageEngine) Unsupported(wire.ImageRequest) string { return "" }
func (processImageEngine) Run(context.Context, string, wire.ImageRequest, map[string]inference.ImageInput, func(int64, string)) inference.ImageEngineReply {
	return inference.ImageEngineReply{Outcome: wire.ReplyOutcomeCompleted, Images: []inference.ImageOutput{{Data: processImagePNG, MediaType: "image/png"}}}
}

// TestNativeImageProviderChild is the test binary running as the independent
// native provider. It owns its own content store and one composed IPC endpoint.
func TestNativeImageProviderChild(t *testing.T) {
	if os.Getenv("OA_IMAGE_PROCESS_CHILD") != "1" {
		return
	}
	endpoint, root, records := os.Getenv("OA_IMAGE_PROCESS_ENDPOINT"), os.Getenv("OA_IMAGE_PROCESS_ROOT"), os.Getenv("OA_IMAGE_PROCESS_RECORDS")
	store, err := storage.NewContentStore("child", root)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context, *identity.Peer, string) error { return nil }
	content, err := storageService.NewComponent(store, allow)
	if err != nil {
		t.Fatal(err)
	}
	if err := content.EnableWriter(allow, 20<<20, casapi.BoundedFileStore{MaxBytes: storageService.MaxRecordFileBytes}, records); err != nil {
		t.Fatal(err)
	}
	engineHost, err := router.NewImageEngine("child-engine", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := inference.New(inference.Config{Router: router.New(engineHost), ImageEngine: processImageEngine{}, ImageEngineHost: engineHost,
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			countFile := os.Getenv("OA_IMAGE_PROCESS_COUNT")
			f, err := os.OpenFile(countFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return "", err
			}
			_, err = f.WriteString("start\n")
			if err == nil {
				err = f.Sync()
			}
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return "", err
			}
			if os.Getenv("OA_IMAGE_PROCESS_MODE") == "crash-on-start" {
				os.Exit(42)
			}
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
				ref, err := store.Place(digest, int64(len(data)))
				if err != nil {
					return inference.ContentUnavailable
				}
				if err := os.WriteFile(store.Path(ref), data, 0600); err != nil {
					return inference.ContentUnavailable
				}
				if err := store.Commit(ref); err != nil {
					return inference.ContentUnavailable
				}
				return inference.ContentResolved
			}, inference.ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	host, err := service.ListenLeaf(endpoint, leaf, inference.ExecutionLocal)
	if err != nil {
		t.Fatal(err)
	}
	host.Content = content
	fmt.Println("OA_IMAGE_PROCESS_READY")
	if err := host.Serve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type imageProcess struct {
	endpoint, root, count string
	cmd                   *exec.Cmd
	stderr                bytes.Buffer
}

func startImageProcess(t *testing.T, mode string) *imageProcess {
	t.Helper()
	root := t.TempDir()
	endpoint := filepath.Join(t.TempDir(), "native-image.sock")
	if runtime.GOOS == "windows" {
		endpoint = fmt.Sprintf(`\\.\pipe\oa-image-child-%d`, time.Now().UnixNano())
	}
	count := filepath.Join(t.TempDir(), "starts.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeImageProviderChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "OA_IMAGE_PROCESS_CHILD=1", "OA_IMAGE_PROCESS_ENDPOINT="+endpoint, "OA_IMAGE_PROCESS_ROOT="+root,
		"OA_IMAGE_PROCESS_RECORDS="+filepath.Join(t.TempDir(), "uploads.json"), "OA_IMAGE_PROCESS_COUNT="+count, "OA_IMAGE_PROCESS_MODE="+mode)
	p := &imageProcess{endpoint: endpoint, root: root, count: count, cmd: cmd}
	cmd.Stderr = &p.stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		scan := bufio.NewScanner(stdout)
		if scan.Scan() {
			ready <- scan.Text()
		} else {
			ready <- ""
		}
	}()
	select {
	case line := <-ready:
		if line != "OA_IMAGE_PROCESS_READY" {
			t.Fatalf("child readiness %q: %s", line, p.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("child readiness timed out: %s", p.stderr.String())
	}
	return p
}

func imageProcessStarts(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start\n")
}

func newProcessMediator(t *testing.T, child *imageProcess, permit bool, caller *storage.ContentStore, commits *atomic.Int32) *inference.Provider {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.User{Kind: "posix", UID: os.Getuid()}
	if runtime.GOOS == "windows" {
		principal = identity.User{Kind: "windows", SID: current.Uid}
	}
	native, err := router.NewNative("child-native", child.endpoint, listen.ServerExpectation{Principal: principal, Program: identity.CanonicalProgramPath(filepath.Clean(exe))}, []string{"stability/sdxl"}, []string{router.ProfileImage})
	if err != nil {
		t.Fatal(err)
	}
	mediator, err := inference.New(inference.Config{Router: router.New(native),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) {
			if permit {
				return "permitted", nil
			}
			return "denied", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "not_permitted"
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
				commits.Add(1)
				ref, err := caller.Place(digest, int64(len(data)))
				if err != nil {
					return inference.ContentUnavailable
				}
				if err := os.WriteFile(caller.Path(ref), data, 0600); err != nil {
					return inference.ContentUnavailable
				}
				if err := caller.Commit(ref); err != nil {
					return inference.ContentUnavailable
				}
				return inference.ContentResolved
			}, inference.ContentResolved
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mediator.Close() })
	return mediator
}

func awaitProcessImage(t *testing.T, ctx context.Context, mediator *inference.Provider, subject inference.Subject, op string) wire.ImageReply {
	t.Helper()
	for cursor := int64(0); ; {
		page := mediator.ObserveImage(ctx, subject, op, cursor, 256, 65536, 1000)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe: %+v", page)
		}
		for _, delta := range page.Deltas {
			if delta.ImageEnd != nil {
				return *delta.ImageEnd
			}
		}
		if page.AtEnd {
			t.Fatalf("missing image end: %+v", page)
		}
		cursor = page.Next
	}
}

func TestNativeImageIndependentProcess(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil {
		t.Skip(err)
	}
	for _, scenario := range []struct {
		name, mode string
		permit     bool
	}{
		{"verified-output", "normal", true},
		{"denied-before-start", "normal", false},
		{"lost-start-reply", "crash-on-start", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			child := startImageProcess(t, scenario.mode)
			callerRoot := t.TempDir()
			caller, err := storage.NewContentStore("caller", callerRoot)
			if err != nil {
				t.Fatal(err)
			}
			if callerRoot == child.root {
				t.Fatal("stores unexpectedly shared")
			}
			var commits atomic.Int32
			mediator := newProcessMediator(t, child, scenario.permit, caller, &commits)
			subject := inference.Subject{Account: "caller-account", Program: "caller-program"}
			req := wire.ImageRequest{Model: "stability/sdxl", Mode: wire.ImageModeGenerate, Prompt: "a lighthouse", Size: "auto", Count: 1,
				Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			admission := mediator.StartImage(ctx, subject, req)
			if !scenario.permit {
				if admission.Outcome != wire.StartOutcomeNotPermitted || imageProcessStarts(t, child.count) != 0 || commits.Load() != 0 {
					t.Fatalf("denied call reached child: %+v starts=%d commits=%d", admission, imageProcessStarts(t, child.count), commits.Load())
				}
				return
			}
			if admission.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", admission)
			}
			end := awaitProcessImage(t, ctx, mediator, subject, admission.Operation)
			if scenario.mode == "crash-on-start" {
				if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "native:start_unknown" || imageProcessStarts(t, child.count) != 1 || commits.Load() != 0 {
					t.Fatalf("ambiguous Start replayed or published: %+v starts=%d commits=%d", end, imageProcessStarts(t, child.count), commits.Load())
				}
				return
			}
			if end.Outcome != wire.ReplyOutcomeCompleted || len(end.Deliveries) != 1 || commits.Load() != 1 || imageProcessStarts(t, child.count) != 1 {
				t.Fatalf("output not copied: %+v starts=%d commits=%d", end, imageProcessStarts(t, child.count), commits.Load())
			}
			sum := sha256.Sum256(processImagePNG)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			if end.Deliveries[0].Digest != digest {
				t.Fatalf("delivery digest %+v", end.Deliveries[0])
			}
			ref, found := caller.Find(digest)
			if !found {
				t.Fatal("caller store missing copied output")
			}
			data, err := os.ReadFile(caller.Path(ref))
			if err != nil || !bytes.Equal(data, processImagePNG) {
				t.Fatalf("caller bytes %d: %v", len(data), err)
			}
		})
	}
}
