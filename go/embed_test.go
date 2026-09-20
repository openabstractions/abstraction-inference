package inference

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

// embeddingOf is the fake upstream's vector for input i: exact in float32.
func embeddingOf(i int) []float32 { return []float32{float32(i) + 0.5, -1.25, 3} }

// embeddings answers an OpenAI-compatible embeddings request with one vector
// per input, as numbers or as base64 float32, and two prompt tokens per input.
func (u *upstream) embeddings(w http.ResponseWriter, body []byte) {
	var req struct {
		Input []string `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	type item struct {
		Index     int `json:"index"`
		Embedding any `json:"embedding"`
	}
	data := []item{}
	// Reversed, so the provider must order by index.
	for i := len(req.Input) - 1; i >= 0; i-- {
		values := embeddingOf(i)
		if u.embedBase64 {
			packed := make([]byte, 4*len(values))
			for k, v := range values {
				binary.LittleEndian.PutUint32(packed[4*k:], math.Float32bits(v))
			}
			data = append(data, item{i, base64.StdEncoding.EncodeToString(packed)})
		} else {
			data = append(data, item{i, values})
		}
	}
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "usage": map[string]int{"prompt_tokens": 2 * len(req.Input), "total_tokens": 2 * len(req.Input)}})
}

func embedRequest(inputs ...string) wire.EmbedRequest {
	return wire.EmbedRequest{Model: "anthropic/claude-sonnet-5", Inputs: inputs, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeHostedAllowed}, Credential: "openrouter"}
}

func vectorValues(t *testing.T, vector string) []float32 {
	t.Helper()
	packed, err := base64.StdEncoding.DecodeString(vector)
	if err != nil || len(packed)%4 != 0 {
		t.Fatalf("vector %q: %v", vector, err)
	}
	out := make([]float32, len(packed)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(packed[4*i:]))
	}
	return out
}

// Vectors come back in input order, equal to the upstream's, as base64
// float32; the credential is applied once for consumer embed@1; the record
// names the embed profile; no reply or error carries the credential.
func TestEmbedReturnsTheUpstreamVectorsInInputOrder(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	surveyed := f.applier.count()
	e := f.provider.Embed(context.Background(), caller, embedRequest("alpha", "beta", "gamma"))
	if e.Outcome != wire.EmbedOutcomeCompleted || e.Dimensions != 3 || len(e.Vectors) != 3 || e.Usage.Input != 6 || e.Host != "openrouter" {
		t.Fatalf("embeddings %+v", e)
	}
	for i, v := range e.Vectors {
		got, want := vectorValues(t, v), embeddingOf(i)
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("vector %d: %v, want %v", i, got, want)
			}
		}
	}
	posts := f.up.posts()
	if len(posts) != 1 || posts[0].path != "/api/v1/embeddings" || f.applier.count() != surveyed+1 {
		t.Fatalf("posts %d, applier uses %d", len(posts), f.applier.count())
	}
	if use := f.applier.uses[surveyed]; !strings.Contains(use, " "+EmbedContract+" openrouter ") {
		t.Fatalf("applier use %q", use)
	}
	records := f.logged()
	if len(records) != 1 || records[0].Profile != router.ProfileEmbed || records[0].Outcome != "completed" || records[0].TokensIn != 6 {
		t.Fatalf("records %+v", records)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), secret) || strings.Contains(f.errs.String(), secret) {
		t.Fatal("the credential reached a reply or an error")
	}
}

// More inputs than the bound, an empty input and an out-of-range dimension
// are invalid before any credential use or upstream request.
func TestEmbedBoundsAreInvalidBeforeSpend(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	surveyed := f.applier.count()
	many := make([]string, MaxEmbedInputs+1)
	for i := range many {
		many[i] = "x"
	}
	tooWide := embedRequest("x")
	tooWide.Dimensions = MaxEmbedDimensions + 1
	for name, req := range map[string]wire.EmbedRequest{"inputs": embedRequest(many...), "empty": embedRequest(""), "dimensions": tooWide} {
		if e := f.provider.Embed(context.Background(), caller, req); e.Outcome != wire.EmbedOutcomeInvalid || len(e.Vectors) != 0 {
			t.Fatalf("%s: %+v", name, e)
		}
	}
	if len(f.up.posts()) != 0 || f.applier.count() != surveyed {
		t.Fatal("an invalid request was sent or spent a credential use")
	}
}

// An upstream answering base64 float32 and one answering number arrays yield
// the same vectors.
func TestEmbedBase64AndNumberEncodingsAreEqual(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	numbers := f.provider.Embed(context.Background(), caller, embedRequest("a", "b"))
	f.up.embedBase64 = true
	packed := f.provider.Embed(context.Background(), caller, embedRequest("a", "b"))
	if numbers.Outcome != wire.EmbedOutcomeCompleted || packed.Outcome != wire.EmbedOutcomeCompleted {
		t.Fatalf("%+v %+v", numbers, packed)
	}
	for i := range numbers.Vectors {
		if numbers.Vectors[i] != packed.Vectors[i] {
			t.Fatalf("vector %d differs: %s %s", i, numbers.Vectors[i], packed.Vectors[i])
		}
	}
}

// A ceiling counts requests: once the day's requests are spent the next call
// is budget_exceeded before the credential is applied or anything is sent.
func TestEmbedCeilingsCountRequests(t *testing.T) {
	ceilings, err := OpenCeilings(filepath.Join(t.TempDir(), "ceilings.json"), map[string]Ceiling{"openrouter": {RequestsPerDay: 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := setup(t, router.WireOpenAICompatible, func(c *Config) { c.Ceilings = ceilings })
	if e := f.provider.Embed(context.Background(), caller, embedRequest("a")); e.Outcome != wire.EmbedOutcomeCompleted {
		t.Fatalf("first call %+v", e)
	}
	uses := f.applier.count()
	if e := f.provider.Embed(context.Background(), caller, embedRequest("a")); e.Outcome != wire.EmbedOutcomeBudgetExceeded || e.Reason != "ceiling:requests:openrouter" {
		t.Fatalf("second call %+v", e)
	}
	if len(f.up.posts()) != 1 || f.applier.count() != uses {
		t.Fatal("a spent ceiling still sent the request or applied the credential")
	}
}

// A requested dimension the host does not honour reads unavailable, and a host
// without the embed profile is never picked.
func TestEmbedDimensionMismatchAndProfileSelection(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	req := embedRequest("a")
	req.Dimensions = 8
	if e := f.provider.Embed(context.Background(), caller, req); e.Outcome != wire.EmbedOutcomeUnavailable || e.Reason != "upstream:dimensions" || len(e.Vectors) != 0 {
		t.Fatalf("dimension mismatch %+v", e)
	}
	posts := len(f.up.posts())
	f.provider.cfg.Router.Hosts()[0].Profiles = []string{router.ProfileChat}
	if e := f.provider.Embed(context.Background(), caller, embedRequest("a")); e.Outcome != wire.EmbedOutcomeNoHost {
		t.Fatalf("embed on a chat-only host %+v", e)
	}
	if len(f.up.posts()) != posts {
		t.Fatal("a refused request was sent")
	}
}

// Without a rule the call is not permitted and nothing is sent.
func TestEmbedWithoutARuleIsNotPermitted(t *testing.T) {
	f := setup(t, router.WireOpenAICompatible, nil)
	surveyed := f.applier.count()
	other := Subject{Account: caller.Account, Program: `C:\apps\other.exe`}
	if e := f.provider.Embed(context.Background(), other, embedRequest("a")); e.Outcome != wire.EmbedOutcomeNotPermitted || e.Reason != "rights:not_granted" {
		t.Fatalf("%+v", e)
	}
	if len(f.up.posts()) != 0 || f.applier.count() != surveyed {
		t.Fatal("a refused call was sent")
	}
}
