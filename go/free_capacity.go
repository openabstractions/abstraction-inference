package inference

import (
	"context"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
)

type freeCapacityKey struct{}

func (p *Provider) supportsFreeCapacity() bool {
	return p.cfg.FreeCapacity && p.cfg.Admit != nil
}

func chat3HasGuarantee(request wire.RequestV3, guarantee wire.ChatGuarantee) bool {
	for _, g := range request.Content.Guarantees {
		if g == guarantee {
			return true
		}
	}
	return false
}

// FreeCapacityAdmission reports the chat caller's no-eviction restriction to
// the provider's admission and accepted-execution preparation owners.
func FreeCapacityAdmission(ctx context.Context) bool {
	if required, _ := ctx.Value(freeCapacityKey{}).(bool); required {
		return true
	}
	state, _ := ctx.Value(chat3RunKey{}).(*chat3Run)
	return state != nil && chat3HasGuarantee(state.request, wire.ChatGuaranteeFreeCapacity)
}
