package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

type timingAdapter func(context.Context, *stream) wire.Reply

type timingObservationBody struct {
	io.Reader
	close func() error
}

func (b timingObservationBody) Close() error { return b.close() }

func TestExecutionObservationBodyClosePrecedesOwnedFinalRecord(t *testing.T) {
	for _, observation := range []ExecutionObservation{{"hit", "resident", false}, {"miss", "not_resident", true}, {"miss", "program_change", true}, {"miss", "missing_partition", false}, {"miss", "unlabelled", false}, {"miss", "request_failed", false}} {
		t.Run(observation.Cause, func(t *testing.T) {
			base := time.Now()
			now := base
			var records []Record
			var report func(ExecutionObservation) *int64
			var cost *int64
			client := &http.Client{Transport: imageRoundTrip(func(request *http.Request) (*http.Response, error) {
				report = func(value ExecutionObservation) *int64 { return ReportExecutionObservation(request.Context(), value) }
				now = base.Add(14 * time.Millisecond)
				body := timingObservationBody{Reader: strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}],\"timings\":{\"prompt_ms\":999}}\n\ndata: [DONE]\n\n"), close: func() error {
					now = base.Add(34 * time.Millisecond)
					cost = report(observation)
					report(observation) // One ended record and at most one eviction.
					return nil
				}}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
			})}
			p := &Provider{ctx: context.Background(), cfg: Config{HTTP: client, Now: func() time.Time { return now }, Record: func(record Record) { records = append(records, record) }}}
			op := newOperation("observed", caller, &router.Host{Name: "fixture", Base: "http://fixture.invalid", Wire: router.WireOpenAICompatible}, "model", "", 16, base)
			op.cancel = func() {}
			p.runs.Add(1)
			p.run(t.Context(), op, openAI{}, wire.Request{Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "prompt"}}}}}, nil, nil)
			if len(records) != 1 || cost == nil || *cost != 14 {
				t.Fatal(records, cost)
			}
			facts := records[0].Timing
			wantEvicted := int64(0)
			if observation.Evicted {
				wantEvicted = 1
			}
			if facts.Restore != observation.Restore || facts.RestoreCause != observation.Cause || *facts.CacheEvictions != wantEvicted || *facts.PrefillMS != 14 || *facts.DecodeMS != 20 || *facts.Engine.PromptMS != 999 {
				t.Fatal(facts)
			}
			*cost = 0
			if *facts.PrefillMS != 14 || report(ExecutionObservation{"miss", "request_failed", true}) != nil || facts.RestoreCause != observation.Cause {
				t.Fatal("copied or late observation changed final record")
			}
		})
	}
	if ReportExecutionObservation(context.Background(), ExecutionObservation{"hit", "resident", false}) != nil {
		t.Fatal("missing operation invented a measurement")
	}
	var clock requestClock
	if clock.observeExecution(ExecutionObservation{"hit", "untrusted text", true}) != nil || clock.facts.Restore != "" || clock.facts.CacheEvictions != nil {
		t.Fatal("unbounded facts admitted")
	}
}

func (timingAdapter) unsupported(wire.Request) string { return "" }
func (a timingAdapter) run(ctx context.Context, _ *http.Client, _ *router.Host, _ string, _ wire.Request, _ map[string][]byte, _ map[string]string, s *stream) wire.Reply {
	return a(ctx, s)
}

func TestRequestTimingFactsUseOneClockAndKeepEngineClaimsSeparate(t *testing.T) {
	// Explicit producer facts exercise classifications without claiming that the
	// current model host observes cold/warm origin or Layer 3 cache restoration.
	for _, observed := range []struct{ load, source, restore string }{
		{"cold", "disk", "miss"}, {"warm", "adopted", "hit"}, {"loaded", "lent", "refused"}, {"", "", ""},
	} {
		t.Run(observed.load+observed.restore, func(t *testing.T) {
			base := time.Now()
			now := base.Add(7 * time.Millisecond)
			var recorded Record
			load, restore, size := int64(11), int64(5), int64(4096)
			p := &Provider{ctx: context.Background(), cfg: Config{Now: func() time.Time { return now }, Record: func(r Record) { recorded = r },
				WaitExecution: func(context.Context, *router.Host, string) ExecutionPreparation {
					now = base.Add(23 * time.Millisecond)
					return ExecutionPreparation{Ready: true, Timing: PreparationTiming{LoadMS: &load, RestoreMS: &restore, LoadBytes: &size,
						Load: observed.load, LoadSource: observed.source, Restore: observed.restore}}
				}}}
			op := newOperation("timed", caller, &router.Host{Name: "fixture", Wire: router.WireOpenAICompatible}, "model", "", 16, base)
			op.cancel = func() {}
			p.runs.Add(1)
			p.run(context.Background(), op, timingAdapter(func(ctx context.Context, s *stream) wire.Reply {
				generationSent(ctx)
				// Heartbeats and usage are not a first generated token.
				now = base.Add(30 * time.Millisecond)
				s.usage(wire.Usage{Input: 100})
				now = base.Add(37 * time.Millisecond)
				s.part(0, wire.Part{Kind: wire.PartKindText, Text: "SECRET RESPONSE"})
				promptMS, decodeMS := 999.0, 777.0
				promptN, cacheN := int64(60), int64(40)
				s.engine(&engineTimings{PromptMS: &promptMS, DecodeMS: &decodeMS, PromptN: &promptN, CacheN: &cacheN})
				now = base.Add(57 * time.Millisecond)
				return wire.Reply{Outcome: wire.ReplyOutcomeCompleted, Usage: wire.Usage{Input: 100, Output: 3}}
			}), wire.Request{Messages: []wire.Message{{Parts: []wire.Part{{Text: "SECRET PROMPT"}}}}}, nil, nil)
			facts := recorded.Timing
			if facts == nil {
				t.Fatal("missing operation timing")
			}
			for name, pair := range map[string][2]int64{"queue": {*facts.QueueMS, 7}, "load": {*facts.LoadMS, 11}, "restore": {*facts.RestoreMS, 5}, "prefill": {*facts.PrefillMS, 14}, "decode": {*facts.DecodeMS, 20}, "prefill tokens": {*facts.PrefillTokens, 60}, "reuse": {*facts.ReusedTokens, 40}} {
				if pair[0] != pair[1] {
					t.Fatalf("%s: %v", name, pair)
				}
			}
			if facts.Load != observed.load || facts.LoadSource != observed.source || facts.Restore != observed.restore || *facts.Engine.PromptMS != 999 || *facts.Engine.DecodeMS != 777 {
				t.Fatalf("producer facts: %+v", facts)
			}
			raw, _ := json.Marshal(recorded)
			if strings.Contains(string(raw), "SECRET") {
				t.Fatal("content entered operation record")
			}
		})
	}
}

func TestRequestTimingMissingTokenAndPreparationStayAbsent(t *testing.T) {
	base := time.Now()
	var clock requestClock
	clock.begin(base, base)
	clock.send(base)
	clock.finish(base.Add(time.Second))
	if clock.facts.QueueMS == nil || *clock.facts.QueueMS != 0 || clock.facts.PrefillMS != nil || clock.facts.DecodeMS != nil || clock.facts.LoadMS != nil || clock.facts.RestoreMS != nil || clock.facts.Load != "" {
		t.Fatalf("invented observation: %+v", clock.facts)
	}
	clock.finish(base.Add(2 * time.Second))
	if clock.facts.DecodeMS != nil {
		t.Fatal("second finalization invented decode")
	}
}

func TestRequestTimingOpenAIParserReportsEngineClaims(t *testing.T) {
	data := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"timings\":{\"prompt_n\":60,\"cache_n\":40,\"prompt_ms\":999.25,\"predicted_ms\":777.5}}\n\ndata: [DONE]\n\n"
	client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	var clock requestClock
	r := runChat3OpenAI(context.Background(), client, &router.Host{Base: "http://fixture.invalid"}, "model", wire.Request{}, nil, nil, nil, nil,
		chat3Sink{Engine: clock.engine})
	if r.Err != nil || r.Cause != "" {
		t.Fatalf("parser: %+v", r)
	}
	if clock.facts.PrefillMS != nil || clock.facts.DecodeMS != nil || *clock.facts.Engine.PromptMS != 999.25 || *clock.facts.Engine.DecodeMS != 777.5 || *clock.facts.PrefillTokens != 60 || *clock.facts.ReusedTokens != 40 {
		t.Fatalf("claims became clock facts: %+v", clock.facts)
	}
}

func TestChat3TimingProjectsTheOperationRecord(t *testing.T) {
	base := time.Now()
	load := int64(17)
	f := setup(t, router.WireOpenAICompatible, func(cfg *Config) {
		cfg.Now = func() time.Time { return base }
		cfg.WaitExecution = func(context.Context, *router.Host, string) ExecutionPreparation {
			return ExecutionPreparation{Ready: true, Timing: PreparationTiming{LoadMS: &load, Load: "warm", LoadSource: "adopted"}}
		}
	})
	client := wire.NewChatV3Client(&wire.ChatV3Dispatcher{Handler: chat3BoundFixture{f.provider, caller, context.Background()}})
	a, err := client.Start(chat3HostedRequest(t, client))
	if err != nil || a.Outcome != wire.ChatStartOutcomeAccepted {
		t.Fatalf("start: %+v %v", a, err)
	}
	<-f.provider.visible(caller, a.Request).finished
	page, err := client.Observe(a.Request, 0, 256, 65536, 0)
	if err != nil || !page.AtEnd {
		t.Fatalf("page: %+v %v", page, err)
	}
	end := page.Events[len(page.Events)-1].End
	if end == nil || end.Timing == nil || end.ServedBy == nil || end.ServedBy.Place != wire.ServedPlaceHosted {
		t.Fatalf("terminal: %+v", end)
	}
	for _, record := range f.waitLogged(t, 1) {
		if record.Operation != a.Request || record.Timing == nil {
			continue
		}
		clock := requestClock{facts: *record.Timing}
		if !reflect.DeepEqual(end.Timing, clock.chatTiming()) {
			t.Fatalf("result and record differ: %+v %+v", end.Timing, record.Timing)
		}
		if *end.Timing.QueueMs != 0 || *end.Timing.LoadMs != 17 || *end.Timing.PrefillMs != 0 || *end.Timing.DecodeMs != 0 || end.Timing.RestoreMs != nil || record.DelegatedTiming != nil {
			t.Fatalf("projection presence: %+v", end.Timing)
		}
		return
	}
	t.Fatal("operation record missing")
}

func TestOptionalEngineClaimsNeverFailPrimaryCompletion(t *testing.T) {
	f64 := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name, diagnostic string
		want             engineTimings
	}{
		{"absent", "", engineTimings{}},
		{"null", "null", engineTimings{}},
		{"unknown_shape", `"future diagnostic"`, engineTimings{}},
		{"array", "[]", engineTimings{}},
		{"null_fields", `{"prompt_ms":null,"predicted_ms":null,"prompt_n":null,"cache_n":null}`, engineTimings{}},
		{"unknown_fields", `{"future":{"state":"unknown"}}`, engineTimings{}},
		{"mixed", `{"prompt_ms":"unknown","predicted_ms":777.5,"prompt_n":1.5,"cache_n":40}`, engineTimings{DecodeMS: f64(777.5), CacheN: i64(40)}},
		{"negative", `{"prompt_ms":-1,"predicted_ms":-2,"prompt_n":-3,"cache_n":-4}`, engineTimings{}},
		{"overflow", `{"prompt_ms":1e309,"predicted_ms":1e999,"prompt_n":9223372036854775808,"cache_n":1e100}`, engineTimings{}},
		{"underflow", `{"prompt_ms":1e-999,"predicted_ms":0,"prompt_n":1e-999,"cache_n":0}`, engineTimings{DecodeMS: f64(0), CacheN: i64(0)}},
		{"wrong_types", `{"prompt_ms":{},"predicted_ms":true,"prompt_n":"60","cache_n":[]}`, engineTimings{}},
		{"zero", `{"prompt_ms":0,"predicted_ms":0,"prompt_n":0,"cache_n":0}`, engineTimings{PromptMS: f64(0), DecodeMS: f64(0), PromptN: i64(0), CacheN: i64(0)}},
		{"valid", `{"prompt_ms":999.25,"predicted_ms":777.5,"prompt_n":60,"cache_n":40}`, engineTimings{PromptMS: f64(999.25), DecodeMS: f64(777.5), PromptN: i64(60), CacheN: i64(40)}},
	} {
		for _, chat3 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chat3=%t", tc.name, chat3), func(t *testing.T) {
				base := time.Now()
				now := base.Add(7 * time.Millisecond)
				host := &router.Host{Base: "http://fixture.invalid", Wire: router.WireOpenAICompatible}
				op := newOperation("timing", caller, host, "model", "", 16, base)
				clock := &op.timing
				clock.begin(base, now)
				ctx := context.WithValue(context.Background(), generationSendKey{}, func() { clock.send(now) })
				diagnostic := ""
				if tc.diagnostic != "" {
					diagnostic = `,"timings":` + tc.diagnostic
				}
				data := `data: {"choices":[{"index":0,"delta":{"content":"completed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":3}` + diagnostic + "}\n\ndata: [DONE]\n\n"
				client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
					now = base.Add(20 * time.Millisecond)
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
				})}
				if chat3 {
					var text string
					r := runChat3OpenAI(ctx, client, host, "model", wire.Request{}, nil, nil, nil, nil, chat3Sink{
						Part: func(p wire.ChatPart) error { text += p.Text; return nil }, Token: func() { clock.token(now) }, Engine: clock.engine})
					if r.Err != nil || r.Cause != "" || text != "completed" || r.Usage.Input == nil || *r.Usage.Input != 100 || r.Usage.Output == nil || *r.Usage.Output != 3 {
						t.Fatalf("optional diagnostic failed completion: %+v text=%q", r, text)
					}
				} else {
					r := (openAI{}).run(ctx, client, host, "model", wire.Request{}, nil, nil, &stream{op: op, now: func() time.Time { return now }})
					if r.Outcome != wire.ReplyOutcomeCompleted || r.Usage.Input != 100 || r.Usage.Output != 3 {
						t.Fatalf("optional diagnostic failed completion: %+v", r)
					}
				}
				clock.finish(base.Add(55 * time.Millisecond))
				if !reflect.DeepEqual(clock.facts.Engine, EngineTiming{PromptMS: tc.want.PromptMS, DecodeMS: tc.want.DecodeMS}) || !reflect.DeepEqual(clock.facts.PrefillTokens, tc.want.PromptN) || !reflect.DeepEqual(clock.facts.ReusedTokens, tc.want.CacheN) {
					t.Fatalf("claims: %+v want %+v", clock.facts, tc.want)
				}
				if *clock.facts.QueueMS != 7 || *clock.facts.PrefillMS != 13 || *clock.facts.DecodeMS != 35 {
					t.Fatalf("diagnostics changed clock facts: %+v", clock.facts)
				}
			})
		}
	}
}

func TestOptionalEngineClaimsKeepPrimaryUsageValidationStrict(t *testing.T) {
	data := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"text\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":\"unknown\"},\"timings\":{\"prompt_ms\":1}}\n\ndata: [DONE]\n\n"
	client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}
	host := &router.Host{Base: "http://fixture.invalid", Wire: router.WireOpenAICompatible}
	op := newOperation("strict", caller, host, "model", "", 16, time.Now())
	legacy := (openAI{}).run(context.Background(), client, host, "model", wire.Request{}, nil, nil, &stream{op: op, now: time.Now})
	modern := runChat3OpenAI(context.Background(), client, host, "model", wire.Request{}, nil, nil, nil, nil, chat3Sink{})
	if legacy.Outcome != wire.ReplyOutcomeUnavailable || modern.Cause != "upstream:malformed" {
		t.Fatalf("primary usage validation relaxed: %+v %+v", legacy, modern)
	}
}
