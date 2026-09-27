package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"sort"
	"strconv"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// EmbedContract is the embeddings profile this provider serves.
const EmbedContract = "abstraction.inference/embed@1"

// Bounds of one embed@1 call.
const (
	MaxEmbedInputs     = 64
	MaxEmbedInputBytes = 32768
	MaxEmbedDimensions = 4096
	MaxEmbedValues     = 131072
	// embedReplyBytes bounds an upstream reply body read.
	embedReplyBytes = 32 << 20
)

// validateEmbed returns the reason an embed request is invalid, or "".
func validateEmbed(req wire.EmbedRequest) string {
	if req.Model == "" || len(req.Model) > 256 {
		return "model"
	}
	if len(req.Inputs) < 1 || len(req.Inputs) > MaxEmbedInputs {
		return "inputs"
	}
	for _, in := range req.Inputs {
		if len(in) < 1 || len(in) > MaxEmbedInputBytes {
			return "inputs"
		}
	}
	if req.Dimensions < 0 || req.Dimensions > MaxEmbedDimensions {
		return "dimensions"
	}
	shape := validate(wire.Request{Model: req.Model, Messages: []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{{Kind: wire.PartKindText, Text: "-"}}}},
		Extensions: req.Extensions, Guarantees: req.Guarantees, Credential: req.Credential})
	return shape
}

// Embed computes embeddings for subject in one bounded call, admitting it in
// the contract's order for profile embed. Nothing leaves the service before
// every check passes.
func (p *Provider) Embed(ctx context.Context, subject Subject, req wire.EmbedRequest) wire.Embeddings {
	started := p.cfg.Now()
	via := routeOf(ctx)
	if via.name == RouteRemote {
		via.claim = req.Extensions[ClaimExtension]
	}
	var host *router.Host
	model, credential := "", ""
	finish := func(e wire.Embeddings) wire.Embeddings {
		if e.Outcome != wire.EmbedOutcomeCompleted {
			e.Vectors, e.Dimensions, e.Usage, e.Cost = nil, 0, wire.Usage{}, nil
		}
		if e.Vectors == nil {
			e.Vectors = []string{}
		}
		r := Record{Profile: router.ProfileEmbed, Route: via.name, Rung: via.rung, Account: subject.Account, Program: subject.Program,
			Model: req.Model, Family: familyOf(req.Model), Credential: req.Credential, Outcome: e.Outcome.String(), Reason: e.Reason,
			TokensIn: e.Usage.Input, WallMS: p.cfg.Now().Sub(started).Milliseconds(), Domain: via.domain, Claim: via.claim}
		if host != nil {
			r.Host, r.Model = host.Name, model
			if host.Wire == router.WireRemote {
				r.Domain = host.Domain
			}
		}
		if p.cfg.Ceilings != nil && req.Credential != "" {
			r.Ceiling = p.cfg.Ceilings.State(req.Credential)
		}
		p.record(r)
		return e
	}
	refuse := func(outcome wire.EmbedOutcome, reason string) wire.Embeddings {
		return finish(wire.Embeddings{Outcome: outcome, Reason: reason})
	}
	if subject.Account == "" || subject.Program == "" {
		return refuse(wire.EmbedOutcomeForbidden, "caller")
	}
	if reason := validateEmbed(req); reason != "" {
		return refuse(wire.EmbedOutcomeInvalid, reason)
	}
	var reason string
	host, model, reason = p.pick(ctx, req.Model, requestGuaranteeWords(req.Guarantees), req.Credential, router.ProfileEmbed)
	if host == nil {
		return refuse(wire.EmbedOutcomeNoHost, reason)
	}
	if p.cfg.Admit != nil {
		if admitReason, ok := p.cfg.Admit(ctx, host, model); !ok {
			return refuse(admitEmbedOutcome(admitReason), admitReason)
		}
	}
	remote := host.Wire == router.WireRemote
	native := host.Wire == router.WireNative
	address := ""
	if !remote && !native {
		if host.Wire == "" || host.Wire == router.WireOpenAICompatible {
			address = host.EmbedURL()
		}
		if address == "" {
			return refuse(wire.EmbedOutcomeUnsupportedFeature, "wire:"+host.Wire)
		}
	}
	word, err := p.cfg.Decide(ctx, subject, ActionComplete, ResourceHost(host.Name))
	if err != nil || ctx.Err() != nil {
		p.report(err)
		return refuse(wire.EmbedOutcomeUnavailable, "rights:unavailable")
	}
	switch word {
	case "permitted":
	case "denied", "not_granted", "unknown_action":
		return refuse(wire.EmbedOutcomeNotPermitted, "rights:"+word)
	default:
		return refuse(wire.EmbedOutcomeUnavailable, "rights:unavailable")
	}
	if host.Hosted {
		credential = host.Credential
	}
	if credential != "" && p.cfg.Ceilings != nil && !remote {
		if unit, over := p.cfg.Ceilings.Exceeded(credential); over {
			return refuse(wire.EmbedOutcomeBudgetExceeded, "ceiling:"+unit+":"+credential)
		}
	}
	var result wire.Embeddings
	switch {
	case remote:
		result = embedRemote(ctx, host, model, req, subject)
	case native:
		result = nativeEmbed(ctx, host, model, req)
	default:
		var headers map[string]string
		if credential != "" {
			var outcome string
			headers, outcome = p.cfg.Apply(ctx, subject, EmbedContract, credential, router.Target(host.Base))
			switch outcome {
			case "applied":
			case "unavailable", "":
				return refuse(wire.EmbedOutcomeUnavailable, "credential:unavailable:"+credential)
			default:
				return refuse(wire.EmbedOutcomeNotPermitted, "credential:"+outcome+":"+credential)
			}
		}
		result = embedOpenAI(ctx, p.cfg.HTTP, address, model, req, headers)
		clear(headers)
	}
	if credential != "" && p.cfg.Ceilings != nil && !remote {
		var micros int64
		if result.Cost != nil {
			micros = result.Cost.Micros
		}
		if err := p.cfg.Ceilings.Add(credential, result.Usage.Input, micros); err != nil {
			p.report(err)
		}
	}
	result.Host, result.Model = host.Name, model
	return finish(result)
}

func embedFailed(outcome wire.EmbedOutcome, reason string) wire.Embeddings {
	return wire.Embeddings{Outcome: outcome, Reason: reason}
}

// embedOpenAI sends one OpenAI-compatible embeddings request and reads the
// vectors, as float arrays or as base64 float32, into the contract's form.
func embedOpenAI(ctx context.Context, client *http.Client, address, model string, req wire.EmbedRequest, headers map[string]string) wire.Embeddings {
	body := map[string]any{"model": model, "input": req.Inputs}
	if req.Dimensions > 0 {
		body["dimensions"] = req.Dimensions
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return embedFailed(wire.EmbedOutcomeInvalid, "upstream:encoding")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(raw))
	if err != nil {
		return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:address")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := credentialRequestClient(client, headers).Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:cancelled")
		}
		return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		//unchecked: best-effort drain for connection reuse; the outcome is already decided by the status code below
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		outcome := wire.EmbedOutcomeRefused
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			outcome = wire.EmbedOutcomeUnavailable
		}
		return embedFailed(outcome, "upstream:"+strconv.Itoa(resp.StatusCode))
	}
	var reply struct {
		Data []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, embedReplyBytes)).Decode(&reply); err != nil {
		return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:malformed")
	}
	if len(reply.Data) != len(req.Inputs) {
		return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:malformed")
	}
	sort.SliceStable(reply.Data, func(i, j int) bool { return reply.Data[i].Index < reply.Data[j].Index })
	out := wire.Embeddings{Outcome: wire.EmbedOutcomeCompleted, Vectors: make([]string, len(reply.Data)), Usage: wire.Usage{Input: reply.Usage.PromptTokens}}
	total := int64(0)
	for i, d := range reply.Data {
		if d.Index != i {
			return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:malformed")
		}
		packed, ok := float32Vector(d.Embedding)
		if !ok {
			return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:malformed")
		}
		n := int64(len(packed) / 4)
		if i == 0 {
			out.Dimensions = n
		}
		if n < 1 || n != out.Dimensions {
			return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:malformed")
		}
		if total += n; total > MaxEmbedValues {
			return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:too_large")
		}
		out.Vectors[i] = base64.StdEncoding.EncodeToString(packed)
	}
	if req.Dimensions > 0 && out.Dimensions != req.Dimensions {
		return embedFailed(wire.EmbedOutcomeUnavailable, "upstream:dimensions")
	}
	return out
}

// float32Vector packs an OpenAI embedding, a JSON array of numbers or a base64
// string of little-endian float32 values, as little-endian float32 bytes.
func float32Vector(raw json.RawMessage) ([]byte, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		packed, err := base64.StdEncoding.DecodeString(text)
		return packed, err == nil && len(packed) > 0 && len(packed)%4 == 0
	}
	var values []float64
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return nil, false
	}
	packed := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(packed[4*i:], math.Float32bits(float32(v)))
	}
	return packed, true
}

// embedRemote delegates the call to another runtime over the remote transport.
// The request travels unchanged except for the chosen model name and the claim
// of the program that asked; the remote applies its own credential.
func embedRemote(ctx context.Context, host *router.Host, model string, req wire.EmbedRequest, subject Subject) wire.Embeddings {
	transport, ok := host.RemoteTransport()
	if !ok {
		return embedFailed(wire.EmbedOutcomeUnavailable, "remote:transport")
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Extensions = maps.Clone(req.Extensions)
	if forwarded.Extensions == nil {
		forwarded.Extensions = map[string]string{}
	}
	forwarded.Extensions[ClaimExtension] = subject.Program
	result, err := wire.NewEmbedderClient(transport.WithContext(ctx)).Embed(forwarded)
	if err != nil {
		return embedFailed(wire.EmbedOutcomeUnavailable, "remote:unreachable")
	}
	if result.Outcome != wire.EmbedOutcomeCompleted {
		outcome := result.Outcome
		if !outcome.Known() {
			outcome = wire.EmbedOutcomeUnavailable
		}
		return embedFailed(outcome, "remote:"+result.Reason)
	}
	return result
}

// nativeEmbed delegates embed@1 to a declared local provider's own OA
// endpoint over the identity-bound native transport, mirroring nativeChat's
// dispatch to the same downstream: the request travels with the chosen model
// name, and the downstream observes this service as its caller.
func nativeEmbed(ctx context.Context, host *router.Host, model string, req wire.EmbedRequest) wire.Embeddings {
	transport, ok := host.NativeTransport()
	if !ok {
		return embedFailed(wire.EmbedOutcomeUnavailable, "native:transport")
	}
	forwarded := req
	forwarded.Model = model
	forwarded.Extensions = nil
	result, err := wire.NewEmbedderClient(transport.WithContext(ctx)).Embed(forwarded)
	if err != nil {
		return embedFailed(wire.EmbedOutcomeUnavailable, "native:unreachable")
	}
	if result.Outcome != wire.EmbedOutcomeCompleted {
		outcome := result.Outcome
		if !outcome.Known() {
			outcome = wire.EmbedOutcomeUnavailable
		}
		return embedFailed(outcome, "native:"+result.Reason)
	}
	return result
}
