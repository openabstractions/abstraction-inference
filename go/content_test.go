package inference

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func digestOf(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

func visionRequest(credential, model, digest, mediaType string) wire.Request {
	req := hostedRequest(credential)
	req.Model = model
	req.Messages = []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{
		{Kind: wire.PartKindText, Text: "before"},
		{Kind: wire.PartKindImage, Digest: digest, MediaType: mediaType},
		{Kind: wire.PartKindText, Text: "after"},
	}}}
	return req
}

func TestVisionBytesReachEachBackendInPartOrder(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("fixture image bytes")...)
	digest := digestOf(image)
	encoded := base64.StdEncoding.EncodeToString(image)
	for _, tc := range []struct {
		name, kind, credential, model string
		ordered                       []string
	}{
		{"openai", router.WireOpenAICompatible, "openrouter", "anthropic/claude-sonnet-5", []string{`"text":"before"`, `"url":"data:image/png;base64,` + encoded + `"`, `"text":"after"`}},
		{"anthropic", router.WireAnthropicMessages, "anthropic", "claude-sonnet-5", []string{`"text":"before"`, `"data":"` + encoded + `"`, `"text":"after"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resolved Subject
			f := setup(t, tc.kind, func(c *Config) {
				c.ResolveContent = func(_ context.Context, subject Subject, got string, limit int64) ([]byte, ContentOutcome) {
					resolved = subject
					if got != digest || limit != c.MaxImageBytes && c.MaxImageBytes != 0 {
						t.Fatalf("resolve %q limit %d", got, limit)
					}
					return image, ContentResolved
				}
			})
			a := f.provider.Start(context.Background(), caller, visionRequest(tc.credential, tc.model, digest, "image/png"))
			if a.Outcome != wire.StartOutcomeAccepted {
				t.Fatalf("admission %+v", a)
			}
			drain(t, f.provider, caller, a.Operation)
			if resolved != caller {
				t.Fatalf("resolver subject %+v", resolved)
			}
			body := f.up.posts()[0].body
			at := -1
			for _, fragment := range tc.ordered {
				next := strings.Index(body[at+1:], fragment)
				if next < 0 {
					t.Fatalf("missing %s in %s", fragment, body)
				}
				at += next + 1
			}
			if strings.Contains(body, digest) {
				t.Fatalf("digest leaked into backend request: %s", body)
			}
		})
	}
}

func TestOpenAIImageOnlyMessageReachesTheBackend(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("image only")...)
	digest := digestOf(image)
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			return image, ContentResolved
		}
	})
	req := hostedRequest("openrouter")
	req.Messages = []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{
		{Kind: wire.PartKindImage, Digest: digest, MediaType: "image/png"},
	}}}
	a := f.provider.Start(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", a)
	}
	drain(t, f.provider, caller, a.Operation)
	body := f.up.posts()[0].body
	want := `"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(image) + `"`
	if !strings.Contains(body, want) || strings.Contains(body, `"content":null`) {
		t.Fatalf("image-only message was not encoded: %s", body)
	}
}

func TestVisionResolutionRefusalsSpendNothing(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("scoped image")...)
	digest := digestOf(image)
	for _, tc := range []struct {
		name    string
		content ContentOutcome
		outcome wire.StartOutcome
		reason  string
	}{
		{"unknown", ContentUnknown, wire.StartOutcomeInvalid, "content:unknown"},
		{"other_scope", ContentForbidden, wire.StartOutcomeForbidden, "content:read:forbidden"},
		{"reader_down", ContentUnavailable, wire.StartOutcomeUnavailable, "content:unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			f := setup(t, router.WireOpenAICompatible, func(c *Config) {
				c.ResolveContent = func(_ context.Context, subject Subject, got string, _ int64) ([]byte, ContentOutcome) {
					calls.Add(1)
					if subject != caller || got != digest {
						t.Fatalf("resolve subject=%+v digest=%q", subject, got)
					}
					return nil, tc.content
				}
			})
			seen, uses := len(f.up.requests()), f.applier.count()
			a := f.provider.Start(context.Background(), caller, visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png"))
			if a.Outcome != tc.outcome || a.Reason != tc.reason || calls.Load() != 1 {
				t.Fatalf("admission %+v, resolves %d", a, calls.Load())
			}
			if len(f.up.requests()) != seen || f.applier.count() != uses {
				t.Fatal("content refusal applied a credential or reached upstream")
			}
		})
	}
}

func TestVisionAuthorizationPrecedesContentResolution(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("private image")...)
	digest := digestOf(image)
	var calls atomic.Int32
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			calls.Add(1)
			return image, ContentResolved
		}
	})
	seen, uses := len(f.up.requests()), f.applier.count()
	stranger := Subject{Account: caller.Account, Program: `C:\apps\other.exe`}
	a := f.provider.Start(context.Background(), stranger, visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png"))
	if a.Outcome != wire.StartOutcomeNotPermitted || calls.Load() != 0 {
		t.Fatalf("admission %+v, resolves %d", a, calls.Load())
	}
	if len(f.up.requests()) != seen || f.applier.count() != uses {
		t.Fatal("denied caller caused outgoing work")
	}
}

func TestVisionDigestMediaTypeAndBoundsAreCheckedBeforeSpend(t *testing.T) {
	imageA := append([]byte("\x89PNG\r\n\x1a\n"), []byte("abc")...)
	imageB := append([]byte("\x89PNG\r\n\x1a\n"), []byte("def")...)
	digestA, digestB := digestOf(imageA), digestOf(imageB)
	var calls atomic.Int32
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.MaxImageBytes = 16
		c.MaxVisionBytes = int64(len(imageA) + len(imageB) - 1)
		c.ResolveContent = func(_ context.Context, _ Subject, digest string, _ int64) ([]byte, ContentOutcome) {
			calls.Add(1)
			if digest == digestA {
				return imageA, ContentResolved
			}
			return imageB, ContentResolved
		}
	})
	seen, uses := len(f.up.requests()), f.applier.count()
	for _, part := range []wire.Part{
		{Kind: wire.PartKindImage, Digest: strings.ToUpper(digestA), MediaType: "image/png"},
		{Kind: wire.PartKindImage, Digest: digestA, MediaType: "image/svg+xml"},
	} {
		req := hostedRequest("openrouter")
		req.Messages = []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{part}}}
		if a := f.provider.Start(context.Background(), caller, req); a.Outcome != wire.StartOutcomeInvalid || calls.Load() != 0 {
			t.Fatalf("invalid reference admission %+v resolves %d", a, calls.Load())
		}
	}
	req := hostedRequest("openrouter")
	req.Messages = []wire.Message{{Role: wire.RoleUser, Parts: []wire.Part{
		{Kind: wire.PartKindImage, Digest: digestA, MediaType: "image/png"},
		{Kind: wire.PartKindImage, Digest: digestB, MediaType: "image/png"},
	}}}
	if a := f.provider.Start(context.Background(), caller, req); a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:too-large" || calls.Load() != 2 {
		t.Fatalf("total bound admission %+v resolves %d", a, calls.Load())
	}
	if len(f.up.requests()) != seen || f.applier.count() != uses {
		t.Fatal("invalid or oversized content caused outgoing work")
	}
}

func TestVisionResolutionVerifiesDigestAndBoundsExpandedBytes(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("abc")...)
	digest := digestOf(image)
	t.Run("digest_mismatch", func(t *testing.T) {
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
				return []byte("different"), ContentResolved
			}
		})
		seen, uses := len(f.up.requests()), f.applier.count()
		a := f.provider.Start(context.Background(), caller, visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png"))
		if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:digest" {
			t.Fatalf("admission %+v", a)
		}
		if len(f.up.requests()) != seen || f.applier.count() != uses {
			t.Fatal("digest mismatch caused outgoing work")
		}
	})
	t.Run("same_digest_once", func(t *testing.T) {
		var calls atomic.Int32
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.MaxImageBytes = int64(len(image))
			c.MaxVisionBytes = 2 * int64(len(image))
			c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
				calls.Add(1)
				return image, ContentResolved
			}
		})
		req := visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png")
		req.Messages[0].Parts = append(req.Messages[0].Parts, wire.Part{Kind: wire.PartKindImage, Digest: digest, MediaType: "image/png"})
		a := f.provider.Start(context.Background(), caller, req)
		if a.Outcome != wire.StartOutcomeAccepted || calls.Load() != 1 {
			t.Fatalf("admission %+v resolves %d", a, calls.Load())
		}
		drain(t, f.provider, caller, a.Operation)
	})
	t.Run("repeated_digest_counts_each_occurrence", func(t *testing.T) {
		var calls atomic.Int32
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.MaxImageBytes = int64(len(image))
			c.MaxVisionBytes = int64(len(image))
			c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
				calls.Add(1)
				return image, ContentResolved
			}
		})
		req := visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png")
		req.Messages[0].Parts = append(req.Messages[0].Parts, wire.Part{Kind: wire.PartKindImage, Digest: digest, MediaType: "image/png"})
		a := f.provider.Start(context.Background(), caller, req)
		if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:too-large" || calls.Load() != 1 || len(f.up.posts()) != 0 {
			t.Fatalf("admission %+v resolves %d posts %d", a, calls.Load(), len(f.up.posts()))
		}
	})
	t.Run("media_type_mismatch", func(t *testing.T) {
		f := setup(t, router.WireOpenAICompatible, func(c *Config) {
			c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
				return image, ContentResolved
			}
		})
		a := f.provider.Start(context.Background(), caller, visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/jpeg"))
		if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "content:media-type" || len(f.up.posts()) != 0 {
			t.Fatalf("admission %+v, posts %d", a, len(f.up.posts()))
		}
	})
}

func TestVisionInSystemMessageIsInvalidBeforeResolution(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("system image")...)
	digest := digestOf(image)
	var calls atomic.Int32
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.ResolveContent = func(context.Context, Subject, string, int64) ([]byte, ContentOutcome) {
			calls.Add(1)
			return image, ContentResolved
		}
	})
	seen, uses := len(f.up.requests()), f.applier.count()
	req := hostedRequest("openrouter")
	req.Messages = []wire.Message{{Role: wire.RoleSystem, Parts: []wire.Part{
		{Kind: wire.PartKindText, Text: "inspect"},
		{Kind: wire.PartKindImage, Digest: digest, MediaType: "image/png"},
	}}}
	a := f.provider.Start(context.Background(), caller, req)
	if a.Outcome != wire.StartOutcomeInvalid || a.Reason != "part:image" || calls.Load() != 0 {
		t.Fatalf("admission %+v resolves %d", a, calls.Load())
	}
	if len(f.up.requests()) != seen || f.applier.count() != uses {
		t.Fatal("invalid system image caused outgoing work")
	}
}

func TestVisionResolutionHonoursCancellation(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("waited image")...)
	digest := digestOf(image)
	entered := make(chan struct{})
	f := setup(t, router.WireOpenAICompatible, func(c *Config) {
		c.ResolveContent = func(ctx context.Context, _ Subject, _ string, _ int64) ([]byte, ContentOutcome) {
			close(entered)
			<-ctx.Done()
			return nil, ContentUnavailable
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan wire.Admission, 1)
	go func() {
		done <- f.provider.Start(ctx, caller, visionRequest("openrouter", "anthropic/claude-sonnet-5", digest, "image/png"))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resolver was not called")
	}
	cancel()
	select {
	case a := <-done:
		if a.Outcome != wire.StartOutcomeUnavailable || a.Reason != "content:unavailable" {
			t.Fatalf("admission %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled resolution did not return")
	}
	if len(f.up.posts()) != 0 {
		t.Fatal("cancelled resolution reached upstream")
	}
}
