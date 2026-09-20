package gateway

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	inference "github.com/openabstractions/abstraction-inference/go"
)

// An OpenAI-style embeddings client reaches the local host through the window
// under its own key: number arrays by default, base64 float32 on request, and
// the audit records the window route and the embed profile.
func TestAnEmbeddingsClientThroughTheWindow(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	f.up.server.Config.Handler.(*http.ServeMux).HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[0.5,-1]},{"index":1,"embedding":[2,0.25]}],"usage":{"prompt_tokens":3}}`))
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.grant("oalk-embed", filepath.Clean(exe), true)
	post := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, f.base+"/v1/embeddings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer oalk-embed")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("status %d body %s", resp.StatusCode, raw)
		}
		return resp.StatusCode, out
	}
	code, out := post(`{"model":"` + model + `","input":["a","b"]}`)
	data, _ := out["data"].([]any)
	if code != http.StatusOK || len(data) != 2 {
		t.Fatalf("float embeddings: %d %v", code, out)
	}
	first := data[0].(map[string]any)["embedding"].([]any)
	if first[0] != 0.5 || first[1] != -1.0 {
		t.Fatalf("first vector %v", first)
	}
	code, out = post(`{"model":"` + model + `","input":["a","b"],"encoding_format":"base64"}`)
	data, _ = out["data"].([]any)
	if code != http.StatusOK || len(data) != 2 || data[1].(map[string]any)["embedding"] != packFloat32([]float32{2, 0.25}) {
		t.Fatalf("base64 embeddings: %d %v", code, out)
	}
	if code, _ = post(`{"model":"` + model + `","input":[[1,2,3]]}`); code != http.StatusBadRequest {
		t.Fatalf("token array input: %d", code)
	}
	records := f.waitLogged(t, 1)
	last := records[len(records)-1]
	for _, r := range records {
		if r.Outcome == "completed" {
			last = r
		}
	}
	if last.Route != inference.RouteWindow || last.Profile != "embed" || last.TokensIn != 3 {
		t.Fatalf("audit record %+v", last)
	}
}

// packFloat32 is the base64 of values as little-endian float32, the form a
// base64 OpenAI embedding takes.
func packFloat32(values []float32) string {
	packed := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(packed[4*i:], math.Float32bits(v))
	}
	return base64.StdEncoding.EncodeToString(packed)
}
