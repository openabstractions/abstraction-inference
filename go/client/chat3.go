package client

import (
	"context"
	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	rights "github.com/openabstractions/abstraction-rights/go/abstraction/rights/api"
	"math"
	"sort"
	"time"
)

// ChatV3 calls chat@3 once per explicit Start. The caller persists identity before sending.
type ChatV3 struct{ transport listen.FrameClient }
type RequestIdentity = acceptance.RequestIdentity
type HistoryWindow = acceptance.HistoryWindow
type RefusalDetail = rights.RefusalDetail
type RefusalSource = rights.RefusalSource
type RefusalDecision = rights.RefusalDecision
type RefusalAsk = rights.RefusalAsk

const (
	RefusalSourceRights       = rights.RefusalSourceRights
	RefusalSourceCredentials  = rights.RefusalSourceCredentials
	RefusalSourceLicense      = rights.RefusalSourceLicense
	RefusalSourceTerms        = rights.RefusalSourceTerms
	RefusalSourceIdentity     = rights.RefusalSourceIdentity
	RefusalSourceOS           = rights.RefusalSourceOS
	RefusalDecisionDenied     = rights.RefusalDecisionDenied
	RefusalDecisionNotGranted = rights.RefusalDecisionNotGranted
	RefusalDecisionPersonOnly = rights.RefusalDecisionPersonOnly
	RefusalAskAvailable       = rights.RefusalAskAvailable
	RefusalAskNone            = rights.RefusalAskNone
)

func RefusalSourceValues() []RefusalSource                  { return rights.RefusalSourceValues() }
func RefusalDecisionValues() []RefusalDecision              { return rights.RefusalDecisionValues() }
func RefusalAskValues() []RefusalAsk                        { return rights.RefusalAskValues() }
func ParseRefusalDecision(s string) (RefusalDecision, bool) { return rights.ParseRefusalDecision(s) }
func ParseRefusalAsk(s string) (RefusalAsk, bool)           { return rights.ParseRefusalAsk(s) }

type RequestV3 = wire.RequestV3
type ChatAdmission = wire.ChatAdmission
type ChatPage = wire.ChatPage
type ChatEvent = wire.ChatEvent
type ChatEventKind = wire.ChatEventKind
type ChatPart = wire.ChatPart
type ChatPartKind = wire.ChatPartKind
type ChatResult = wire.ChatResult
type ChatTiming = wire.ChatTiming
type ChatLoad = wire.ChatLoad
type ChatLoadSource = wire.ChatLoadSource

const (
	ChatLoadCold          = wire.ChatLoadCold
	ChatLoadWarm          = wire.ChatLoadWarm
	ChatLoadLoaded        = wire.ChatLoadLoaded
	ChatLoadSourceDisk    = wire.ChatLoadSourceDisk
	ChatLoadSourceAdopted = wire.ChatLoadSourceAdopted
	ChatLoadSourceLent    = wire.ChatLoadSourceLent
)

func ChatLoadValues() []ChatLoad             { return wire.ChatLoadValues() }
func ChatLoadSourceValues() []ChatLoadSource { return wire.ChatLoadSourceValues() }

type ChatResultOutcome = wire.ChatResultOutcome
type ChatStartOutcome = wire.ChatStartOutcome
type ChatProgress = wire.ChatProgress
type ChatProgressPhase = wire.ChatProgressPhase
type ChatUsage = wire.ChatUsage
type ChatBlockEnd = wire.ChatBlockEnd
type ChatHistoryRead = wire.ChatHistoryRead
type ChatHistoryOutcome = wire.ChatHistoryOutcome
type ChatCancellation = wire.ChatCancellation
type ChatTarget = wire.ChatTarget
type ChatContent = wire.ChatContent
type ChatMessage = wire.ChatMessage
type ChatInputPart = wire.ChatInputPart
type ChatOptions = wire.ChatOptions
type ChatTool = wire.ChatTool
type ChatToolChoice = wire.ChatToolChoice
type ChatToolChoiceKind = wire.ChatToolChoiceKind
type ChatGuarantee = wire.ChatGuarantee
type ChatReplay = wire.ChatReplay
type ReasoningSeal = wire.ReasoningSeal
type SealOmission = wire.SealOmission
type FinishReason = wire.FinishReason
type ServedBy = wire.ServedBy
type ServedPlace = wire.ServedPlace

func NewChatV3(endpoint string) *ChatV3 {
	return NewChatV3WithTransport(listen.FrameClient{Endpoint: endpoint})
}
func NewChatV3WithTransport(t listen.FrameClient) *ChatV3 {
	return &ChatV3{t.WithDefaults(callMargin, 1<<20)}
}
func (c *ChatV3) Start(ctx context.Context, r wire.RequestV3) (wire.ChatAdmission, error) {
	if e := ctx.Err(); e != nil {
		return wire.ChatAdmission{}, e
	}
	return wire.NewChatV3Client(c.transport.WithContext(ctx)).Start(r)
}
func (c *ChatV3) GetHistoryWindow(ctx context.Context) (wire.ChatHistoryRead, error) {
	if e := ctx.Err(); e != nil {
		return wire.ChatHistoryRead{}, e
	}
	return wire.NewChatV3Client(c.transport.WithContext(ctx)).GetHistoryWindow()
}
func (c *ChatV3) Cancel(ctx context.Context, id string) (wire.ChatCancellation, error) {
	if e := ctx.Err(); e != nil {
		return wire.ChatCancellation{}, e
	}
	return wire.NewChatV3Client(c.transport.WithContext(ctx)).Cancel(id)
}
func (c *ChatV3) Observe(ctx context.Context, id string, cursor, count, bytes, wait int64) (wire.ChatPage, error) {
	if e := ctx.Err(); e != nil {
		return wire.ChatPage{}, e
	}
	if cursor < 0 || count < 1 || count > 256 || bytes < 1 || bytes > 65536 || wait < 0 || wait > 30000 {
		return wire.ChatPage{}, ErrInconsistent
	}
	t := c.transport
	budget := time.Duration(wait) * time.Millisecond
	if t.Timeout > time.Duration(math.MaxInt64)-budget {
		return wire.ChatPage{}, ErrInconsistent
	}
	t.Timeout += budget
	p, e := wire.NewChatV3Client(t.WithContext(ctx)).Observe(id, cursor, count, bytes, wait)
	if e != nil {
		return p, e
	}
	if p.Outcome == wire.PageOutcomePage {
		n := int64(len(p.Events))
		if n > count || cursor > math.MaxInt64-n || p.Next != cursor+n {
			return p, ErrInconsistent
		}
		for i, v := range p.Events {
			if v.Sequence != cursor+int64(i) || (v.Kind == wire.ChatEventKindEnd && (i != len(p.Events)-1 || !p.AtEnd)) {
				return p, ErrInconsistent
			}
		}
		if p.AtEnd && n > 0 && p.Events[n-1].Kind != wire.ChatEventKindEnd {
			return p, ErrInconsistent
		}
	} else if len(p.Events) != 0 || p.AtEnd || (p.Outcome == wire.PageOutcomeGap && p.Next <= cursor) || (p.Outcome != wire.PageOutcomeGap && p.Next != cursor) {
		return p, ErrInconsistent
	}
	return p, nil
}

// ChatV3Reply contains ordered output blocks and the actual terminal observation.
// Text contains answer text only. Reasoning remains in its own typed blocks.
type ChatV3Reply struct {
	Blocks    []wire.ChatPart
	BlockEnds []wire.ChatBlockEnd
	Result    wire.ChatResult
	Text      string
}

// FoldV3 retains opaque seals even when public reasoning display text is discarded.
// RetainReasoning is an explicit display retention choice. No replay is constructed.
type FoldV3 struct {
	RetainReasoning bool
	Cursor          int64
	Progress        *wire.ChatProgress
	Usage           *wire.ChatUsage
	parts           map[int64]wire.ChatPart
	closed          map[int64]wire.ChatBlockEnd
	end             *wire.ChatResult
	partial         bool
}

func NewFoldV3(retainReasoning bool, cursor int64) FoldV3 {
	return FoldV3{RetainReasoning: retainReasoning, Cursor: cursor, partial: cursor > 0}
}

func (f *FoldV3) Add(e wire.ChatEvent) error {
	if f.end != nil || e.Sequence != f.Cursor {
		return ErrInconsistent
	}
	switch e.Kind {
	case wire.ChatEventKindProgress:
		if e.Progress == nil {
			return ErrInconsistent
		}
		v := *e.Progress
		f.Progress = &v
	case wire.ChatEventKindUsage:
		if e.Usage == nil {
			return ErrInconsistent
		}
		v := *e.Usage
		f.Usage = &v
	case wire.ChatEventKindPart:
		if e.Part == nil {
			return ErrInconsistent
		}
		d := *e.Part
		if _, ok := f.closed[d.Block]; ok {
			return ErrInconsistent
		}
		p, ok := f.parts[d.Block]
		if ok && (p.Kind != d.Kind || (d.CallID != "" && d.CallID != p.CallID) || (d.Name != "" && d.Name != p.Name)) {
			return ErrInconsistent
		}
		if !f.RetainReasoning && (d.Kind == wire.ChatPartKindReasoning || d.Kind == wire.ChatPartKindReasoningSummary) {
			d.Text = ""
		}
		if !ok {
			p = d
		} else {
			p.Text += d.Text
			p.Arguments += d.Arguments
			if d.Seal != nil {
				p.Seal = d.Seal
			}
		}
		if f.parts == nil {
			f.parts = map[int64]wire.ChatPart{}
		}
		f.parts[d.Block] = p
	case wire.ChatEventKindBlockEnd:
		if e.BlockEnd == nil {
			return ErrInconsistent
		}
		b := *e.BlockEnd
		if _, ok := f.parts[b.Block]; !ok && !f.partial {
			return ErrInconsistent
		}
		if _, ok := f.closed[b.Block]; ok {
			return ErrInconsistent
		}
		if f.closed == nil {
			f.closed = map[int64]wire.ChatBlockEnd{}
		}
		f.closed[b.Block] = b
	case wire.ChatEventKindEnd:
		if e.End == nil {
			return ErrInconsistent
		}
		v := *e.End
		f.end = &v
	default:
		return ErrInconsistent
	}
	f.Cursor++
	return nil
}
func (f *FoldV3) Reply() (ChatV3Reply, bool) {
	if f.end == nil {
		return ChatV3Reply{}, false
	}
	r := ChatV3Reply{Result: *f.end}
	if r.Result.Outcome == wire.ChatResultOutcomeCompleted && r.Result.FinishReason != nil && !r.Result.FinishReason.Known() {
		other := wire.FinishReasonOther
		r.Result.FinishReason = &other
	}
	keys := make([]int64, 0, len(f.parts))
	for k := range f.parts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		p := f.parts[k]
		r.Blocks = append(r.Blocks, p)
		if p.Kind == wire.ChatPartKindText {
			r.Text += p.Text
		}
	}
	keys = keys[:0]
	for k := range f.closed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		r.BlockEnds = append(r.BlockEnds, f.closed[k])
	}
	return r, true
}

// ChatV3Session preserves its admission and cursor across wait failures and gaps.
// Resume reconstructs output from the supplied cursor; retain the previous Fold
// when a complete local transcript is needed. Cancel is always explicit.
type ChatV3Observation interface {
	Observe(ctx context.Context, request string, cursor, maxDeltas, maxBytes, waitMs int64) (wire.ChatPage, error)
	Cancel(ctx context.Context, request string) (wire.ChatCancellation, error)
}
type ChatV3Session struct {
	Chat      ChatV3Observation
	Admission wire.ChatAdmission
	Request   string
	Cursor    int64
	Fold      FoldV3
}

func (c *ChatV3) Resume(id string, cursor int64, retainReasoning bool) *ChatV3Session {
	return &ChatV3Session{Chat: c, Request: id, Cursor: cursor, Fold: NewFoldV3(retainReasoning, cursor)}
}
func (c *ChatV3) Open(ctx context.Context, r wire.RequestV3, retainReasoning bool) (*ChatV3Session, error) {
	a, e := c.Start(ctx, r)
	s := c.Resume(a.Request, 0, retainReasoning)
	s.Admission = a
	return s, e
}
func (s *ChatV3Session) Next(ctx context.Context) (wire.ChatPage, error) {
	p, e := s.Chat.Observe(ctx, s.Request, s.Cursor, PageDeltas, PageBytes, PageWait.Milliseconds())
	if e != nil {
		return p, e
	}
	if p.Outcome != wire.PageOutcomePage {
		return p, &PageRefusal{Outcome: p.Outcome}
	}
	next := s.Fold
	next.parts = map[int64]wire.ChatPart{}
	next.closed = map[int64]wire.ChatBlockEnd{}
	for k, v := range s.Fold.parts {
		next.parts[k] = v
	}
	for k, v := range s.Fold.closed {
		next.closed[k] = v
	}
	for _, v := range p.Events {
		if e = next.Add(v); e != nil {
			return p, e
		}
	}
	s.Fold = next
	s.Cursor = p.Next
	return p, nil
}
func (s *ChatV3Session) Cancel(ctx context.Context) (wire.ChatCancellation, error) {
	return s.Chat.Cancel(ctx, s.Request)
}

// Complete drains this existing handle. Errors leave the session resumable.
func (s *ChatV3Session) Complete(ctx context.Context) (ChatV3Reply, error) {
	for {
		if r, ok := s.Fold.Reply(); ok {
			return r, nil
		}
		p, e := s.Next(ctx)
		if e != nil {
			return ChatV3Reply{}, e
		}
		if p.AtEnd {
			if r, ok := s.Fold.Reply(); ok {
				return r, nil
			}
			return ChatV3Reply{}, ErrInconsistent
		}
	}
}

const (
	ChatEventKindProgress = wire.ChatEventKindProgress
	ChatEventKindPart     = wire.ChatEventKindPart
	ChatEventKindBlockEnd = wire.ChatEventKindBlockEnd
	ChatEventKindUsage    = wire.ChatEventKindUsage
	ChatEventKindEnd      = wire.ChatEventKindEnd
)

func ChatEventKindValues() []ChatEventKind                 { return wire.ChatEventKindValues() }
func ParseChatEventKind(word string) (ChatEventKind, bool) { return wire.ParseChatEventKind(word) }

const (
	ChatPartKindText             = wire.ChatPartKindText
	ChatPartKindReasoning        = wire.ChatPartKindReasoning
	ChatPartKindReasoningSummary = wire.ChatPartKindReasoningSummary
	ChatPartKindToolCall         = wire.ChatPartKindToolCall
	ChatPartKindModelRefusal     = wire.ChatPartKindModelRefusal
)

func ChatPartKindValues() []ChatPartKind                 { return wire.ChatPartKindValues() }
func ParseChatPartKind(word string) (ChatPartKind, bool) { return wire.ParseChatPartKind(word) }

const (
	ChatResultOutcomeCompleted = wire.ChatResultOutcomeCompleted
	ChatResultOutcomeFailed    = wire.ChatResultOutcomeFailed
	ChatResultOutcomeCancelled = wire.ChatResultOutcomeCancelled
)

func ChatResultOutcomeValues() []ChatResultOutcome { return wire.ChatResultOutcomeValues() }
func ParseChatResultOutcome(word string) (ChatResultOutcome, bool) {
	return wire.ParseChatResultOutcome(word)
}

const (
	ChatStartOutcomeAccepted       = wire.ChatStartOutcomeAccepted
	ChatStartOutcomeNotPermitted   = wire.ChatStartOutcomeNotPermitted
	ChatStartOutcomeBudgetExceeded = wire.ChatStartOutcomeBudgetExceeded
	ChatStartOutcomeUnsupported    = wire.ChatStartOutcomeUnsupported
	ChatStartOutcomeNoHost         = wire.ChatStartOutcomeNoHost
	ChatStartOutcomeUnavailable    = wire.ChatStartOutcomeUnavailable
	ChatStartOutcomeInvalid        = wire.ChatStartOutcomeInvalid
	ChatStartOutcomeForbidden      = wire.ChatStartOutcomeForbidden
	ChatStartOutcomeExhausted      = wire.ChatStartOutcomeExhausted
	ChatStartOutcomeConflict       = wire.ChatStartOutcomeConflict
	ChatStartOutcomeUnknown        = wire.ChatStartOutcomeUnknown
)

func ChatStartOutcomeValues() []ChatStartOutcome { return wire.ChatStartOutcomeValues() }
func ParseChatStartOutcome(word string) (ChatStartOutcome, bool) {
	return wire.ParseChatStartOutcome(word)
}

const (
	ChatProgressPhaseQueued  = wire.ChatProgressPhaseQueued
	ChatProgressPhaseLoading = wire.ChatProgressPhaseLoading
	ChatProgressPhaseWarming = wire.ChatProgressPhaseWarming
)

func ChatProgressPhaseValues() []ChatProgressPhase { return wire.ChatProgressPhaseValues() }

const (
	ChatHistoryOutcomeAvailable   = wire.ChatHistoryOutcomeAvailable
	ChatHistoryOutcomeForbidden   = wire.ChatHistoryOutcomeForbidden
	ChatHistoryOutcomeInvalid     = wire.ChatHistoryOutcomeInvalid
	ChatHistoryOutcomeUnavailable = wire.ChatHistoryOutcomeUnavailable
)

func ChatHistoryOutcomeValues() []ChatHistoryOutcome { return wire.ChatHistoryOutcomeValues() }
func ParseChatHistoryOutcome(word string) (ChatHistoryOutcome, bool) {
	return wire.ParseChatHistoryOutcome(word)
}

const (
	ChatToolChoiceKindAuto     = wire.ChatToolChoiceKindAuto
	ChatToolChoiceKindNone     = wire.ChatToolChoiceKindNone
	ChatToolChoiceKindRequired = wire.ChatToolChoiceKindRequired
	ChatToolChoiceKindNamed    = wire.ChatToolChoiceKindNamed
)

func ChatToolChoiceKindValues() []ChatToolChoiceKind { return wire.ChatToolChoiceKindValues() }
func ParseChatToolChoiceKind(word string) (ChatToolChoiceKind, bool) {
	return wire.ParseChatToolChoiceKind(word)
}

const (
	ChatGuaranteeLocalOnly     = wire.ChatGuaranteeLocalOnly
	ChatGuaranteeHostedAllowed = wire.ChatGuaranteeHostedAllowed
	ChatGuaranteeNoRetention   = wire.ChatGuaranteeNoRetention
	ChatGuaranteeFreeCapacity  = wire.ChatGuaranteeFreeCapacity
)

func ChatGuaranteeValues() []ChatGuarantee { return wire.ChatGuaranteeValues() }

const (
	SealOmissionEncodedSize        = wire.SealOmissionEncodedSize
	SealOmissionIncompatibleOrigin = wire.SealOmissionIncompatibleOrigin
)

func SealOmissionValues() []SealOmission { return wire.SealOmissionValues() }

const (
	FinishReasonStop              = wire.FinishReasonStop
	FinishReasonLength            = wire.FinishReasonLength
	FinishReasonToolCalls         = wire.FinishReasonToolCalls
	FinishReasonContentFilter     = wire.FinishReasonContentFilter
	FinishReasonModelRefusal      = wire.FinishReasonModelRefusal
	FinishReasonContextWindow     = wire.FinishReasonContextWindow
	FinishReasonMalformedToolCall = wire.FinishReasonMalformedToolCall
	FinishReasonPaused            = wire.FinishReasonPaused
	FinishReasonOther             = wire.FinishReasonOther
)

func FinishReasonValues() []FinishReason { return wire.FinishReasonValues() }

const (
	ServedPlaceLocal  = wire.ServedPlaceLocal
	ServedPlaceHosted = wire.ServedPlaceHosted
	ServedPlaceRemote = wire.ServedPlaceRemote
)

func ServedPlaceValues() []ServedPlace { return wire.ServedPlaceValues() }
