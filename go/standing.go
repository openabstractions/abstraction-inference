package inference

import (
	"context"
	"errors"
	rightsclient "github.com/openabstractions/abstraction-rights/go/client"
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
	outcome, _ := rightsclient.ParseDecisionOutcome(word)
	switch {
	case outcome == rightsclient.DecisionOutcomePermitted:
		return "", nil
	case rightsclient.Evaluated(outcome):
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
	return p.standing(op.standingContext(ctx), op.subject, op.host.Name, credential)
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
	_ = p.RecheckChecked(ctx)
}

// RecheckChecked preserves unavailable work and reports every decision it
// could not obtain. A rights editor can distinguish saved policy from complete
// settlement; periodic Recheck keeps its existing uncertainty behavior.
func (p *Provider) RecheckChecked(ctx context.Context) error {
	p.mu.Lock()
	running := make([]*operation, 0, len(p.ops))
	for _, op := range p.ops {
		running = append(running, op)
	}
	p.mu.Unlock()
	type holding struct {
		subject          Subject
		host, credential string
		remoteOperation  *operation
	}
	decided := map[holding]string{}
	var failure error
	for _, op := range running {
		if err := ctx.Err(); err != nil {
			return errors.Join(failure, err)
		}
		if op.isEnded() || op.host == nil {
			continue
		}
		credential := op.credential
		if op.remote {
			credential = ""
		}
		key := holding{subject: op.subject, host: op.host.Name, credential: credential}
		if op.remoteValues != nil {
			key.remoteOperation = op
		}
		refusal, ok := decided[key]
		if !ok {
			var err error
			refusal, err = p.standingOf(ctx, op)
			if err != nil {
				failure = errors.Join(failure, err)
				refusal = ""
			}
			decided[key] = refusal
		}
		if refusal != "" {
			op.stop(refusal)
		}
	}
	return errors.Join(failure, ctx.Err())
}
