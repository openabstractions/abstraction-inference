package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const liveIPCSecret = "sk-LIVE-IPC-FIXTURE"

type liveIPCFixture struct {
	client   *wire.LiveClient
	endpoint string
	program  string
	output   []byte
	saved    []byte
	appends  atomic.Int64
	commits  atomic.Int64

	recordMu sync.Mutex
	records  []inference.Record
	recorded chan struct{}
}

type ipcLiveConnection struct {
	f      *liveIPCFixture
	events chan inference.LiveEvent
	closed chan struct{}
	once   sync.Once
}

func (c *ipcLiveConnection) Append(_ context.Context, audio []byte) error {
	if len(audio) != 9600 {
		return inference.NewLiveError(inference.LiveErrorInvalid)
	}
	c.f.appends.Add(1)
	return nil
}

func (c *ipcLiveConnection) Commit(context.Context) error {
	c.f.commits.Add(1)
	c.events <- inference.LiveEvent{Kind: inference.LiveEventAudio, Audio: append([]byte(nil), c.f.output...)}
	c.events <- inference.LiveEvent{Kind: inference.LiveEventTranscript, Transcript: "native IPC"}
	c.events <- inference.LiveEvent{Kind: inference.LiveEventTranscript, Final: true}
	c.events <- inference.LiveEvent{Kind: inference.LiveEventTerminal, Final: true, Outcome: inference.LiveCompleted, Usage: inference.LiveUsage{InputTokens: 2, OutputTokens: 3}}
	return nil
}

func (c *ipcLiveConnection) Read(ctx context.Context) (inference.LiveEvent, error) {
	select {
	case event := <-c.events:
		return event, nil
	default:
	}
	select {
	case event := <-c.events:
		return event, nil
	case <-ctx.Done():
		return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorCancelled)
	case <-c.closed:
		return inference.LiveEvent{}, inference.NewLiveError(inference.LiveErrorCancelled)
	}
}

func (c *ipcLiveConnection) Close() { c.once.Do(func() { close(c.closed) }) }

func (f *liveIPCFixture) waitRecords(t *testing.T, count int) []inference.Record {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		f.recordMu.Lock()
		records := append([]inference.Record(nil), f.records...)
		f.recordMu.Unlock()
		if len(records) >= count {
			return records
		}
		select {
		case <-f.recorded:
		case <-timer.C:
			f.recordMu.Lock()
			records = append([]inference.Record(nil), f.records...)
			f.recordMu.Unlock()
			if len(records) >= count {
				return records
			}
			t.Fatalf("audit callback did not record %d entries: %+v", count, records)
		}
	}
}

func serveLiveIPC(t *testing.T) *liveIPCFixture {
	t.Helper()
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	f := &liveIPCFixture{output: []byte{10, 20, 30, 40}, recorded: make(chan struct{}, 8)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-realtime"}]}`))
		}
	}))
	t.Cleanup(upstream.Close)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.program = filepath.Clean(exe)
	routes := router.New(router.NewHosted("live", upstream.URL, router.WireOpenAIRealtime, "live"))
	routes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	routes.Survey()
	p, err := inference.New(inference.Config{
		Router: routes,
		HTTP:   upstream.Client(),
		LiveDialer: func(_ context.Context, client *http.Client, _, _, _ string, _ wire.LiveFormat, headers map[string]string) (inference.LiveConnection, error) {
			if client.CheckRedirect == nil || headers["Authorization"] != "Bearer "+liveIPCSecret {
				t.Error("live connector did not receive guarded credential request")
			}
			return &ipcLiveConnection{f: f, events: make(chan inference.LiveEvent, 4), closed: make(chan struct{})}, nil
		},
		SurveyAge: time.Hour,
		Idle:      2 * time.Second,
		Decide: func(_ context.Context, subject inference.Subject, action, resource string) (string, error) {
			if !strings.EqualFold(subject.Program, f.program) || subject.Account == "" || action != inference.ActionComplete || resource != "host:live" {
				return "not_granted", nil
			}
			return "permitted", nil
		},
		Apply: func(_ context.Context, subject inference.Subject, contract, credential, target string) (map[string]string, string) {
			if !strings.EqualFold(subject.Program, f.program) || subject.Account == "" || contract != inference.LiveContract || credential != "live" || target != "127.0.0.1" {
				return nil, "not_permitted"
			}
			return map[string]string{"Authorization": "Bearer " + liveIPCSecret}, "applied"
		},
		PrepareContentWrite: func(_ context.Context, subject inference.Subject, profile, media string, limit int64) (inference.ContentCommitter, inference.ContentOutcome) {
			if !strings.EqualFold(subject.Program, f.program) || subject.Account == "" || profile != "live" || media != "audio/pcm;rate=24000;channels=1;format=s16le" || limit < int64(len(f.output)) {
				return nil, inference.ContentForbidden
			}
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
				if digest != digestOf(data) {
					return inference.ContentUnavailable
				}
				f.saved = append([]byte(nil), data...)
				return inference.ContentResolved
			}, inference.ContentResolved
		},
		Record: func(record inference.Record) {
			f.recordMu.Lock()
			f.records = append(f.records, record)
			f.recordMu.Unlock()
			select {
			case f.recorded <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	host, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- host.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = p.Close()
	})
	f.endpoint = endpoint
	f.client = wire.NewLiveClient(listen.FrameClient{Endpoint: endpoint}.WithDefaults(35*time.Second, MaxFrameBytes))
	return f
}

func TestLiveNativeIPCBindsCallerAndCompletes(t *testing.T) {
	f := serveLiveIPC(t)
	request := wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "live"}
	admission, err := f.client.Start(request)
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("start = %+v, %v", admission, err)
	}
	pcm := make([]byte, 9600)
	for _, want := range []wire.LiveInputOutcome{wire.LiveInputOutcomeAccepted, wire.LiveInputOutcomeDuplicate} {
		got, err := f.client.Append(admission.Operation, 0, pcm)
		if err != nil || got.Outcome != want || got.NextSequence != 1 {
			t.Fatalf("append = %+v, %v; want %s", got, err, want)
		}
	}
	// Ownership is by bound program, so another connection from this executable
	// can commit the operation it started through the first connection.
	second := wire.NewLiveClient(listen.FrameClient{Endpoint: f.endpoint}.WithDefaults(35*time.Second, MaxFrameBytes))
	committed, err := second.Commit(admission.Operation)
	if err != nil || committed.Outcome != wire.LiveInputOutcomeAccepted || committed.NextSequence != 1 {
		t.Fatalf("commit = %+v, %v", committed, err)
	}

	var audio []byte
	var transcript strings.Builder
	finalTranscript := false
	var end *wire.LiveReply
	for cursor := int64(0); end == nil; {
		page, err := f.client.Observe(admission.Operation, cursor, 256, 65536, 2000)
		if err != nil || page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe = %+v, %v", page, err)
		}
		for _, delta := range page.Deltas {
			if delta.Audio != nil {
				audio = append(audio, delta.Audio.Data...)
			}
			if delta.Transcript != nil {
				transcript.WriteString(delta.Transcript.Text)
				finalTranscript = finalTranscript || delta.Transcript.IsFinal
			}
			if delta.LiveEnd != nil {
				copy := *delta.LiveEnd
				end = &copy
			}
		}
		cursor = page.Next
	}
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Delivery.Digest != digestOf(f.output) || end.Delivery.Size != int64(len(f.output)) || end.InputAudioBytes != int64(len(pcm)) {
		t.Fatalf("end = %+v", *end)
	}
	if !bytes.Equal(audio, f.output) || !bytes.Equal(f.saved, f.output) || transcript.String() != "native IPC" || !finalTranscript {
		t.Fatalf("audio=%v saved=%v transcript=%q final=%t", audio, f.saved, transcript.String(), finalTranscript)
	}
	if f.appends.Load() != 1 || f.commits.Load() != 1 {
		t.Fatalf("upstream appends=%d commits=%d", f.appends.Load(), f.commits.Load())
	}
	records := f.waitRecords(t, 1)
	if len(records) != 1 || !strings.EqualFold(records[0].Program, f.program) || records[0].Outcome != "completed" || records[0].Route != inference.RouteNative {
		t.Fatalf("records=%+v, program=%s", records, f.program)
	}
}

func TestLiveNativeIPCCancelledObserveAndAbsentEndpoint(t *testing.T) {
	f := serveLiveIPC(t)
	request := wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "live"}
	admission, err := f.client.Start(request)
	if err != nil || admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("start = %+v, %v", admission, err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelWait()
	waiting := wire.NewLiveClient(listen.FrameClient{Endpoint: f.endpoint}.WithDefaults(35*time.Second, MaxFrameBytes).WithContext(waitCtx))
	started := time.Now()
	if _, err := waiting.Observe(admission.Operation, 0, 1, 1024, 30000); err == nil {
		t.Fatal("cancelled Observe succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled Observe took %v", elapsed)
	}
	cancellation, err := f.client.Cancel(admission.Operation)
	if err != nil || cancellation.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel = %+v, %v", cancellation, err)
	}

	absent := endpointFor(t)
	absentCtx, cancelAbsent := context.WithTimeout(context.Background(), time.Second)
	defer cancelAbsent()
	absentClient := wire.NewLiveClient(listen.FrameClient{Endpoint: absent}.WithDefaults(time.Second, MaxFrameBytes).WithContext(absentCtx))
	if _, err := absentClient.Start(request); err == nil {
		t.Fatal("absent endpoint Start succeeded")
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
