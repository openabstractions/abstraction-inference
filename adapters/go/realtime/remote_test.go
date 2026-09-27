package realtime_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	websocket "github.com/openabstractions/abstraction-inference/adapters/go/transportws"
	identityremote "github.com/openabstractions/abstraction-identity/remote"
	"github.com/openabstractions/abstraction-inference/adapters/go/realtime"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	infservice "github.com/openabstractions/abstraction-inference/go/service"
	router "github.com/openabstractions/abstraction-router/go"
	routerwire "github.com/openabstractions/abstraction-router/go/abstraction/router"
)

type remoteLiveFixture struct {
	local, remote   *inference.Provider
	localCaller     inference.Subject
	output          []byte
	saved           []byte
	appends         atomic.Int64
	remoteApply     atomic.Int64
	remotePrepare   atomic.Int64
	handlerCalls    atomic.Int64
	dropAppendReply atomic.Bool
	localCeilings   *inference.Ceilings
	remoteCeilings  *inference.Ceilings

	recordMu      sync.Mutex
	remoteRecords []inference.Record
	localRecords  []inference.Record
	recorded      chan struct{}
}

func newRemoteLiveFixture(t *testing.T) *remoteLiveFixture {
	t.Helper()
	f := &remoteLiveFixture{localCaller: inference.Subject{Account: "local-account", Program: "/apps/voice-client"}, output: []byte{2, 4, 6, 8}, recorded: make(chan struct{}, 8)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-realtime"}]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer remote-secret" {
			http.Error(w, "refused", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		if _, _, err = conn.Read(ctx); err != nil {
			return
		}
		if conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated"}`)) != nil {
			return
		}
		for {
			_, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(payload, &event) != nil {
				return
			}
			switch event.Type {
			case "input_audio_buffer.append":
				f.appends.Add(1)
			case "response.create":
				for _, message := range []string{
					`{"type":"response.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(f.output) + `"}`,
					`{"type":"response.output_audio_transcript.delta","delta":"remote"}`,
					`{"type":"response.output_audio_transcript.done","transcript":"remote"}`,
					`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":13}}}`,
				} {
					if conn.Write(ctx, websocket.MessageText, []byte(message)) != nil {
						return
					}
				}
			}
		}
	}))
	t.Cleanup(upstream.Close)

	remoteCeilings, err := inference.OpenCeilings(filepath.Join(t.TempDir(), "remote-ceilings.json"), map[string]inference.Ceiling{"remote-key": {}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.remoteCeilings = remoteCeilings
	remoteRoutes := router.New(router.NewHosted("voice", upstream.URL, router.WireOpenAIRealtime, "remote-key"))
	remoteRoutes.UseCredentials(func(context.Context, string, string, string) (map[string]string, error) { return nil, nil })
	remoteRoutes.Survey()
	remoteSubject := inference.Subject{Account: "trusted-runtime", Program: "/services/openabstractions"}
	remoteProvider, err := inference.New(inference.Config{
		Router: remoteRoutes, HTTP: upstream.Client(), LiveDialer: realtime.Dial, SurveyAge: time.Hour, Idle: 5 * time.Second, Ceilings: remoteCeilings,
		Decide: func(_ context.Context, subject inference.Subject, action, resource string) (string, error) {
			if subject != remoteSubject || action != inference.ActionComplete || resource != "host:voice" {
				return "not_granted", nil
			}
			return "permitted", nil
		},
		Apply: func(_ context.Context, subject inference.Subject, contract, credential, target string) (map[string]string, string) {
			if subject != remoteSubject || contract != inference.LiveContract || credential != "remote-key" || target != "127.0.0.1" {
				return nil, "not_permitted"
			}
			f.remoteApply.Add(1)
			return map[string]string{"Authorization": "Bearer remote-secret"}, "applied"
		},
		PrepareContentWrite: func(_ context.Context, subject inference.Subject, profile, media string, _ int64) (inference.ContentCommitter, inference.ContentOutcome) {
			if subject != remoteSubject || profile != "live" || media != "audio/pcm;rate=24000;channels=1;format=s16le" {
				return nil, inference.ContentForbidden
			}
			f.remotePrepare.Add(1)
			return func(_ context.Context, digest string, data []byte) inference.ContentOutcome {
				if digest != digestBytes(data) {
					return inference.ContentUnavailable
				}
				f.saved = append([]byte(nil), data...)
				return inference.ContentResolved
			}, inference.ContentResolved
		},
		Record: func(record inference.Record) {
			f.recordMu.Lock()
			f.remoteRecords = append(f.remoteRecords, record)
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
	f.remote = remoteProvider
	t.Cleanup(func() { _ = remoteProvider.Close() })

	serverTLS, clientTLS := remoteLiveTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	remoteCtx, stopRemote := context.WithCancel(context.Background())
	remoteDone := make(chan error, 1)
	remoteHandler := infservice.RemoteHandler(remoteProvider, remoteRoutes, func(_ context.Context, peer identityremote.Peer) (inference.Subject, string, error) {
		f.handlerCalls.Add(1)
		if peer.Key == ([32]byte{}) {
			return inference.Subject{}, "", errors.New("missing authenticated peer")
		}
		return remoteSubject, "local-runtime", nil
	})
	handler := func(ctx context.Context, peer identityremote.Peer, frame []byte) ([]byte, error) {
		reply, err := remoteHandler(ctx, peer, frame)
		var call struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(frame, &call)
		if call.Method == "Append" && f.dropAppendReply.CompareAndSwap(true, false) {
			return nil, nil
		}
		return reply, err
	}
	go func() {
		remoteDone <- (identityremote.Server{TLS: serverTLS, Handler: handler, Timeout: 30 * time.Second, MaxFrame: 8 << 20}).Serve(remoteCtx, listener)
	}()
	t.Cleanup(func() {
		stopRemote()
		if err := <-remoteDone; !errors.Is(err, context.Canceled) {
			t.Errorf("remote server: %v", err)
		}
	})

	// A mismatched server name must fail before the authenticated handler runs.
	wrongTLS := clientTLS.Clone()
	wrongTLS.ServerName = "other-runtime.test"
	wrongHost, err := router.NewRemote("wrong", listener.Addr().String(), wrongTLS, "remote-key")
	if err != nil {
		t.Fatal(err)
	}
	wrongTransport, _ := wrongHost.RemoteTransport()
	if _, err := routerwire.NewRouterClient(wrongTransport.WithContext(context.Background())).Models(false); err == nil {
		t.Fatal("remote transport accepted the wrong target identity")
	}
	if f.handlerCalls.Load() != 0 {
		t.Fatal("wrong target identity reached the remote handler")
	}

	remoteHost, err := router.NewRemote("lab", listener.Addr().String(), clientTLS, "remote-key")
	if err != nil {
		t.Fatal(err)
	}
	remoteHost.Profiles = []string{router.ProfileLive}
	localRoutes := router.New(remoteHost)
	localRoutes.Survey()
	localCeilings, err := inference.OpenCeilings(filepath.Join(t.TempDir(), "local-ceilings.json"), map[string]inference.Ceiling{"remote-key": {RequestsPerDay: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := localCeilings.AddUsage("remote-key", 99, 0, 9); err != nil {
		t.Fatal(err)
	}
	f.localCeilings = localCeilings
	localProvider, err := inference.New(inference.Config{
		Router: localRoutes, SurveyAge: time.Hour, Idle: 5 * time.Second, Ceilings: localCeilings,
		Decide: func(_ context.Context, subject inference.Subject, action, resource string) (string, error) {
			if subject != f.localCaller || action != inference.ActionComplete || resource != "host:lab" {
				return "not_granted", nil
			}
			return "permitted", nil
		},
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			t.Error("local runtime applied the remote credential")
			return nil, "not_permitted"
		},
		PrepareContentWrite: func(context.Context, inference.Subject, string, string, int64) (inference.ContentCommitter, inference.ContentOutcome) {
			t.Error("local runtime prepared a writer for remote output")
			return nil, inference.ContentUnavailable
		},
		Record: func(record inference.Record) {
			f.recordMu.Lock()
			f.localRecords = append(f.localRecords, record)
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
	f.local = localProvider
	t.Cleanup(func() { _ = localProvider.Close() })
	return f
}

func TestLiveRemoteForwardsFramesAndRemoteOwnedResult(t *testing.T) {
	f := newRemoteLiveFixture(t)
	req := wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "remote-key"}
	admission := f.local.StartLive(context.Background(), f.localCaller, req)
	if admission.Outcome != wire.StartOutcomeAccepted || admission.Host != "lab" {
		t.Fatalf("start = %+v", admission)
	}
	pcm := make([]byte, 9600)
	for _, want := range []wire.LiveInputOutcome{wire.LiveInputOutcomeAccepted, wire.LiveInputOutcomeDuplicate} {
		got := f.local.AppendLive(context.Background(), f.localCaller, admission.Operation, 0, pcm)
		if got.Outcome != want || got.NextSequence != 1 {
			t.Fatalf("append = %+v; want %s", got, want)
		}
	}
	if got := f.local.CommitLive(context.Background(), f.localCaller, admission.Operation); got.Outcome != wire.LiveInputOutcomeAccepted {
		t.Fatalf("commit = %+v", got)
	}
	end, deltas := observeRemoteLiveEnd(t, f.local, f.localCaller, admission.Operation)
	if end.Outcome != wire.ReplyOutcomeCompleted || end.Delivery.Digest != digestBytes(f.output) || end.Delivery.Location != "lab" || end.Usage.Input != 11 || end.Usage.Output != 13 {
		t.Fatalf("end = %+v", end)
	}
	var relayed []byte
	for _, delta := range deltas {
		if delta.Audio != nil {
			relayed = append(relayed, delta.Audio.Data...)
		}
	}
	if !bytes.Equal(relayed, f.output) || !bytes.Equal(f.saved, f.output) || f.appends.Load() != 1 || f.remoteApply.Load() != 1 || f.remotePrepare.Load() != 1 {
		t.Fatalf("relay=%v saved=%v appends=%d apply=%d prepare=%d", relayed, f.saved, f.appends.Load(), f.remoteApply.Load(), f.remotePrepare.Load())
	}
	_, localTokens, _, localRequests, _, localSeconds, _ := f.localCeilings.SpendUnits("remote-key")
	if localTokens != 99 || localRequests != 1 || localSeconds != 9 {
		t.Fatalf("local runtime counted remote spend: tokens=%d requests=%d seconds=%d", localTokens, localRequests, localSeconds)
	}
	_, remoteTokens, _, remoteRequests, _, _, _ := f.remoteCeilings.SpendUnits("remote-key")
	if remoteTokens != 24 || remoteRequests != 1 {
		t.Fatalf("remote spend = tokens %d requests %d", remoteTokens, remoteRequests)
	}
	recordDeadline := time.NewTimer(3 * time.Second)
	defer recordDeadline.Stop()
	for {
		f.recordMu.Lock()
		haveBothRecords := len(f.remoteRecords) >= 1 && len(f.localRecords) >= 1
		f.recordMu.Unlock()
		if haveBothRecords {
			break
		}
		select {
		case <-f.recorded:
		case <-recordDeadline.C:
			t.Fatal("live completion records were not flushed")
		}
	}
	f.recordMu.Lock()
	records := append([]inference.Record(nil), f.remoteRecords...)
	localRecords := append([]inference.Record(nil), f.localRecords...)
	f.recordMu.Unlock()
	if len(records) != 1 || records[0].Route != inference.RouteRemote || records[0].Domain != "local-runtime" || records[0].Claim != f.localCaller.Program || records[0].Outcome != "completed" {
		t.Fatalf("remote records = %+v", records)
	}
	if len(localRecords) != 1 || localRecords[0].Domain != "lab" || localRecords[0].Credential != "remote-key" || localRecords[0].Outcome != "completed" {
		t.Fatalf("local records = %+v", localRecords)
	}
}

func TestLiveRemoteCancelPropagates(t *testing.T) {
	f := newRemoteLiveFixture(t)
	req := wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "remote-key"}
	admission := f.local.StartLive(context.Background(), f.localCaller, req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(admission)
	}
	if got := f.local.AppendLive(context.Background(), f.localCaller, admission.Operation, 0, make([]byte, 9600)); got.Outcome != wire.LiveInputOutcomeAccepted {
		t.Fatal(got)
	}
	cancelled := f.local.CancelLive(f.localCaller, admission.Operation)
	if cancelled.Outcome != wire.CancelOutcomeCancelled {
		t.Fatalf("cancel = %+v", cancelled)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.recordMu.Lock()
		records := append([]inference.Record(nil), f.remoteRecords...)
		f.recordMu.Unlock()
		for _, record := range records {
			if record.Outcome == "cancelled" && record.Reason == "cancel" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote cancel was not recorded: %+v", records)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLiveRemoteUncertainAppendCancelsRemote(t *testing.T) {
	f := newRemoteLiveFixture(t)
	req := wire.LiveRequest{Model: "gpt-realtime", Voice: "marin", Format: wire.LiveFormatPcm1624000,
		Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "remote-key"}
	admission := f.local.StartLive(context.Background(), f.localCaller, req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatal(admission)
	}
	f.dropAppendReply.Store(true)
	result := f.local.AppendLive(context.Background(), f.localCaller, admission.Operation, 0, make([]byte, 9600))
	if result.Outcome != wire.LiveInputOutcomeUnavailable || result.NextSequence != 0 {
		t.Fatalf("append = %+v", result)
	}
	end, _ := observeRemoteLiveEnd(t, f.local, f.localCaller, admission.Operation)
	if end.Outcome != wire.ReplyOutcomeUnavailable || end.Reason != "upstream:write" {
		t.Fatalf("local end = %+v", end)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.recordMu.Lock()
		records := append([]inference.Record(nil), f.remoteRecords...)
		f.recordMu.Unlock()
		for _, record := range records {
			if record.Outcome == "cancelled" && record.Reason == "cancel" {
				if f.appends.Load() != 1 {
					t.Fatalf("remote append count = %d", f.appends.Load())
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("uncertain append did not cancel remote: %+v", records)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func observeRemoteLiveEnd(t *testing.T, provider *inference.Provider, subject inference.Subject, operation string) (wire.LiveReply, []wire.Delta) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var deltas []wire.Delta
	for cursor := int64(0); ctx.Err() == nil; {
		page := provider.ObserveLive(ctx, subject, operation, cursor, 256, 65536, 500)
		if page.Outcome != wire.PageOutcomePage {
			t.Fatalf("observe = %+v", page)
		}
		deltas = append(deltas, page.Deltas...)
		for _, delta := range page.Deltas {
			if delta.LiveEnd != nil {
				return *delta.LiveEnd, deltas
			}
		}
		cursor = page.Next
	}
	t.Fatal("remote live operation timed out")
	return wire.LiveReply{}, nil
}

func remoteLiveTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "live remote test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	certificate := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"runtime.test"},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, public, caPrivate)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private}
	}
	server := &tls.Config{Certificates: []tls.Certificate{certificate(2, x509.ExtKeyUsageServerAuth)}, ClientCAs: roots}
	client := &tls.Config{Certificates: []tls.Certificate{certificate(3, x509.ExtKeyUsageClientAuth)}, RootCAs: roots, ServerName: "runtime.test"}
	return server, client
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
