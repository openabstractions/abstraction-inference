package service

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-inference/go/client"
	router "github.com/openabstractions/abstraction-router/go"
)

// operatorRecorder answers every operator call and records the subject each
// call was bound to.
type operatorRecorder struct {
	mu       sync.Mutex
	subjects []inference.Subject
}

func (o *operatorRecorder) saw(s inference.Subject) {
	o.mu.Lock()
	o.subjects = append(o.subjects, s)
	o.mu.Unlock()
}

func (o *operatorRecorder) Hosts(_ context.Context, s inference.Subject) wire.HostList {
	o.saw(s)
	return wire.HostList{Outcome: wire.ListOutcomePage, Revision: "hosts-v1:x", Hosts: []wire.HostState{{Entry: wire.HostEntry{Name: "ollama", Kind: "ollama", Base: "http://127.0.0.1:11434"}, Up: true}}}
}
func (o *operatorRecorder) AddHost(_ context.Context, s inference.Subject, _ string, _ wire.HostEntry) wire.HostChange {
	o.saw(s)
	return wire.HostChange{Outcome: wire.EditOutcomeConflict, Revision: "hosts-v1:y", Reason: "revision"}
}
func (o *operatorRecorder) RemoveHost(_ context.Context, s inference.Subject, _, _ string) wire.HostChange {
	o.saw(s)
	return wire.HostChange{Outcome: wire.EditOutcomeUnknown}
}
func (o *operatorRecorder) Keys(_ context.Context, s inference.Subject) wire.KeyList {
	o.saw(s)
	return wire.KeyList{Outcome: wire.ListOutcomeForbidden, Keys: []wire.LocalKey{}}
}
func (o *operatorRecorder) IssueKey(_ context.Context, s inference.Subject, _, _ string) wire.KeyIssued {
	o.saw(s)
	return wire.KeyIssued{Outcome: wire.EditOutcomeNoSecureStore}
}
func (o *operatorRecorder) RevokeKey(_ context.Context, s inference.Subject, _ string) wire.KeyRevoked {
	o.saw(s)
	return wire.KeyRevoked{Outcome: wire.EditOutcomeUnknown}
}
func (o *operatorRecorder) Gateway(_ context.Context, s inference.Subject) wire.GatewayState {
	o.saw(s)
	return wire.GatewayState{Outcome: wire.ListOutcomePage, Revision: "gateway-v1:x", Open: true, Address: "127.0.0.1:8793", Listening: true, ListeningAddress: "127.0.0.1:8793"}
}
func (o *operatorRecorder) SetGateway(_ context.Context, s inference.Subject, _ string, _ bool, _ string) wire.GatewayChange {
	o.saw(s)
	return wire.GatewayChange{Outcome: wire.EditOutcomeInvalid, Reason: "address"}
}
func (o *operatorRecorder) Audit(_ context.Context, s inference.Subject, cursor, _ int64) wire.AuditPage {
	o.saw(s)
	return wire.AuditPage{Outcome: wire.AuditOutcomePage, Entries: []wire.AuditEntry{{Sequence: cursor, Route: wire.AuditRouteWindow, Outcome: "completed"}}, Next: cursor + 1, AtEnd: true}
}

// operator@1 shares chat@1's endpoint, every call reaches the operator with
// the program the boundary bound, and a chat@1 admission records the native
// route and the rung of that binding.
func TestOperatorProfileSharesTheEndpointAndBindsTheCaller(t *testing.T) {
	if err := identity.CanEver(listen.Program); err != nil || runtime.GOOS == "darwin" {
		t.Skip("Program proof unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var records []inference.Record
	p, err := inference.New(inference.Config{Router: router.New(),
		Decide: func(context.Context, inference.Subject, string, string) (string, error) { return "not_granted", nil },
		Apply: func(context.Context, inference.Subject, string, string, string) (map[string]string, string) {
			return nil, "unknown"
		},
		Record: func(r inference.Record) { mu.Lock(); records = append(records, r); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := endpointFor(t)
	h, err := Listen(endpoint, p)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &operatorRecorder{}
	h.Operator = recorder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; p.Close() })

	operator := client.NewOperator(endpoint)
	call, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	hosts, err := operator.Hosts(call)
	if err != nil || hosts.Outcome != wire.ListOutcomePage || len(hosts.Hosts) != 1 {
		t.Fatalf("hosts %+v %v", hosts, err)
	}
	if change, err := operator.AddHost(call, "stale", wire.HostEntry{Name: "x", Kind: "ollama", Base: "http://127.0.0.1:1"}); err != nil || change.Outcome != wire.EditOutcomeConflict {
		t.Fatalf("add %+v %v", change, err)
	}
	if keys, err := operator.Keys(call); err != nil || keys.Outcome != wire.ListOutcomeForbidden {
		t.Fatalf("keys %+v %v", keys, err)
	}
	if issued, err := operator.IssueKey(call, exe, ""); err != nil || issued.Outcome != wire.EditOutcomeNoSecureStore {
		t.Fatalf("issue %+v %v", issued, err)
	}
	if page, err := operator.Audit(call, 7, 16); err != nil || page.Outcome != wire.AuditOutcomePage || page.Entries[0].Sequence != 7 {
		t.Fatalf("audit %+v %v", page, err)
	}
	if state, err := operator.Gateway(call); err != nil || !state.Open || state.ListeningAddress != "127.0.0.1:8793" {
		t.Fatalf("gateway %+v %v", state, err)
	}
	if change, err := operator.SetGateway(call, "gateway-v1:x", true, "0.0.0.0:8793"); err != nil || change.Outcome != wire.EditOutcomeInvalid || change.Reason != "address" {
		t.Fatalf("set gateway %+v %v", change, err)
	}
	recorder.mu.Lock()
	for _, s := range recorder.subjects {
		if !strings.EqualFold(s.Program, filepath.Clean(exe)) || s.Account == "" {
			t.Fatalf("operator saw subject %+v; this program is %s", s, exe)
		}
	}
	recorder.mu.Unlock()

	chat := client.New(endpoint)
	reply, err := chat.Complete(call, wire.Request{Model: "nothing:1b", Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly},
		Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "hi"}}}}})
	if err != nil || reply.Outcome != wire.ReplyOutcomeNoHost {
		t.Fatalf("complete %+v %v", reply, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 || records[0].Route != inference.RouteNative || !strings.Contains(records[0].Rung, "/"+runtime.GOOS+" user=kernel process=kernel path=bound") {
		t.Fatalf("native record %+v", records)
	}
	t.Logf("native rung %q", records[0].Rung)
}
