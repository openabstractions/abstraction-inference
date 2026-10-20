package inference

import (
	"context"
	"slices"
	"sync"
)

// The gateway window's per-call context (INF-W5, INF-W10): a call admitted
// under a key that names no credential spends under the credential each
// hosted model server declares, and the first-use questions its refusal
// admitted are collected so that the window can hold the call open until
// they are answered. Only an in-process boundary sets either value; no wire
// field carries them.

type declaredCredentialKey struct{}

// WithDeclaredCredential lets a call that carries the hosted-allowed
// guarantee and no credential name reach a hosted model server under the
// credential that server's declaration names. abstraction.credentials/apply
// for the caller still decides every use, and INF-C2's budget of that
// credential still applies at admission.
func WithDeclaredCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, declaredCredentialKey{}, true)
}

func declaredCredential(ctx context.Context) bool {
	declared, _ := ctx.Value(declaredCredentialKey{}).(bool)
	return declared
}

// Asked collects the ids of the first-use questions a call's refusal admitted
// or found pending. The decision point adds them (AskedFrom); the boundary
// that attached it reads them after Start returns.
type Asked struct {
	mu  sync.Mutex
	ids []string
}

type askedKey struct{}

// WithAsked attaches a to ctx.
func WithAsked(ctx context.Context, a *Asked) context.Context {
	return context.WithValue(ctx, askedKey{}, a)
}

// AskedFrom is the collector WithAsked attached to ctx, or nil.
func AskedFrom(ctx context.Context) *Asked {
	a, _ := ctx.Value(askedKey{}).(*Asked)
	return a
}

// Add records one question id; a repeated id is kept once.
func (a *Asked) Add(id string) {
	if a == nil || id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Contains(a.ids, id) {
		a.ids = append(a.ids, id)
	}
}

// IDs returns the recorded question ids in the order they were added.
func (a *Asked) IDs() []string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ids)
}
