package inference

import (
	"context"
	"slices"
	"strings"

	router "github.com/openabstractions/abstraction-router/go"
)

// ReservedExtensionPrefix names the extension namespace OpenAbstractions owns.
// Its keys carry provenance between OA services. Engine and wire adapters
// receive only vendor controls.
const ReservedExtensionPrefix = "openabstractions/"

type designatedRuntimeKey struct{}

// WithDesignatedRuntime marks an admitting call whose bound caller is one of
// the OA runtimes a leaf provider designates to authorize applications. Start
// then records that runtime's ClaimExtension as the original caller. The
// service boundary sets it after binding the caller; request fields cannot.
func WithDesignatedRuntime(ctx context.Context) context.Context {
	return context.WithValue(ctx, designatedRuntimeKey{}, true)
}

// claimTrusted reports whether the bound caller is a runtime the provider
// trusts to name the original caller: a remote runtime mapped by its
// certificate, or a designated runtime of a leaf provider.
func claimTrusted(ctx context.Context) bool {
	if routeOf(ctx).name == RouteRemote {
		return true
	}
	designated, _ := ctx.Value(designatedRuntimeKey{}).(bool)
	return designated
}

// providerExtensions is the result of separating one request's extensions at
// the provider's admission boundary.
type providerExtensions struct {
	// vendor holds every non-reserved control, unchanged.
	vendor map[string]string
	// claim is the original caller a trusted delegating runtime named, or "".
	claim string
	// required is the first reserved key the caller listed as required.
	required string
}

// separateExtensions is the one place a provider takes OA-owned keys out of a
// received request. Every Start and Embed calls it before validation reads the
// request and assigns vendor back before any adapter, engine or forwarder sees
// it. A claim from a caller the provider does not trust is discarded.
func separateExtensions(ctx context.Context, values map[string]string, required []string) providerExtensions {
	out := providerExtensions{vendor: values}
	for _, key := range required {
		if strings.HasPrefix(key, ReservedExtensionPrefix) {
			out.required = key
			break
		}
	}
	reserved := false
	for key := range values {
		if strings.HasPrefix(key, ReservedExtensionPrefix) {
			reserved = true
			break
		}
	}
	if !reserved {
		return out
	}
	if claimTrusted(ctx) {
		out.claim = values[ClaimExtension]
	}
	out.vendor = make(map[string]string, len(values))
	for key, value := range values {
		if !strings.HasPrefix(key, ReservedExtensionPrefix) {
			out.vendor[key] = value
		}
	}
	return out
}

// unsupportedSelectedExtensions consults the selected registration generation,
// not display capabilities or another host's assertion. The provider still
// validates the values and model-specific constraints when it receives Start.
func unsupportedSelectedExtensions(host *router.Host, profile string, required []string) string {
	var known map[string]bool
	if host != nil {
		known = host.SupportedExtensions[profile]
	}
	for _, key := range required {
		if !known[key] {
			return "extension:" + key
		}
	}
	return ""
}

// forwardedNativeExtensions copies caller controls without letting a caller
// forge the claim that a delegating runtime makes about its bound app.
func forwardedNativeExtensions(values map[string]string, required []string, program string) (map[string]string, []string) {
	out := cloneStrings(values)
	if out == nil {
		out = make(map[string]string, 1)
	}
	out[ClaimExtension] = program
	return out, slices.Clone(required)
}
