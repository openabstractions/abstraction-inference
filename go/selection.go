package inference

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const ActionSelectionManage = "abstraction.inference/selection.manage"
const ActionSelectionManageFor = "abstraction.inference/selection.manage_for"

type SelectionResolverV2 func(ctx context.Context, subject Subject, name string) (wire.SelectionPolicyV2, bool, error)

// resolveSelection reads the service-owned scoped policy, including provider pins.
func (p *Provider) resolveSelection(ctx context.Context, subject Subject, name string) (wire.SelectionPolicyV2, bool, error) {
	if p.cfg.ResolveSelectionV2 == nil {
		return wire.SelectionPolicyV2{}, false, nil
	}
	return p.cfg.ResolveSelectionV2(ctx, subject, name)
}

func validSelectionCandidate(candidate wire.SelectionCandidateV2) bool {
	if candidate.Model == "" || len(candidate.Model) > 256 ||
		(candidate.Placement != wire.CandidatePlacementLocal && candidate.Placement != wire.CandidatePlacementHosted) ||
		(candidate.Placement == wire.CandidatePlacementLocal && candidate.Credential != "") ||
		(candidate.Placement == wire.CandidatePlacementHosted && candidate.Credential == "") {
		return false
	}
	return candidate.Provider == "" || len(candidate.Provider) <= 64 && nameChars(candidate.Provider, "_.-")
}

// selectedHostCurrent prevents a replaced declaration from being admitted
// through a router snapshot that predates the replacement.
func (p *Provider) selectedHostCurrent(host *router.Host) bool {
	if host == nil {
		return false
	}
	for _, current := range p.cfg.Router.Hosts() {
		if current == host && current.Servable() {
			return true
		}
	}
	return false
}

// ValidateTarget enforces the exclusive, nonempty chat@2 target.
func ValidateTarget(target wire.Target) bool {
	return (target.Model != "") != (target.SelectionPolicy != "")
}

// RequestFromContent retains chat@1's exact request semantics after chat@2
// chooses a model. The selected candidate supplies only Model.
func RequestFromContent(model string, content wire.RequestContent) wire.Request {
	return wire.Request{Model: model, Messages: content.Messages, Tools: content.Tools, Options: content.Options,
		Extensions: content.Extensions, RequiredExtensions: content.RequiredExtensions,
		Guarantees: content.Guarantees, Credential: content.Credential}
}
