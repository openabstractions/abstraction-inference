package client

import (
	"context"
	"iter"

	"github.com/openabstractions/abstraction-identity/listen"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type Target = wire.Target
type RequestContent = wire.RequestContent
type RequestV2 = wire.RequestV2
type AdmissionV2 = wire.AdmissionV2
type SelectionReason = wire.SelectionReason
type CandidatePlacement = wire.CandidatePlacement
type SelectionReadOutcome = wire.SelectionReadOutcome
type SelectionChange = wire.SelectionChange
type SelectionCandidateV2 = wire.SelectionCandidateV2
type SelectionPolicyV2 = wire.SelectionPolicyV2
type SelectionReadV2 = wire.SelectionReadV2
type ImageRequestContent = wire.ImageRequestContent
type ImageRequestV2 = wire.ImageRequestV2

const (
	SelectionReasonExplicitModel             = wire.SelectionReasonExplicitModel
	SelectionReasonPreferred                 = wire.SelectionReasonPreferred
	SelectionReasonPreferredRouteUnavailable = wire.SelectionReasonPreferredRouteUnavailable
)

// SelectionReasonValues returns every member of SelectionReason in declaration order, in a new slice.
func SelectionReasonValues() []SelectionReason { return wire.SelectionReasonValues() }

const (
	CandidatePlacementLocal  = wire.CandidatePlacementLocal
	CandidatePlacementHosted = wire.CandidatePlacementHosted
)

// CandidatePlacementValues returns every member of CandidatePlacement in declaration order, in a new slice.
func CandidatePlacementValues() []CandidatePlacement { return wire.CandidatePlacementValues() }

const (
	SelectionReadOutcomeFound       = wire.SelectionReadOutcomeFound
	SelectionReadOutcomeUnknown     = wire.SelectionReadOutcomeUnknown
	SelectionReadOutcomeInvalid     = wire.SelectionReadOutcomeInvalid
	SelectionReadOutcomeForbidden   = wire.SelectionReadOutcomeForbidden
	SelectionReadOutcomeUnavailable = wire.SelectionReadOutcomeUnavailable
)

// SelectionReadOutcomeValues returns every member of SelectionReadOutcome in declaration order, in a new slice.
func SelectionReadOutcomeValues() []SelectionReadOutcome { return wire.SelectionReadOutcomeValues() }

// ChatV2 calls the versioned explicit-target chat service. Its operation IDs
// use the same Observe and Cancel registry as chat@1.
type ChatV2 struct{ transport listen.FrameClient }

func NewChatV2(endpoint string) *ChatV2 {
	return NewChatV2WithTransport(listen.FrameClient{Endpoint: endpoint})
}

func NewChatV2WithTransport(transport listen.FrameClient) *ChatV2 {
	return &ChatV2{transport: transport.WithDefaults(callMargin, 1<<20)}
}

func (c *ChatV2) Start(ctx context.Context, request RequestV2) (AdmissionV2, error) {
	if err := ctx.Err(); err != nil {
		return AdmissionV2{}, err
	}
	return wire.NewChatV2Client(c.transport.WithContext(ctx)).Start(request)
}

func (c *ChatV2) StartWithGuarantees(ctx context.Context, request RequestV2, guarantees []wire.RequestGuarantee) (AdmissionV2, error) {
	if err := ctx.Err(); err != nil {
		return AdmissionV2{}, err
	}
	return wire.NewChatV2Client(c.transport.WithContext(ctx)).StartWithGuarantees(request, guarantees)
}

func (c *ChatV2) Observe(ctx context.Context, operation string, cursor, maxDeltas, maxBytes, waitMS int64) (DeltaPage, error) {
	return observeOperation(ctx, c.transport, cursor, maxDeltas, maxBytes, waitMS,
		func(d Delta) bool { return d.Kind != wire.DeltaKindEnd || d.End != nil },
		func(transport listen.FrameClient) (DeltaPage, error) {
			return wire.NewChatV2Client(transport.WithContext(ctx)).Observe(operation, cursor, maxDeltas, maxBytes, waitMS)
		})
}

func (c *ChatV2) Cancel(ctx context.Context, operation string) (Cancellation, error) {
	if err := ctx.Err(); err != nil {
		return Cancellation{}, err
	}
	return wire.NewChatV2Client(c.transport.WithContext(ctx)).Cancel(operation)
}

// Stream starts the explicit-target request once and yields ordered deltas.
// A refusal yields one terminal reply; abandoning or failing observation
// cancels accepted work through the same lifecycle as Chat.Stream.
func (c *ChatV2) Stream(ctx context.Context, request RequestV2) iter.Seq2[Delta, error] {
	return streamChat(ctx, func() (Admission, error) {
		a, err := c.Start(ctx, request)
		return checkedAdmission(Admission{Outcome: a.Outcome, Reason: a.Reason,
			Operation: a.Operation, Host: a.Host, Model: a.Model,
			RetentionMs: a.RetentionMs, IdleMs: a.IdleMs, RetainedDeltas: a.RetainedDeltas}, err)
	}, c)
}

// SelectionOperatorV2 manages the same scoped policy store with optional
// logical provider pins. Pinned and unpinned records share the same store.
type SelectionOperatorV2 struct{ transport listen.FrameClient }

func NewSelectionOperatorV2(endpoint string) *SelectionOperatorV2 {
	return NewSelectionOperatorV2WithTransport(listen.FrameClient{Endpoint: endpoint})
}

func NewSelectionOperatorV2WithTransport(transport listen.FrameClient) *SelectionOperatorV2 {
	return &SelectionOperatorV2{transport: transport.WithDefaults(callMargin, 1<<20)}
}

// ReadEffective reads the caller's effective named policy without management authority.
func (o *SelectionOperatorV2) ReadEffective(ctx context.Context, name string) (SelectionReadV2, error) {
	if err := ctx.Err(); err != nil {
		return SelectionReadV2{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).ReadEffective(name)
}

// ReadAccountDefault reads a named account default with selection.manage_for.
func (o *SelectionOperatorV2) ReadAccountDefault(ctx context.Context, name string) (SelectionReadV2, error) {
	if err := ctx.Err(); err != nil {
		return SelectionReadV2{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).ReadAccountDefault(name)
}

// PutAccountDefault conditionally edits a named account default.
func (o *SelectionOperatorV2) PutAccountDefault(ctx context.Context, expectedRevision string, policy SelectionPolicyV2) (SelectionChange, error) {
	if err := ctx.Err(); err != nil {
		return SelectionChange{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).PutAccountDefault(expectedRevision, policy)
}

// DeleteAccountDefault conditionally deletes a named account default.
func (o *SelectionOperatorV2) DeleteAccountDefault(ctx context.Context, expectedRevision, name string) (SelectionChange, error) {
	if err := ctx.Err(); err != nil {
		return SelectionChange{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).DeleteAccountDefault(expectedRevision, name)
}

func (o *SelectionOperatorV2) Read(ctx context.Context, targetProgram, name string) (SelectionReadV2, error) {
	if err := ctx.Err(); err != nil {
		return SelectionReadV2{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).Read(targetProgram, name)
}

func (o *SelectionOperatorV2) Put(ctx context.Context, targetProgram, expectedRevision string, policy SelectionPolicyV2) (SelectionChange, error) {
	if err := ctx.Err(); err != nil {
		return SelectionChange{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).Put(targetProgram, expectedRevision, policy)
}

func (o *SelectionOperatorV2) DeletePolicy(ctx context.Context, targetProgram, expectedRevision, name string) (SelectionChange, error) {
	if err := ctx.Err(); err != nil {
		return SelectionChange{}, err
	}
	return wire.NewSelectionOperatorV2Client(o.transport.WithContext(ctx)).DeletePolicy(targetProgram, expectedRevision, name)
}
