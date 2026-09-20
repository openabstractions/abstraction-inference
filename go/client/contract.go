package client

// The generated contract types this package's API reaches, re-exported so an
// application names them through this package and never imports the generated
// one. scripts/idiom_check.py refuses a reachable type this file leaves out.

import (
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type Part = wire.Part

type Message = wire.Message

type Tool = wire.Tool

type Temperature = wire.Temperature

type Options = wire.Options

type Request = wire.Request

type RequestGuarantee = wire.RequestGuarantee

const (
	RequestGuaranteeLocalOnly     = wire.RequestGuaranteeLocalOnly
	RequestGuaranteeHostedAllowed = wire.RequestGuaranteeHostedAllowed
)

// RequestGuaranteeValues returns every request placement choice in declaration order.
func RequestGuaranteeValues() []RequestGuarantee { return wire.RequestGuaranteeValues() }

type TranscriptionRequest = wire.TranscriptionRequest

type TranscriptSegment = wire.TranscriptSegment

type TranscriptionReply = wire.TranscriptionReply

type SpeechRequest = wire.SpeechRequest
type AudioChunk = wire.AudioChunk
type Delivery = wire.Delivery
type SpeechReply = wire.SpeechReply
type LiveRequest = wire.LiveRequest
type LiveInputResult = wire.LiveInputResult
type LiveTranscript = wire.LiveTranscript
type LiveReply = wire.LiveReply
type ImageRequest = wire.ImageRequest
type ImageProgress = wire.ImageProgress
type ImageResult = wire.ImageResult
type ImageReply = wire.ImageReply

type Usage = wire.Usage

type Cost = wire.Cost

type Reply = wire.Reply

type Delta = wire.Delta

type Admission = wire.Admission

type DeltaPage = wire.DeltaPage

type Cancellation = wire.Cancellation

type ServiceError = wire.ServiceError

type Role = wire.Role

type CeilingLimit = wire.CeilingLimit

type HostEntry = wire.HostEntry

type Spend = wire.Spend

type HostState = wire.HostState

type HostList = wire.HostList

type HostChange = wire.HostChange
type GatewayState = wire.GatewayState
type GatewayChange = wire.GatewayChange
type LocalKey = wire.LocalKey

type KeyList = wire.KeyList

type KeyIssued = wire.KeyIssued

type KeyRevoked = wire.KeyRevoked

type AuditEntry = wire.AuditEntry

type AuditPage = wire.AuditPage

type ListOutcome = wire.ListOutcome

const (
	ListOutcomePage        = wire.ListOutcomePage
	ListOutcomeInvalid     = wire.ListOutcomeInvalid
	ListOutcomeForbidden   = wire.ListOutcomeForbidden
	ListOutcomeUnavailable = wire.ListOutcomeUnavailable
)

// ListOutcomeValues returns every member of ListOutcome in declaration order, in a new slice.
func ListOutcomeValues() []ListOutcome { return wire.ListOutcomeValues() }

type EditOutcome = wire.EditOutcome

const (
	EditOutcomeApplied       = wire.EditOutcomeApplied
	EditOutcomeConflict      = wire.EditOutcomeConflict
	EditOutcomeUnknown       = wire.EditOutcomeUnknown
	EditOutcomeInvalid       = wire.EditOutcomeInvalid
	EditOutcomeNoSecureStore = wire.EditOutcomeNoSecureStore
	EditOutcomeForbidden     = wire.EditOutcomeForbidden
	EditOutcomeUnavailable   = wire.EditOutcomeUnavailable
)

// EditOutcomeValues returns every member of EditOutcome in declaration order, in a new slice.
func EditOutcomeValues() []EditOutcome { return wire.EditOutcomeValues() }

type KeyState = wire.KeyState

const (
	KeyStateActive  = wire.KeyStateActive
	KeyStateRevoked = wire.KeyStateRevoked
	KeyStateLost    = wire.KeyStateLost
)

// KeyStateValues returns every member of KeyState in declaration order, in a new slice.
func KeyStateValues() []KeyState { return wire.KeyStateValues() }

type AuditRoute = wire.AuditRoute

const (
	AuditRouteNative = wire.AuditRouteNative
	AuditRouteWindow = wire.AuditRouteWindow
	AuditRouteRemote = wire.AuditRouteRemote
)

// AuditRouteValues returns every member of AuditRoute in declaration order, in a new slice.
func AuditRouteValues() []AuditRoute { return wire.AuditRouteValues() }

type AuditOutcome = wire.AuditOutcome

const (
	AuditOutcomePage        = wire.AuditOutcomePage
	AuditOutcomeGap         = wire.AuditOutcomeGap
	AuditOutcomeInvalid     = wire.AuditOutcomeInvalid
	AuditOutcomeForbidden   = wire.AuditOutcomeForbidden
	AuditOutcomeUnavailable = wire.AuditOutcomeUnavailable
)

// AuditOutcomeValues returns every member of AuditOutcome in declaration order, in a new slice.
func AuditOutcomeValues() []AuditOutcome { return wire.AuditOutcomeValues() }

const (
	RoleSystem    = wire.RoleSystem
	RoleUser      = wire.RoleUser
	RoleAssistant = wire.RoleAssistant
	RoleTool      = wire.RoleTool
)

// RoleValues returns every member of Role in declaration order, in a new slice.
func RoleValues() []Role { return wire.RoleValues() }

type PartKind = wire.PartKind

const (
	PartKindText       = wire.PartKindText
	PartKindImage      = wire.PartKindImage
	PartKindToolCall   = wire.PartKindToolCall
	PartKindToolResult = wire.PartKindToolResult
)

// PartKindValues returns every member of PartKind in declaration order, in a new slice.
func PartKindValues() []PartKind { return wire.PartKindValues() }

type ReplyOutcome = wire.ReplyOutcome

const (
	ReplyOutcomeCompleted          = wire.ReplyOutcomeCompleted
	ReplyOutcomeRefused            = wire.ReplyOutcomeRefused
	ReplyOutcomeNotPermitted       = wire.ReplyOutcomeNotPermitted
	ReplyOutcomeBudgetExceeded     = wire.ReplyOutcomeBudgetExceeded
	ReplyOutcomeUnsupportedFeature = wire.ReplyOutcomeUnsupportedFeature
	ReplyOutcomeNoHost             = wire.ReplyOutcomeNoHost
	ReplyOutcomeCancelled          = wire.ReplyOutcomeCancelled
	ReplyOutcomeUnavailable        = wire.ReplyOutcomeUnavailable
	ReplyOutcomeInvalid            = wire.ReplyOutcomeInvalid
	ReplyOutcomeForbidden          = wire.ReplyOutcomeForbidden
	ReplyOutcomeExhausted          = wire.ReplyOutcomeExhausted
)

// ReplyOutcomeValues returns every member of ReplyOutcome in declaration order, in a new slice.
func ReplyOutcomeValues() []ReplyOutcome { return wire.ReplyOutcomeValues() }

type StopReason = wire.StopReason

const (
	StopReasonNoStop        = wire.StopReasonNoStop
	StopReasonEnd           = wire.StopReasonEnd
	StopReasonMaxOutput     = wire.StopReasonMaxOutput
	StopReasonStopSequence  = wire.StopReasonStopSequence
	StopReasonToolCalls     = wire.StopReasonToolCalls
	StopReasonContentFilter = wire.StopReasonContentFilter
	StopReasonOther         = wire.StopReasonOther
)

// StopReasonValues returns every member of StopReason in declaration order, in a new slice.
func StopReasonValues() []StopReason { return wire.StopReasonValues() }

type DeltaKind = wire.DeltaKind

const (
	DeltaKindPart          = wire.DeltaKindPart
	DeltaKindUsage         = wire.DeltaKindUsage
	DeltaKindSegment       = wire.DeltaKindSegment
	DeltaKindAudio         = wire.DeltaKindAudio
	DeltaKindTranscript    = wire.DeltaKindTranscript
	DeltaKindImageProgress = wire.DeltaKindImageProgress
	DeltaKindImageResult   = wire.DeltaKindImageResult
	DeltaKindEnd           = wire.DeltaKindEnd
)

// DeltaKindValues returns every member of DeltaKind in declaration order, in a new slice.
func DeltaKindValues() []DeltaKind { return wire.DeltaKindValues() }

type TranscriptUnitKind = wire.TranscriptUnitKind

const (
	TranscriptUnitKindSegment = wire.TranscriptUnitKindSegment
	TranscriptUnitKindWord    = wire.TranscriptUnitKindWord
)

func TranscriptUnitKindValues() []TranscriptUnitKind { return wire.TranscriptUnitKindValues() }

type TimestampMode = wire.TimestampMode

const (
	TimestampModeNone           = wire.TimestampModeNone
	TimestampModeSegment        = wire.TimestampModeSegment
	TimestampModeWord           = wire.TimestampModeWord
	TimestampModeSegmentAndWord = wire.TimestampModeSegmentAndWord
)

func TimestampModeValues() []TimestampMode { return wire.TimestampModeValues() }

type SpeechFormat = wire.SpeechFormat

const (
	SpeechFormatMp3  = wire.SpeechFormatMp3
	SpeechFormatOpus = wire.SpeechFormatOpus
	SpeechFormatAac  = wire.SpeechFormatAac
	SpeechFormatFlac = wire.SpeechFormatFlac
	SpeechFormatWav  = wire.SpeechFormatWav
	SpeechFormatPcm  = wire.SpeechFormatPcm
)

func SpeechFormatValues() []SpeechFormat { return wire.SpeechFormatValues() }

type LiveFormat = wire.LiveFormat

const LiveFormatPcm1624000 = wire.LiveFormatPcm1624000

func LiveFormatValues() []LiveFormat { return wire.LiveFormatValues() }

type LiveInputOutcome = wire.LiveInputOutcome

const (
	LiveInputOutcomeAccepted    = wire.LiveInputOutcomeAccepted
	LiveInputOutcomeDuplicate   = wire.LiveInputOutcomeDuplicate
	LiveInputOutcomeOutOfOrder  = wire.LiveInputOutcomeOutOfOrder
	LiveInputOutcomeClosed      = wire.LiveInputOutcomeClosed
	LiveInputOutcomeInvalid     = wire.LiveInputOutcomeInvalid
	LiveInputOutcomeUnknown     = wire.LiveInputOutcomeUnknown
	LiveInputOutcomeForbidden   = wire.LiveInputOutcomeForbidden
	LiveInputOutcomeUnavailable = wire.LiveInputOutcomeUnavailable
	LiveInputOutcomeExhausted   = wire.LiveInputOutcomeExhausted
)

func LiveInputOutcomeValues() []LiveInputOutcome { return wire.LiveInputOutcomeValues() }

type ImageMode = wire.ImageMode

const (
	ImageModeGenerate = wire.ImageModeGenerate
	ImageModeEdit     = wire.ImageModeEdit
)

func ImageModeValues() []ImageMode { return wire.ImageModeValues() }

type StartOutcome = wire.StartOutcome

const (
	StartOutcomeAccepted           = wire.StartOutcomeAccepted
	StartOutcomeNotPermitted       = wire.StartOutcomeNotPermitted
	StartOutcomeBudgetExceeded     = wire.StartOutcomeBudgetExceeded
	StartOutcomeUnsupportedFeature = wire.StartOutcomeUnsupportedFeature
	StartOutcomeNoHost             = wire.StartOutcomeNoHost
	StartOutcomeUnavailable        = wire.StartOutcomeUnavailable
	StartOutcomeInvalid            = wire.StartOutcomeInvalid
	StartOutcomeForbidden          = wire.StartOutcomeForbidden
	StartOutcomeExhausted          = wire.StartOutcomeExhausted
)

// StartOutcomeValues returns every member of StartOutcome in declaration order, in a new slice.
func StartOutcomeValues() []StartOutcome { return wire.StartOutcomeValues() }

type PageOutcome = wire.PageOutcome

const (
	PageOutcomePage        = wire.PageOutcomePage
	PageOutcomeGap         = wire.PageOutcomeGap
	PageOutcomeUnknown     = wire.PageOutcomeUnknown
	PageOutcomeInvalid     = wire.PageOutcomeInvalid
	PageOutcomeForbidden   = wire.PageOutcomeForbidden
	PageOutcomeUnavailable = wire.PageOutcomeUnavailable
)

// PageOutcomeValues returns every member of PageOutcome in declaration order, in a new slice.
func PageOutcomeValues() []PageOutcome { return wire.PageOutcomeValues() }

type CancelOutcome = wire.CancelOutcome

const (
	CancelOutcomeCancelled   = wire.CancelOutcomeCancelled
	CancelOutcomeEnded       = wire.CancelOutcomeEnded
	CancelOutcomeUnknown     = wire.CancelOutcomeUnknown
	CancelOutcomeInvalid     = wire.CancelOutcomeInvalid
	CancelOutcomeForbidden   = wire.CancelOutcomeForbidden
	CancelOutcomeUnavailable = wire.CancelOutcomeUnavailable
)

// CancelOutcomeValues returns every member of CancelOutcome in declaration order, in a new slice.
func CancelOutcomeValues() []CancelOutcome { return wire.CancelOutcomeValues() }

type ServiceErrorCode = wire.ServiceErrorCode

const (
	ServiceErrorCodeHandlerError   = wire.ServiceErrorCodeHandlerError
	ServiceErrorCodeInvalidResult  = wire.ServiceErrorCodeInvalidResult
	ServiceErrorCodeUnknownVersion = wire.ServiceErrorCodeUnknownVersion
	ServiceErrorCodeUnknownService = wire.ServiceErrorCodeUnknownService
	ServiceErrorCodeUnknownMethod  = wire.ServiceErrorCodeUnknownMethod
	ServiceErrorCodeWrongMode      = wire.ServiceErrorCodeWrongMode
)

// ServiceErrorCodeValues returns every member of ServiceErrorCode in declaration order, in a new slice.
func ServiceErrorCodeValues() []ServiceErrorCode { return wire.ServiceErrorCodeValues() }
