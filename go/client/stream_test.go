package client

import (
	"context"
	"errors"
	"iter"
	"net"
	"sync"
	"testing"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type scriptedChat struct {
	admission                Admission
	pages                    []DeltaPage
	startError, observeError bool
	starts, cancels          int
	cursors                  []int64
	request                  Request
	requestV2                RequestV2
}

func (s *scriptedChat) Start(q Request) (Admission, error) {
	s.starts++
	s.request = q
	if s.startError {
		return Admission{}, errors.New("fixture start")
	}
	return s.admission, nil
}
func (s *scriptedChat) Observe(operation string, cursor, maxDeltas, maxBytes, wait int64) (DeltaPage, error) {
	if operation != "operation" || maxDeltas != PageDeltas || maxBytes != PageBytes || wait != PageWait.Milliseconds() {
		return DeltaPage{}, errors.New("wrong observation bounds or operation")
	}
	s.cursors = append(s.cursors, cursor)
	if s.observeError {
		return DeltaPage{}, errors.New("fixture observation")
	}
	p := s.pages[0]
	s.pages = s.pages[1:]
	return p, nil
}
func (s *scriptedChat) Cancel(operation string) (Cancellation, error) {
	if operation != "operation" {
		return Cancellation{}, errors.New("wrong cancellation operation")
	}
	s.cancels++
	return Cancellation{Outcome: wire.CancelOutcomeCancelled}, nil
}

type scriptedChatV2 struct{ *scriptedChat }

func (s scriptedChatV2) StartWithGuarantees(RequestV2, []wire.RequestGuarantee) (AdmissionV2, error) {
	return AdmissionV2{}, errors.New("unused caller guarantees")
}

func (s scriptedChatV2) Start(q RequestV2) (AdmissionV2, error) {
	s.requestV2 = q
	a, err := s.scriptedChat.Start(Request{Model: q.Target.Model, Messages: q.Content.Messages, Guarantees: q.Content.Guarantees, Credential: q.Content.Credential, Options: q.Content.Options})
	v := AdmissionV2{Outcome: a.Outcome, Reason: a.Reason, Operation: a.Operation, Host: a.Host, Model: a.Model, RetentionMs: a.RetentionMs, IdleMs: a.IdleMs, RetainedDeltas: a.RetainedDeltas}
	if a.Outcome == wire.StartOutcomeAccepted {
		reason := wire.SelectionReasonExplicitModel
		v.PreferredModel = q.Target.Model
		v.SelectionReason = &reason
	}
	return v, err
}
func chatStreamFixture(t *testing.T, v2 bool, s *scriptedChat) (func(context.Context) iter.Seq2[Delta, error], func()) {
	t.Helper()
	var wg sync.WaitGroup
	transport := listen.FrameClient{Endpoint: "memory", Dialer: func(context.Context, string) (net.Conn, error) {
		a, b := net.Pipe()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer b.Close()
			frame, err := listen.ReadFrameFrom(b, 1<<20)
			if err != nil {
				return
			}
			var reply []byte
			if v2 {
				reply, err = (&wire.ChatV2Dispatcher{Handler: scriptedChatV2{s}}).ExchangeFrame(frame)
			} else {
				reply, err = (&wire.ChatDispatcher{Handler: s}).ExchangeFrame(frame)
			}
			if err == nil {
				_ = listen.WriteFrameTo(b, reply, 1<<20)
			}
		}()
		return a, nil
	}}
	wait := func() { wg.Wait() }
	t.Cleanup(wait)
	request := Request{Model: "chosen/model", Messages: []Message{{Role: RoleUser, Parts: []Part{{Kind: PartKindText, Text: "prompt"}}}}, Guarantees: []RequestGuarantee{RequestGuaranteeHostedAllowed}, Credential: "named", Options: &Options{MaxOutput: 32}}
	if v2 {
		c := NewChatV2WithTransport(transport)
		return func(ctx context.Context) iter.Seq2[Delta, error] {
			return c.Stream(ctx, RequestV2{Target: Target{Model: request.Model}, Content: RequestContent{Messages: request.Messages, Guarantees: request.Guarantees, Credential: request.Credential, Options: request.Options}})
		}, wait
	}
	c := NewWithTransport(transport)
	return func(ctx context.Context) iter.Seq2[Delta, error] { return c.Stream(ctx, request) }, wait
}
func TestChatVersionsShareStreamLifetime(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		for _, mode := range []string{"complete", "terminal-break", "early-break", "cancel-during-yield", "gap", "refused-page", "malformed-page", "observe-error", "start-refusal", "inconsistent-refusal", "start-error", "malformed-admission", "cancelled-before-start"} {
			name := "chat1/"
			if v2 {
				name = "chat2/"
			}
			t.Run(name+mode, func(t *testing.T) {
				s := &scriptedChat{admission: Admission{Outcome: wire.StartOutcomeAccepted, Operation: "operation", Host: "host", Model: "chosen/model", RetainedDeltas: 16, IdleMs: 1000, RetentionMs: 1000}, pages: []DeltaPage{
					{Outcome: wire.PageOutcomePage, Deltas: []Delta{{Sequence: 0, Kind: wire.DeltaKindPart, Part: &Part{Kind: wire.PartKindText, Text: "hello"}}}, Next: 1},
					{Outcome: wire.PageOutcomePage, Deltas: []Delta{{Sequence: 1, Kind: wire.DeltaKindEnd, End: &Reply{Outcome: wire.ReplyOutcomeCompleted, StopReason: wire.StopReasonEnd, Message: Message{Role: RoleAssistant, Parts: []Part{}}}}}, Next: 2, AtEnd: true},
				}}
				wantCancel, wantError := 0, false
				switch mode {
				case "early-break", "cancel-during-yield":
					wantCancel = 1
				case "gap":
					s.pages = []DeltaPage{{Outcome: wire.PageOutcomeGap, Next: 4}}
					wantCancel, wantError = 1, true
				case "refused-page":
					s.pages = []DeltaPage{{Outcome: wire.PageOutcomeForbidden}}
					wantCancel, wantError = 1, true
				case "malformed-page":
					s.pages[0].Next = 3
					wantCancel, wantError = 1, true
				case "observe-error":
					s.observeError = true
					wantCancel, wantError = 1, true
				case "start-refusal":
					s.admission = Admission{Outcome: wire.StartOutcomeNotPermitted, Reason: "rights:complete"}
				case "inconsistent-refusal":
					s.admission = Admission{Outcome: wire.StartOutcomeNotPermitted, Host: "untrusted-refusal-host", RetainedDeltas: 16}
					wantError = true
				case "start-error":
					s.startError = true
					wantError = true
				case "malformed-admission":
					s.admission.Operation = ""
					wantError = true
				case "cancelled-before-start":
					wantError = true
				}
				stream, wait := chatStreamFixture(t, v2, s)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if mode == "cancelled-before-start" {
					cancel()
				}
				var gotError error
				var deltas []Delta
				for d, err := range stream(ctx) {
					if err != nil {
						gotError = err
						break
					}
					deltas = append(deltas, d)
					if mode == "cancel-during-yield" {
						cancel()
						break
					}
					if mode == "early-break" || mode == "terminal-break" && d.Kind == wire.DeltaKindEnd {
						break
					}
				}
				wait()
				wantStarts := 1
				if mode == "cancelled-before-start" {
					wantStarts = 0
				}
				if s.starts != wantStarts || s.cancels != wantCancel || (gotError != nil) != wantError {
					t.Fatalf("starts %d cancels %d error %v", s.starts, s.cancels, gotError)
				}
				if mode == "gap" && !errors.Is(gotError, ErrGap) {
					t.Fatalf("gap lost: %v", gotError)
				}
				if (mode == "malformed-admission" || mode == "inconsistent-refusal") && !errors.Is(gotError, ErrInconsistent) {
					t.Fatalf("admission consistency owner lost: %v", gotError)
				}
				if mode == "start-refusal" && (len(deltas) != 1 || deltas[0].End == nil || deltas[0].End.Outcome != wire.ReplyOutcomeNotPermitted || deltas[0].End.Reason != "rights:complete" || len(s.cursors) != 0) {
					t.Fatalf("refusal: %+v", deltas)
				}
				if mode == "complete" || mode == "terminal-break" {
					if len(s.cursors) != 2 || s.cursors[0] != 0 || s.cursors[1] != 1 {
						t.Fatalf("cursors: %v", s.cursors)
					}
				}
				if s.starts > 0 && !s.startError {
					if s.request.Model != "chosen/model" || s.request.Credential != "named" || s.request.Options.MaxOutput != 32 || s.request.Messages[0].Parts[0].Text != "prompt" {
						t.Fatalf("request: %+v", s.request)
					}
					if v2 && (s.requestV2.Target.Model != "chosen/model" || s.requestV2.Target.SelectionPolicy != "") {
						t.Fatalf("target: %+v", s.requestV2.Target)
					}
				}
			})
		}
	}
}
