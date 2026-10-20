package inference

import (
	"context"
	"strings"
	"testing"

	router "github.com/openabstractions/abstraction-router/go"
)

func engineHostConfig(routes *router.Router) Config {
	return Config{Router: routes,
		Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil },
		Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
			return nil, "applied"
		}}
}

// An image engine composed with a router that does not hold its host would
// answer every image request no_host "router:not-here", as pytorchd did
// from 2026-10-02 to 2026-11-09. New refuses that composition, and every
// other way an engine and its declaration can come apart.
func TestNewRefusesAnEngineItsRouterCannotReach(t *testing.T) {
	host, err := router.NewImageEngine("fixture-engine", []string{"stability/sdxl"})
	if err != nil {
		t.Fatal(err)
	}
	// Equal fields, another pointer: dispatch compares the routed host by
	// identity, so a copy is another host.
	copied := *host
	audio := router.OpenAICompatible("fixture-audio", "http://127.0.0.1:1", "/v1", []string{"whisper"})
	cases := []struct {
		name   string
		routes *router.Router
		adjust func(*Config)
		want   string
	}{
		{"image engine with an empty router", router.New(), func(c *Config) {
			c.ImageEngine, c.ImageEngineHost = &testImageEngine{}, host
		}, "image engine host"},
		{"image engine host held by copy only", router.New(&copied), func(c *Config) {
			c.ImageEngine, c.ImageEngineHost = &testImageEngine{}, host
		}, "not a host of the provider's router"},
		{"image engine without its host", router.New(host), func(c *Config) {
			c.ImageEngine = &testImageEngine{}
		}, "image engine needs the router host"},
		{"image host without its engine", router.New(host), func(c *Config) {
			c.ImageEngineHost = host
		}, "has no engine"},
		{"transcription engine with an empty router", router.New(), func(c *Config) {
			c.TranscriptionEngine, c.TranscriptionEngineHost = &fakeTranscriber{}, audio
		}, "transcription engine host"},
		{"speech engine without its host", router.New(audio), func(c *Config) {
			c.SpeechEngine = &fakeSpeaker{}
		}, "speech engine needs the router host"},
		{"speech host without its engine", router.New(audio), func(c *Config) {
			c.SpeechEngineHost = audio
		}, "has no engine"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := engineHostConfig(tc.routes)
			tc.adjust(&cfg)
			p, err := New(cfg)
			if err == nil {
				_ = p.Close()
				t.Fatal("New accepted the composition")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New refused with %q, wanted one naming %q", err, tc.want)
			}
		})
	}
	t.Run("each engine on its own router host", func(t *testing.T) {
		cfg := engineHostConfig(router.New(host, audio))
		cfg.ImageEngine, cfg.ImageEngineHost = &testImageEngine{}, host
		cfg.TranscriptionEngine, cfg.TranscriptionEngineHost = &fakeTranscriber{}, audio
		cfg.SpeechEngine, cfg.SpeechEngineHost = &fakeSpeaker{}, audio
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_ = p.Close()
	})
}
