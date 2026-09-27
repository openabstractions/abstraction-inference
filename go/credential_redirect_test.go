package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialedUpstreamDoesNotFollowRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("redirect target received credential %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/next", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	_, reply := post(context.Background(), &http.Client{}, origin.URL, map[string]any{"model": "m"}, map[string]string{"X-Api-Key": "upstream-secret"})
	if reply == nil || reply.Reason != "upstream:unreachable" {
		t.Fatalf("redirect outcome: %+v", reply)
	}
	if reached {
		t.Fatal("redirect target was reached")
	}
	_, image := imageRequest(context.Background(), &http.Client{}, http.MethodPost, origin.URL, "application/json", strings.NewReader(`{}`), map[string]string{"X-Api-Key": "image-secret"})
	if image.reason != "upstream:unreachable" {
		t.Fatalf("image redirect outcome: %+v", image)
	}
	if reached {
		t.Fatal("image redirect target was reached")
	}

	// Calls without an applied credential keep ordinary HTTP redirects.
	reached = false
	resp, public := post(context.Background(), &http.Client{}, origin.URL, map[string]any{"model": "m"}, nil)
	if public != nil || !reached {
		t.Fatalf("public redirect: reached=%v reply=%+v", reached, public)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialedLiveClientDoesNotFollowRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Errorf("redirect target received credential %q", got)
		}
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/realtime", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := credentialRequestClient(&http.Client{}, map[string]string{"X-Api-Key": "live-secret"})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL+"/realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "live-secret")
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || reached {
		t.Fatalf("live credential redirect: reached=%v err=%v", reached, err)
	}
}
