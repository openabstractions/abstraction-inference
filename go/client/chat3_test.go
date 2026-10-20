package client

import (
	"bufio"
	"context"
	"errors"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type chat3StartPort struct {
	*chat3ManagedPort
	starts  int
	outcome wire.ChatStartOutcome
	request wire.RequestV3
}

func (p *chat3StartPort) GetHistoryWindow() (wire.ChatHistoryRead, error) {
	return wire.ChatHistoryRead{Outcome: wire.ChatHistoryOutcomeUnavailable}, nil
}
func (p *chat3StartPort) Start(r wire.RequestV3) (wire.ChatAdmission, error) {
	p.starts++
	p.request = r
	a := wire.ChatAdmission{Outcome: p.outcome}
	if p.outcome == wire.ChatStartOutcomeAccepted {
		a.Request = "retained"
		a.Server = "fixture"
		a.Model = "fixture"
		a.RetentionMs = 60000
		a.IdleMs = 10000
		a.RetainedDeltas = 256
	}
	return a, nil
}
func (p *chat3StartPort) Observe(id string, cursor, count, bytes, wait int64) (wire.ChatPage, error) {
	return p.chat3ManagedPort.Observe(context.Background(), id, cursor, count, bytes, wait)
}
func (p *chat3StartPort) Cancel(id string) (wire.ChatCancellation, error) {
	return p.chat3ManagedPort.Cancel(context.Background(), id)
}
func TestChat3ManagedStartOnce(t *testing.T) {
	for _, outcome := range []wire.ChatStartOutcome{wire.ChatStartOutcomeUnknown, wire.ChatStartOutcomeAccepted} {
		p := &chat3StartPort{chat3ManagedPort: &chat3ManagedPort{err: errors.New("observe disconnected")}, outcome: outcome}
		var wg sync.WaitGroup
		c := NewChatV3WithTransport(listen.FrameClient{Endpoint: "fixture", Dialer: func(context.Context, string) (net.Conn, error) {
			a, b := net.Pipe()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer b.Close()
				frame, e := listen.ReadFrameFrom(b, 1<<20)
				if e != nil {
					return
				}
				reply, e := (&wire.ChatV3Dispatcher{Handler: p}).ExchangeFrame(frame)
				if e == nil {
					_ = listen.WriteFrameTo(b, reply, 1<<20)
				}
			}()
			return a, nil
		}})
		var request wire.RequestV3
		if e := wire.FromJSON([]byte(`{"identity":{"key":"persisted","history_epoch":"epoch","attempt":0},"target":{"model":"fixture"},"content":{"messages":[{"role":"user","parts":[{"kind":"text","text":"prompt"}]}],"guarantees":[]}}`), &request); e != nil {
			t.Fatal(e)
		}
		s, e := c.Open(context.Background(), request, false)
		if e != nil {
			t.Fatal(e)
		}
		if s.Admission.Outcome != outcome || p.starts != 1 || p.request.Identity.Key != "persisted" || p.request.Identity.HistoryEpoch != "epoch" {
			t.Fatal("Start/correlation changed")
		}
		if outcome == wire.ChatStartOutcomeAccepted {
			if _, e = s.Next(context.Background()); e == nil {
				t.Fatal("observe error")
			}
			if s.Request != "retained" || s.Cursor != 0 || p.starts != 1 || p.cancels != 0 {
				t.Fatal("accepted retry/cancel")
			}
		}
		wg.Wait()
	}
}

func managedCorpus(t *testing.T) []wire.ChatEvent {
	t.Helper()
	f, e := os.Open(managedFixture(t, "chat3_managed.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	var events []wire.ChatEvent
	s := bufio.NewScanner(f)
	for s.Scan() {
		var v wire.ChatEvent
		if e = wire.FromJSON(s.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
		events = append(events, v)
	}
	if e = s.Err(); e != nil {
		t.Fatal(e)
	}
	return events
}
func TestChat3ManagedFold(t *testing.T) {
	events := managedCorpus(t)
	partial := NewFoldV3(false, 2)
	for _, e := range events[2:] {
		if x := partial.Add(e); x != nil {
			t.Fatal(x)
		}
	}
	suffix, _ := partial.Reply()
	if len(suffix.Blocks) != 4 || len(suffix.BlockEnds) != 5 {
		t.Fatal("suffix invented content or lost close")
	}
	for _, outcome := range []wire.ChatResultOutcome{wire.ChatResultOutcomeCompleted, wire.ChatResultOutcomeFailed, wire.ChatResultOutcomeCancelled} {
		f := NewFoldV3(false, 0)
		part := events[1]
		part.Sequence = 0
		if e := f.Add(part); e != nil {
			t.Fatal(e)
		}
		end := events[14]
		end.Sequence = 1
		terminal := *end.End
		terminal.Outcome = outcome
		terminal.Cause = "fixture"
		if outcome != wire.ChatResultOutcomeCompleted {
			terminal.FinishReason = nil
		}
		end.End = &terminal
		if e := f.Add(end); e != nil {
			t.Fatal(e)
		}
		r, _ := f.Reply()
		if len(r.BlockEnds) != 0 || len(r.Blocks) != 1 || r.Result.Outcome != outcome {
			t.Fatal("synthetic close/terminal")
		}
	}
	for _, retain := range []bool{false, true} {
		f := FoldV3{RetainReasoning: retain}
		for i, e := range events {
			if _, ok := f.Reply(); ok {
				t.Fatal("synthetic terminal")
			}
			if x := f.Add(e); x != nil {
				t.Fatal(i, x)
			}
		}
		r, ok := f.Reply()
		if !ok || r.Text != "Hello world" || len(r.Blocks) != 5 || r.Blocks[3].Arguments != "{}" || r.Blocks[4].Text != "declined" {
			t.Fatal(r)
		}
		if (r.Blocks[0].Text != "") != retain || (r.Blocks[1].Text != "") != retain {
			t.Fatal("reasoning retention")
		}
		if r.Blocks[0].Seal.Origin != "vendor/opaque" || string(r.Blocks[0].Seal.Bytes) != string([]byte{0, 255, 65}) || r.BlockEnds[1].SealOmitted == nil {
			t.Fatal("opaque seal/omission")
		}
		if f.Progress.Done == nil || *f.Progress.Done != 0 || f.Progress.Total != nil || r.Result.Usage.Input == nil || *r.Result.Usage.Input != 0 || r.Result.Usage.Reasoning != nil || r.Result.ServedBy.Server != "fixture" || *r.Result.FinishReason != wire.FinishReasonOther {
			t.Fatal("terminal observations")
		}
		if f.Add(events[14]) == nil {
			t.Fatal("event after terminal")
		}
		timing := r.Result.Timing
		if timing == nil || timing.QueueMs == nil || *timing.QueueMs != 0 || *timing.LoadMs != 31 || *timing.PrefillMs != 14 || *timing.DecodeMs != 20 || *timing.ReusedTokens != 0 || timing.RestoreMs != nil || string(*timing.Load) != "future_load" || string(*timing.LoadSource) != "future_source" {
			t.Fatal("terminal timing observations")
		}
		if string(*events[14].End.FinishReason) != "future_vendor_finish" {
			t.Fatal("raw finish mutated")
		}
	}
	f := FoldV3{}
	for _, e := range events[:3] {
		if x := f.Add(e); x != nil {
			t.Fatal(x)
		}
	}
	d := events[1]
	d.Sequence = 3
	if f.Add(d) == nil {
		t.Fatal("closed block append")
	}
	f = FoldV3{}
	if f.Add(events[1]) == nil {
		t.Fatal("sequence gap")
	}
	if x := f.Add(events[0]); x != nil {
		t.Fatal(x)
	}
	if f.Add(events[0]) == nil {
		t.Fatal("duplicate")
	}
}

func managedFixture(t *testing.T, name string) string {
	t.Helper()
	// The fixture owner binds this source copy to the canonical corpus; the
	// same manifest publishes that corpus at this mandatory public path.
	path := filepath.Join("testdata", name)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("missing maintained client fixture %s: %v", name, err)
	}
	return path
}

func TestChat3ManagedPolicyLocalOnlyCodec(t *testing.T) {
	data, err := os.ReadFile(managedFixture(t, "chat3_policy_local_only.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request wire.RequestV3
	if err = wire.FromJSON(data, &request); err != nil {
		t.Fatal(err)
	}
	encoded, err := wire.ToJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip wire.RequestV3
	if err = wire.FromJSON(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
	for _, r := range []wire.RequestV3{request, roundtrip} {
		if r.Target.SelectionPolicy != "private" || r.Target.Model != "" || len(r.Content.Guarantees) != 1 || r.Content.Guarantees[0] != wire.ChatGuaranteeLocalOnly || r.Content.Credential != "" || r.Identity.Key != "privacy-key" || r.Identity.HistoryEpoch != "epoch" {
			t.Fatal("policy privacy/correlation lost", r)
		}
	}
}

type chat3ManagedPort struct {
	page           wire.ChatPage
	err            error
	calls, cancels int
	id             string
	cursor         int64
}

func (p *chat3ManagedPort) Observe(_ context.Context, id string, cursor, _, _, _ int64) (wire.ChatPage, error) {
	p.calls++
	p.id = id
	p.cursor = cursor
	return p.page, p.err
}
func (p *chat3ManagedPort) Cancel(context.Context, string) (wire.ChatCancellation, error) {
	p.cancels++
	return wire.ChatCancellation{}, nil
}
func TestChat3ManagedWaitPreservesHandle(t *testing.T) {
	p := &chat3ManagedPort{err: context.DeadlineExceeded}
	s := &ChatV3Session{Chat: p, Request: "retained", Cursor: 7, Fold: FoldV3{Cursor: 7}}
	for _, e := range []error{context.DeadlineExceeded, errors.New("disconnect")} {
		p.err = e
		if _, x := s.Next(context.Background()); x == nil {
			t.Fatal("missing error")
		}
		if s.Request != "retained" || s.Cursor != 7 || s.Fold.Cursor != 7 || p.cancels != 0 {
			t.Fatal("lost accepted work")
		}
	}
	p.err = nil
	p.page = wire.ChatPage{Outcome: wire.PageOutcomeGap, Next: 9}
	if _, e := s.Next(context.Background()); e == nil {
		t.Fatal("gap")
	}
	if s.Cursor != 7 || p.cancels != 0 {
		t.Fatal("gap cancelled")
	}
	p.page = wire.ChatPage{Outcome: wire.PageOutcomePage, Events: managedCorpus(t), Next: 15, AtEnd: true}
	s.Cursor = 0
	s.Fold = FoldV3{}
	if _, e := s.Next(context.Background()); e != nil {
		t.Fatal(e)
	}
	if s.Cursor != 15 || p.id != "retained" || p.cursor != 0 || p.cancels != 0 {
		t.Fatal("correlation")
	}
	if _, e := s.Cancel(context.Background()); e != nil || p.cancels != 1 {
		t.Fatal("explicit cancel")
	}
}
