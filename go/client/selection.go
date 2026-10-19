package client

import (
	"context"

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

// SelectionOperatorV2 manages the same scoped policy store with optional
// logical provider pins. Pinned and unpinned records share the same store.
type SelectionOperatorV2 struct{ transport listen.FrameClient }

func NewSelectionOperatorV2(endpoint string) *SelectionOperatorV2 {
	return NewSelectionOperatorV2WithTransport(listen.FrameClient{Endpoint: endpoint})
}

func NewSelectionOperatorV2WithTransport(transport listen.FrameClient) *SelectionOperatorV2 {
	return &SelectionOperatorV2{transport: transport.WithDefaults(callMargin, 1<<20)}
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
