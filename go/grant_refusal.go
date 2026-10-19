package inference

import (
	"context"
	"slices"
	"strings"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// grantRefusal is the refusal of one request that lacks grants. Every grant
// a request needs is decided before it answers: complete on its host, the
// input profile it consumes, its output profile. A request missing several is
// refused once, naming each in that order separated by spaces, and the
// first-use question of each is admitted in the same pass, so a person
// answers them together instead of one per attempt (INF-V4). Nothing is
// granted here: each grant still needs its own answer. The job executor and
// the synchronous image@1 and speech@1 calls share it.
type grantRefusal struct {
	reasons []string
	// complete reports that complete on the host was refused.
	complete bool
	// final reports a refusal no answer can lift (complete denied, or an
	// unknown action): the request is refused at once and nothing more is
	// decided or asked.
	final bool
}

func (g *grantRefusal) add(reason string) {
	if !slices.Contains(g.reasons, reason) {
		g.reasons = append(g.reasons, reason)
	}
}

func (g *grantRefusal) refused() bool { return len(g.reasons) > 0 }

func (g *grantRefusal) reason() string { return strings.Join(g.reasons, " ") }

// startOutcome is a synchronous call's outcome for the refusal: not_permitted
// when complete was refused, as a call refused on complete alone reads, and
// forbidden for a refused input or output.
func (g *grantRefusal) startOutcome() wire.StartOutcome {
	if g.complete {
		return wire.StartOutcomeNotPermitted
	}
	return wire.StartOutcomeForbidden
}

// decideComplete decides complete on host for subject. A refusal joins g. Only
// not_granted lets the other grants be gathered, since only then can a
// person's answers let the request run: a request that consumes an input
// profile admits that profile's paired question beside it (INF-V4). A denied
// complete or an unknown action is final: the caller refuses with that word
// alone and asks nothing more. It reports false when no decision could be
// obtained, which every word other than permitted, denied, not_granted and
// unknown_action is.
func (p *Provider) decideComplete(ctx context.Context, subject Subject, host *router.Host, input string, g *grantRefusal) bool {
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		p.report(err)
		return false
	}
	switch word {
	case "permitted":
	case "not_granted":
		if input != "" {
			p.askInput(ctx, subject, input)
		}
		g.complete = true
		g.add("rights:" + word)
	case "denied", "unknown_action":
		g.complete, g.final = true, true
		g.add("rights:" + word)
	default:
		return false
	}
	return true
}

// prepareOutputs authorizes count generated outputs of profile before paid
// work and returns their single-use commits. A refused output joins g as
// output:forbidden, and AskOutput is told so the runtime may admit the
// output profile's paired question (INF-S7); a runtime whose preparer admits
// it already leaves AskOutput nil. It reports false when no authorization
// could be obtained.
func (p *Provider) prepareOutputs(ctx context.Context, subject Subject, profile, media string, limit, count int64, g *grantRefusal) ([]ContentCommitter, bool) {
	if p.cfg.PrepareContentWrite == nil {
		return nil, false
	}
	commits := make([]ContentCommitter, 0, count)
	for i := int64(0); i < count; i++ {
		commit, outcome := p.cfg.PrepareContentWrite(ctx, subject, profile, media, limit)
		if outcome == ContentForbidden {
			p.askOutput(ctx, subject, profile)
			g.add("output:forbidden")
			return nil, true
		}
		if outcome != ContentResolved || commit == nil {
			return nil, false
		}
		commits = append(commits, commit)
	}
	return commits, true
}
