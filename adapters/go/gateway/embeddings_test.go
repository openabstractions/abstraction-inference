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
	"strconv"
	"strings"
	"sync"
	"testing"

	identity "github.com/openabstractions/abstraction-identity"
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
	f.grant("oalk-embed", identity.CanonicalProgramPath(filepath.Clean(exe)), true)
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

// A RAG indexer sends 150 texts in one request. The window splits them into
// embed@1 calls the contract admits, each within its 64-input bound, and the
// client receives 150 vectors in input order with the usage summed. A
// token-array input names the LangChain switch that sends text instead.
func TestALargeEmbeddingsBatchIsSplitAndJoinedInOrder(t *testing.T) {
	if !gatewayTransportProvesProgram(t) {
		t.Skip("Program proof unavailable on current loopback transport")
	}
	f := setup(t, realBinding(t))
	var sizes []int
	var mu sync.Mutex
	f.up.server.Config.Handler.(*http.ServeMux).HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		sizes = append(sizes, len(in.Input))
		mu.Unlock()
		data := make([]map[string]any, len(in.Input))
		for i, text := range in.Input {
			n, _ := strconv.Atoi(strings.TrimPrefix(text, "doc-"))
			data[i] = map[string]any{"index": i, "embedding": []float64{float64(n), 1}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": len(in.Input)}})
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.grant("oalk-embed", identity.CanonicalProgramPath(filepath.Clean(exe)), true)
	post := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, f.base+"/v1/embeddings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer oalk-embed")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	texts := make([]string, 150)
	for i := range texts {
		texts[i] = `"doc-` + strconv.Itoa(i) + `"`
	}
	code, out := post(`{"model":"` + model + `","input":[` + strings.Join(texts, ",") + `]}`)
	data, _ := out["data"].([]any)
	if code != http.StatusOK || len(data) != 150 {
		t.Fatalf("batch of 150: %d %d vectors %v", code, len(data), out["error"])
	}
	for i, d := range data {
		item := d.(map[string]any)
		if item["index"] != float64(i) || item["embedding"].([]any)[0] != float64(i) {
			t.Fatalf("vector %d out of order: %v", i, item)
		}
	}
	if usage := out["usage"].(map[string]any); usage["prompt_tokens"] != 150.0 {
		t.Fatalf("usage %v", usage)
	}
	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, n := range sizes {
		if n > inference.MaxEmbedInputs {
			t.Fatalf("one embed@1 call carried %d inputs: %v", n, sizes)
		}
		total += n
	}
	if len(sizes) < 3 || total != 150 {
		t.Fatalf("provider calls %v", sizes)
	}
	code, out = post(`{"model":"` + model + `","input":[[1,2,3]]}`)
	if msg, _ := out["error"].(map[string]any)["message"].(string); code != http.StatusBadRequest || !strings.Contains(msg, "check_embedding_ctx_length=False") {
		t.Fatalf("token array input: %d %v", code, out)
	}
}

func TestEmbedBatchKeepsVectorsWithinTheValueBound(t *testing.T) {
	for _, c := range []struct {
		dims int64
		want int
	}{{0, 32}, {4096, 32}, {768, 64}, {2048, 64}, {3072, 42}} {
		if got := embedBatch(c.dims); got != c.want || int64(got)*max(c.dims, 1) > inference.MaxEmbedValues && c.dims > 0 {
			t.Fatalf("embedBatch(%d) = %d, want %d", c.dims, got, c.want)
		}
	}
}
