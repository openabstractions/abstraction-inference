package inference

import (
	"context"
	"errors"
	"time"
)

// standingInterval is how often the watchdog decides every running operation
// again (Recheck), so a rule that expires, which commits no change, ends what
// it admitted within one interval (CONTRACT.md INF-A6). It is the resource
// service's expiry clock, which ends a revoked lease on the same beat.
const standingInterval = 5 * time.Second

// A StandingDecider decides again, asking nobody, whether subject may still
// do what an operation it started does: complete on the model server named
// host and, when credential is not empty, use that credential. It answers ""
// while both are permitted, a refusal word in Start's reason form
// (rights:<word> or credential:<word>) when either is refused, and an error
// when no decision could be obtained (INF-A6).
type StandingDecider func(ctx context.Context, subject Subject, host, credential string) (refusal string, err error)

// errStandingUnavailable is a standing decision that could not be obtained.
var errStandingUnavailable = errors.New("inference: standing decision unavailable")

// standing decides again for subject on host and credential (INF-A6). Without
// Config.Standing it decides complete through Decide and no credential, which
// is what a leaf provider's static decision needs.
func (p *Provider) standing(ctx context.Context, subject Subject, host, credential string) (string, error) {
	if p.cfg.Standing != nil {
		refusal, err := p.cfg.Standing(ctx, subject, host, credential)
		if err != nil || ctx.Err() != nil {
			p.report(err)
			return "", errStandingUnavailable
		}
		return refusal, nil
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host))
	if err != nil || ctx.Err() != nil {
		p.report(err)
		return "", errStandingUnavailable
	}
	switch word {
	case "permitted":
		return "", nil
	case "denied", "not_granted", "unknown_action":
		return "rights:" + word, nil
	}
	return "", errStandingUnavailable
}

// standingOf decides again for op: complete on its server, and the
// credential it applied here. An operation delegated to a remote runtime
// applied no credential here; that runtime decides its own.
func (p *Provider) standingOf(ctx context.Context, op *operation) (string, error) {
	credential := op.credential
	if op.remote {
		credential = ""
	}
	return p.standing(ctx, op.subject, op.host.Name, credential)
}

// Recheck decides every running operation again and stops each one whose
// rule no longer permits it: a revoked, blocked or expired grant on complete
// for its server or on the credential it applied (INF-A6). A stopped operation
// ends cancelled with the refusal as its reason. A decision that could not be
// obtained stops nothing here; a live session's next input is refused while
// it lasts. The watchdog calls it every standingInterval, and a composition
// also calls it on each committed rights change (abstraction-rights
// RIGHTS-R23), so a revoke ends what it admitted before the edit replies.
func (p *Provider) Recheck(ctx context.Context) {
	p.mu.Lock()
	running := make([]*operation, 0, len(p.ops))
	for _, op := range p.ops {
		running = append(running, op)
	}
	p.mu.Unlock()
	type holding struct {
		subject          Subject
		host, credential string
	}
	decided := map[holding]string{}
	for _, op := range running {
		if op.isEnded() || op.host == nil {
			continue
		}
		credential := op.credential
		if op.remote {
			credential = ""
		}
		key := holding{subject: op.subject, host: op.host.Name, credential: credential}
		refusal, ok := decided[key]
		if !ok {
			var err error
			refusal, err = p.standing(ctx, op.subject, op.host.Name, credential)
			if err != nil {
				refusal = ""
			}
			decided[key] = refusal
		}
		if refusal != "" {
			op.stop(refusal)
		}
	}
}
