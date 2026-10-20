package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

func TestHostAliasValidationComposesLegacyAndGenericPolicies(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, generic := range []bool{false, true} {
			cfg := Config{ValidateComfyBinding: func(context.Context, string, string) bool { return legacy }, ValidateHostBinding: func(_ context.Context, id, kind, base string) bool {
				if id != "binding" || kind != "comfyui" || base != "http://127.0.0.1:1234" {
					t.Fatal("generic binding tuple changed")
				}
				return generic
			}}
			if err := validateComfyRecipes(&cfg); err != nil {
				t.Fatal(err)
			}
			if got := cfg.ValidateComfyBinding(context.Background(), "binding", "http://127.0.0.1:1234"); got != (legacy && generic) {
				t.Fatalf("legacy=%v generic=%v got=%v", legacy, generic, got)
			}
		}
	}
	cfg := Config{ValidateHostBinding: func(_ context.Context, id, kind, base string) bool {
		return id == "binding" && kind == "comfyui" && base == "base"
	}}
	if err := validateComfyRecipes(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.ValidateComfyBinding(context.Background(), "binding", "base") || cfg.ValidateComfyBinding(context.Background(), "retired", "base") {
		t.Fatal("generic-only binding adapter")
	}
}

func TestHostAliasUnsupportedKindNeverProbesRecipeEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client()}}
	for _, kind := range []string{"unknown", "ollama", "swarmui", ""} {
		aliases, err := p.ReadyHostAliases(context.Background(), "binding", kind, server.URL)
		if err != nil || len(aliases) != 0 {
			t.Fatalf("%s: %v %v", kind, aliases, err)
		}
	}
	if calls != 0 {
		t.Fatal("unsupported kind probed recipe endpoint")
	}
}

func recipeFixture(t *testing.T) *ComfyRecipe {
	t.Helper()
	d := comfyRecipeData{Schema: "oa.comfy.recipe@1", ID: "fixture-v1", Variant: "fixture", ModelAlias: "fixture-model", ManifestSHA256: strings.Repeat("a", 64), Output: "save"}
	d.Template.Name = "fixture.json"
	d.Template.Revision = "revision"
	d.Template.URL = "https://example.invalid/fixture"
	d.Template.SHA256 = strings.Repeat("b", 64)
	d.Components = []comfyComponent{{"diffusion_model", "diffusion_models", "weights.bin", 1, strings.Repeat("c", 64), "org/repo", "revision", "weights.bin"}, {"text_encoder", "text_encoders", "encoder.bin", 2, strings.Repeat("d", 64), "org/repo", "revision", "encoder.bin"}, {"vae", "vae", "vae.bin", 3, strings.Repeat("e", 64), "org/repo", "revision", "vae.bin"}}
	d.Graph = map[string]comfyNode{
		"weights": {"UNETLoader", map[string]any{"unet_name": "weights.bin"}}, "encoder": {"CLIPLoader", map[string]any{"clip_name": "encoder.bin"}}, "vae": {"VAELoader", map[string]any{"vae_name": "vae.bin"}},
		"width": {"PrimitiveInt", map[string]any{"value": 512}}, "height": {"PrimitiveInt", map[string]any{"value": 512}}, "seed": {"RandomNoise", map[string]any{"noise_seed": 7}},
		"text":   {"CLIPTextEncode", map[string]any{"text": "preset", "clip": []any{"encoder", 0}}},
		"sample": {"Sampler", map[string]any{"model": []any{"weights", 0}, "text": []any{"text", 0}, "width": []any{"width", 0}, "height": []any{"height", 0}, "noise": []any{"seed", 0}}},
		"decode": {"VAEDecode", map[string]any{"samples": []any{"sample", 0}, "vae": []any{"vae", 0}}}, "save": {"SaveImage", map[string]any{"images": []any{"decode", 0}, "filename_prefix": "fixture"}}}
	d.Slots = map[string]comfySlot{"prompt": {"text", "text"}, "width": {"width", "value"}, "height": {"height", "value"}, "seed": {"seed", "noise_seed"}}
	d.Graph["weights"].Inputs["weight_dtype"] = "default"
	d.Graph["encoder"].Inputs["type"] = "flux2"
	d.Graph["encoder"].Inputs["device"] = "default"
	d.Graph["negative"] = comfyNode{"ConditioningZeroOut", map[string]any{"conditioning": []any{"text", 0}}}
	d.Graph["guider"] = comfyNode{"CFGGuider", map[string]any{"cfg": 1, "model": []any{"weights", 0}, "positive": []any{"text", 0}, "negative": []any{"negative", 0}}}
	d.Graph["sampler"] = comfyNode{"KSamplerSelect", map[string]any{"sampler_name": "euler"}}
	d.Graph["sigmas"] = comfyNode{"Flux2Scheduler", map[string]any{"steps": 4, "width": []any{"width", 0}, "height": []any{"height", 0}}}
	d.Graph["latent"] = comfyNode{"EmptyFlux2LatentImage", map[string]any{"batch_size": 1, "width": []any{"width", 0}, "height": []any{"height", 0}}}
	d.Graph["sample"] = comfyNode{"SamplerCustomAdvanced", map[string]any{"noise": []any{"seed", 0}, "guider": []any{"guider", 0}, "sampler": []any{"sampler", 0}, "sigmas": []any{"sigmas", 0}, "latent_image": []any{"latent", 0}}}
	d.Request.Kind = "generate"
	d.Request.Count = 1
	d.Request.Sizes = []string{"512x512"}
	d.GraphSHA256 = comfyHash(d.Graph)
	recipe, err := ParseComfyRecipe(comfyCanonical(d))
	if err != nil {
		t.Fatal(err)
	}
	return recipe
}

func TestComfyRecipeStrictSchemaAndGraph(t *testing.T) {
	r := recipeFixture(t)
	cases := map[string]func(*comfyRecipeData){
		"extra-role":     func(d *comfyRecipeData) { d.Components[2].Role = "text_encoder" },
		"path":           func(d *comfyRecipeData) { d.Components[0].Filename = "../weights.bin" },
		"loader":         func(d *comfyRecipeData) { d.Graph["weights"].Inputs["unet_name"] = "different.bin" },
		"slot-edge":      func(d *comfyRecipeData) { d.Slots["width"] = comfySlot{"sample", "width"} },
		"duplicate-slot": func(d *comfyRecipeData) { d.Slots["height"] = d.Slots["width"] },
		"missing-node":   func(d *comfyRecipeData) { delete(d.Graph, "vae") },
		"cycle":          func(d *comfyRecipeData) { d.Graph["encoder"].Inputs["loop"] = []any{"save", 0} },
		"unreachable": func(d *comfyRecipeData) {
			d.Graph["other"] = comfyNode{"SaveImage", map[string]any{"images": []any{"decode", 0}}}
		},
		"request": func(d *comfyRecipeData) { d.Request.Count = 2 },
		"custom-node": func(d *comfyRecipeData) {
			node := d.Graph["sample"]
			node.Class = "ExecuteAnything"
			d.Graph["sample"] = node
		},
		"output-traversal":  func(d *comfyRecipeData) { d.Graph["save"].Inputs["filename_prefix"] = "../escape" },
		"unknown-input":     func(d *comfyRecipeData) { d.Graph["sample"].Inputs["external_url"] = "https://example.invalid/payload" },
		"fixed-dimension":   func(d *comfyRecipeData) { d.Graph["latent"].Inputs["width"] = 512 },
		"swapped-dimension": func(d *comfyRecipeData) { d.Graph["sigmas"].Inputs["height"] = []any{"width", 0} },
		"zeroed-prompt-only": func(d *comfyRecipeData) {
			d.Graph["fixedtext"] = comfyNode{"CLIPTextEncode", map[string]any{"text": "fixed", "clip": []any{"encoder", 0}}}
			d.Graph["guider"].Inputs["positive"] = []any{"fixedtext", 0}
		},
		"different-noise": func(d *comfyRecipeData) {
			d.Graph["othernoise"] = comfyNode{"RandomNoise", map[string]any{"noise_seed": 8}}
			d.Graph["sample"].Inputs["noise"] = []any{"othernoise", 0}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			copy, err := ParseComfyRecipe(comfyCanonical(r.data))
			if err != nil {
				t.Fatal(err)
			}
			mutate(&copy.data)
			copy.data.GraphSHA256 = comfyHash(copy.data.Graph)
			if _, err := ParseComfyRecipe(comfyCanonical(copy.data)); err == nil {
				t.Fatal("invalid recipe accepted")
			}
		})
	}
	raw := string(comfyCanonical(r.data))
	for name, bad := range map[string]string{"duplicate": strings.Replace(raw, `"schema":`, `"schema":"oa.comfy.recipe@1","schema":`, 1), "unknown": strings.Replace(raw, `"schema":`, `"extra":1,"schema":`, 1), "trailing": raw + "{}"} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseComfyRecipe([]byte(bad)); err == nil {
				t.Fatal("malformed data accepted")
			}
		})
	}
}

func TestComfyRecipeNonSquareControlsAndReadySubset(t *testing.T) {
	recipe := recipeFixture(t)
	recipe.data.Request.Sizes = []string{"768x512"}
	recipe, err := ParseComfyRecipe(comfyCanonical(recipe.data))
	if err != nil {
		t.Fatal(err)
	}
	graph := recipe.workflow(wire.ImageRequest{Prompt: "actual prompt", Size: "768x512"})
	if graph["width"].(map[string]any)["inputs"].(map[string]any)["value"] != 768 || graph["height"].(map[string]any)["inputs"].(map[string]any)["value"] != 512 {
		t.Fatal("non-square controls lost")
	}
	other := recipeFixture(t)
	other.data.ID, other.data.ModelAlias = "other-v1", "other-model"
	other, err = ParseComfyRecipe(comfyCanonical(other.data))
	if err != nil {
		t.Fatal(err)
	}
	first, second := recipeManifest(recipe), recipeManifest(other)
	first["ready"] = false
	first["assets"].([]any)[0].(map[string]any)["state"] = "missing"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			_ = json.NewEncoder(w).Encode(first)
		} else {
			_ = json.NewEncoder(w).Encode(second)
		}
	}))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{{"binding", server.URL, "/first", recipe, false}, {"binding", server.URL, "/second", other, false}}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"missing", "present_unverified", "invalid"} {
		first["assets"].([]any)[0].(map[string]any)["state"] = state
		aliases, err := p.ReadyComfyRecipeAliases(context.Background(), "binding", server.URL)
		if err != nil || len(aliases) != 1 || aliases[0] != other.Alias() {
			t.Fatalf("ready subset: %v %v", aliases, err)
		}
	}
	for _, mutate := range []func(){func() { first["ready"] = true }, func() { first["ready"] = false; first["assets"].([]any)[0].(map[string]any)["state"] = "unknown" }, func() { first["assets"].([]any)[0].(map[string]any)["state"] = "ready" }, func() {
		first["assets"].([]any)[0].(map[string]any)["state"] = "missing"
		first["manifest_sha256"] = strings.Repeat("0", 64)
	}} {
		mutate()
		if _, err := p.ReadyComfyRecipeAliases(context.Background(), "binding", server.URL); err == nil {
			t.Fatal("malformed readiness accepted")
		}
	}
}

func TestComfyRecipeBindingCopiesAndRefusesAuthorityChanges(t *testing.T) {
	recipe := recipeFixture(t)
	binding := ComfyRecipeBinding{"binding", "http://127.0.0.1:1234", "/manifest", recipe, false}
	cfg := Config{ComfyRecipes: []ComfyRecipeBinding{binding}}
	if validateComfyRecipes(&cfg) == nil {
		t.Fatal("nil current-binding policy accepted")
	}
	cfg.ValidateComfyBinding = func(context.Context, string, string) bool { return true }
	if err := validateComfyRecipes(&cfg); err != nil {
		t.Fatal(err)
	}
	recipe.data.Graph["text"].Inputs["text"] = "mutated"
	if cfg.ComfyRecipes[0].Recipe.data.Graph["text"].Inputs["text"] != "preset" {
		t.Fatal("configured data not copied")
	}
	for _, route := range []string{"//other/manifest", "https://other/manifest", "/../manifest", "/manifest#fragment", "/manifest\\other", "/manifest?verify=1"} {
		copy := cfg
		copy.ComfyRecipes = append([]ComfyRecipeBinding(nil), cfg.ComfyRecipes...)
		copy.ComfyRecipes[0].ManifestPath = route
		if validateComfyRecipes(&copy) == nil {
			t.Fatalf("unsafe route %q", route)
		}
	}
	cfg.ComfyRecipes = append(cfg.ComfyRecipes, cfg.ComfyRecipes[0])
	if validateComfyRecipes(&cfg) == nil {
		t.Fatal("duplicate binding")
	}
}

func recipeManifest(r *ComfyRecipe) map[string]any {
	assets := []any{}
	for _, component := range r.data.Components {
		var row map[string]any
		_ = json.Unmarshal(comfyCanonical(component), &row)
		row["state"] = "ready"
		assets = append(assets, row)
	}
	return map[string]any{"id": r.ID(), "variant": r.Variant(), "manifest_sha256": r.ManifestSHA256(), "ready": true, "assets": assets, "detail": "verified by owning folder resolver"}
}

func TestComfyRecipeCurrentBindingManifestAndSelectedOutput(t *testing.T) {
	r := recipeFixture(t)
	posts := 0
	checks := 0
	current := true
	manifest := recipeManifest(r)
	var submitted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/prompt":
			posts++
			var body struct {
				Prompt map[string]any `json:"prompt"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			submitted = body.Prompt
			fmt.Fprint(w, `{"prompt_id":"one"}`)
		case "/history/one":
			fmt.Fprint(w, `{"one":{"status":{"completed":true},"outputs":{"preview":{"images":[{"Filename":"wrong.png"}]},"save":{"images":[{"Filename":"right.png"}]}}}}`)
		case "/view":
			if request.URL.Query().Get("filename") != "right.png" {
				t.Error("unselected output fetched")
			}
			_, _ = w.Write(pngOne)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	host := router.ComfyUI(server.URL)
	host.BindingID = "binding"
	p := &Provider{cfg: Config{HTTP: server.Client(), ComfyRecipes: []ComfyRecipeBinding{{"binding", server.URL, "/manifest", r, false}}, ValidateComfyBinding: func(context.Context, string, string) bool { checks++; return current }}}
	req := wire.ImageRequest{Mode: wire.ImageModeGenerate, Prompt: "requested", Size: "512x512", Count: 1}
	result := p.runBoundComfyImage(context.Background(), host, r.Alias(), req, imageTestBounds(), func(int64, string) {})
	if result.outcome != wire.ReplyOutcomeCompleted || len(result.images) != 1 || posts != 1 || checks != 2 {
		t.Fatalf("result=%+v posts=%d checks=%d", result, posts, checks)
	}
	text := submitted["text"].(map[string]any)["inputs"].(map[string]any)["text"]
	if text != "requested" || r.data.Graph["text"].Inputs["text"] != "preset" {
		t.Fatal("substitution mutated original")
	}
	for _, test := range []struct {
		name   string
		change func()
	}{
		{"disabled", func() { current = false }},
		{"redeclaration", func() { current = true; host.BindingID = "new" }},
		{"base", func() { host.BindingID = "binding"; host.Base = server.URL + "/other" }},
		{"not-ready", func() { host.Base = server.URL; manifest["ready"] = false }},
		{"wrong-variant", func() { manifest["ready"] = true; manifest["variant"] = "other" }},
		{"wrong-component", func() {
			manifest["variant"] = r.Variant()
			manifest["assets"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("f", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.change()
			out := p.runBoundComfyImage(context.Background(), host, r.Alias(), req, imageTestBounds(), func(int64, string) {})
			if out.outcome == wire.ReplyOutcomeCompleted || posts != 1 {
				t.Fatalf("bad admission %+v posts%d", out, posts)
			}
		})
	}
	manifest = recipeManifest(r)
	host.Base = server.URL
	current = true
	checks = 0
	p.cfg.ValidateComfyBinding = func(context.Context, string, string) bool { checks++; return checks == 1 }
	out := p.runBoundComfyImage(context.Background(), host, r.Alias(), req, imageTestBounds(), func(int64, string) {})
	if out.outcome == wire.ReplyOutcomeCompleted || posts != 1 {
		t.Fatal("binding changed after readiness was submitted")
	}
}

func TestComfyRecipeRefusesRequestShapesAndCallerGraphs(t *testing.T) {
	r := recipeFixture(t)
	req := wire.ImageRequest{Mode: wire.ImageModeGenerate, Prompt: "x", Size: "512x512", Count: 1}
	for _, change := range []func(*wire.ImageRequest){func(q *wire.ImageRequest) { q.Count = 2 }, func(q *wire.ImageRequest) { q.Size = "1024x1024" }, func(q *wire.ImageRequest) { q.Mode = wire.ImageModeEdit }, func(q *wire.ImageRequest) { q.Extensions = map[string]string{"comfyui/workflow": "{}"} }, func(q *wire.ImageRequest) { q.RequiredExtensions = []string{"comfyui/seed"} }} {
		copy := req
		change(&copy)
		if r.requestReason(copy) == "" {
			t.Fatal("unsupported request accepted")
		}
	}
}

func TestComfyManifestRedirectHasNoForeignEffect(t *testing.T) {
	foreign := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign++ }))
	defer other.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer source.Close()
	p := &Provider{cfg: Config{HTTP: source.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	if p.verifyComfyRecipe(context.Background(), &ComfyRecipeBinding{"binding", source.URL, "/manifest", recipeFixture(t), false}) == "" || foreign != 0 {
		t.Fatal("redirect crossed manifest authority")
	}
}

func TestComfyRecipeGeneratedData(t *testing.T) {
	filename := os.Getenv("OA_COMFY_RECIPE_DATA_FIXTURE")
	if filename == "" {
		t.Skip("set OA_COMFY_RECIPE_DATA_FIXTURE for the source-bound Python export composition check")
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := ParseComfyRecipe(raw)
	if err != nil {
		t.Fatal(err)
	}
	request := wire.ImageRequest{Mode: wire.ImageModeGenerate, Prompt: "composition fixture", Size: recipe.data.Request.Sizes[0], Count: 1}
	if reason := recipe.requestReason(request); reason != "" {
		t.Fatal(reason)
	}
	bound := recipe.workflow(request)
	if len(bound) != len(recipe.data.Graph) || recipe.Alias() == "" || recipe.ManifestSHA256() == "" {
		t.Fatal("generated recipe lost identity or graph")
	}
}

func TestComfyRecipeReplacementIsAtomicAndRetiresAliases(t *testing.T) {
	r := recipeFixture(t)
	p := &Provider{cfg: Config{ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	binding := ComfyRecipeBinding{"binding", "http://127.0.0.1:1234", "/manifest", r, false}
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{binding}); err != nil {
		t.Fatal(err)
	}
	host := router.ComfyUI(binding.ExpectedBase)
	host.BindingID = binding.BindingID
	admitted, reason := p.comfyRecipeBinding(host, r.Alias())
	if admitted == nil || reason != "" {
		t.Fatal("first replacement missing")
	}
	bad := binding
	bad.ManifestPath = "https://elsewhere/manifest"
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{binding, bad}); err == nil {
		t.Fatal("invalid whole replacement admitted")
	}
	if got, _ := p.comfyRecipeBinding(host, r.Alias()); got != admitted {
		t.Fatal("invalid replacement partially applied")
	}
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for n := 0; n < 50; n++ {
				_, _ = p.comfyRecipeBinding(host, r.Alias())
			}
		}()
	}
	for n := 0; n < 20; n++ {
		if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{binding}); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
	if err := p.ReplaceComfyRecipes(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.comfyRecipeBinding(host, r.Alias()); got != nil {
		t.Fatal("removed recipe remained bound")
	}
	if admitted.Recipe.Alias() != r.Alias() {
		t.Fatal("in-flight immutable binding changed")
	}
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{binding}); err != nil {
		t.Fatal(err)
	}
	host.BindingID = "replacement-generation"
	if _, why := p.comfyRecipeBinding(host, r.Alias()); why == "" {
		t.Fatal("redeclared host used stale recipe")
	}
}

func TestComfyCheckpointEligibilityRefusesRemovedRecipesAndParts(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models/checkpoints":
			fmt.Fprint(w, `["sub/checkpoint.bin"]`)
		case "/prompt":
			posts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			graph := body["prompt"].(map[string]any)
			name := graph["1"].(map[string]any)["inputs"].(map[string]any)["ckpt_name"]
			if name != "sub/checkpoint.bin" {
				t.Errorf("loader name %v", name)
			}
			fmt.Fprint(w, `{"prompt_id":"p"}`)
		case "/history/p":
			fmt.Fprint(w, `{"p":{"status":{"completed":true},"outputs":{"7":{"images":[{"Filename":"out.png"}]}}}}`)
		case "/view":
			_, _ = w.Write(pngOne)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	host := router.ComfyUI(server.URL)
	host.BindingID = "binding"
	recipe := recipeFixture(t)
	binding := ComfyRecipeBinding{"binding", server.URL, "/manifest", recipe, false}
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{binding}); err != nil {
		t.Fatal(err)
	}
	if err := p.ReplaceComfyRecipes(nil); err != nil {
		t.Fatal(err)
	}
	req := wire.ImageRequest{Mode: wire.ImageModeGenerate, Prompt: "x", Size: "512x512", Count: 1}
	for _, name := range []string{recipe.Alias(), "diffusion_models/part.bin", "text_encoders/encoder.bin", "vae/vae.bin"} {
		out := p.runBoundComfyImage(context.Background(), host, name, req, imageTestBounds(), func(int64, string) {})
		if out.outcome == wire.ReplyOutcomeCompleted || posts != 0 {
			t.Fatalf("noncheckpoint %s submitted", name)
		}
	}
	for _, name := range []string{"sub/checkpoint.bin", "checkpoints/sub/checkpoint.bin"} {
		out := p.runBoundComfyImage(context.Background(), host, name, req, imageTestBounds(), func(int64, string) {})
		if out.outcome != wire.ReplyOutcomeCompleted {
			t.Fatalf("eligible checkpoint refused %+v", out)
		}
	}
	if posts != 2 {
		t.Fatalf("posts%d", posts)
	}
}

func TestComfyRecipeThroughStartImageAndConstructorAliases(t *testing.T) {
	recipe := recipeFixture(t)
	posts := 0
	manifest := recipeManifest(recipe)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/abstraction/recipes":
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "oa.comfy.recipes@1", "recipes": []any{map[string]any{"recipe": recipe.data, "manifest_path": "/manifest"}}})
		case "/models":
			fmt.Fprint(w, `["checkpoints"]`)
		case "/system_stats":
			fmt.Fprint(w, `{"devices":[]}`)
		case "/models/checkpoints":
			fmt.Fprint(w, `[]`)
		case "/manifest":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/prompt":
			posts++
			var body struct {
				Prompt map[string]comfyNode `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Prompt["weights"].Class != "UNETLoader" || body.Prompt["text"].Inputs["text"] != "actual image caller" {
				t.Error("actual route lost recipe or prompt")
			}
			fmt.Fprint(w, `{"prompt_id":"native-entry"}`)
		case "/history/native-entry":
			fmt.Fprint(w, `{"native-entry":{"status":{"completed":true},"outputs":{"save":{"images":[{"Filename":"out.png"}]}}}}`)
		case "/view":
			_, _ = w.Write(pngOne)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var provider *Provider
	host := router.ComfyUI(server.URL, router.ComfyRecipeAliases(func(ctx context.Context) ([]string, error) {
		if provider == nil {
			return nil, nil
		}
		if err := provider.DiscoverComfyRecipes(ctx, "binding", server.URL); err != nil {
			return nil, err
		}
		return provider.ReadyComfyRecipeAliases(ctx, "binding", server.URL)
	}))
	host.BindingID = "binding"
	routes := router.New(host)
	store := &imageStore{}
	var err error
	provider, err = New(Config{Router: routes, HTTP: server.Client(), Decide: func(context.Context, Subject, string, string) (string, error) { return "permitted", nil }, Apply: func(context.Context, Subject, string, string, string) (map[string]string, string) {
		return nil, "applied"
	}, PrepareContentWrite: store.prepare, ValidateComfyBinding: func(context.Context, string, string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	routes.Survey()
	req := wire.ImageRequest{Model: recipe.Alias(), Mode: wire.ImageModeGenerate, Prompt: "actual image caller", Size: "512x512", Count: 1, Guarantees: []wire.RequestGuarantee{wire.RequestGuaranteeLocalOnly}}
	admission := provider.StartImage(context.Background(), caller, req)
	if admission.Outcome != wire.StartOutcomeAccepted {
		t.Fatalf("admission %+v", admission)
	}
	_, reply := drainImage(t, provider, caller, admission.Operation)
	if reply.Outcome != wire.ReplyOutcomeCompleted || posts != 1 {
		t.Fatalf("reply %+v posts%d", reply, posts)
	}
	if err := provider.ReplaceComfyRecipes(nil); err != nil {
		t.Fatal(err)
	}
	if next := provider.StartImage(context.Background(), caller, req); next.Outcome == wire.StartOutcomeAccepted || posts != 1 {
		t.Fatalf("cached removed alias accepted %+v", next)
	}
}

func TestComfyDiscoveryClosedInventoryAndAtomicRefresh(t *testing.T) {
	recipe := recipeFixture(t)
	entry := map[string]any{"recipe": recipe.data, "manifest_path": "/manifest"}
	inventory := map[string]any{"schema": "oa.comfy.recipes@1", "recipes": []any{entry}}
	current := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/abstraction/recipes" {
			t.Errorf("unexpected metadata fetch %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(inventory)
	}))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return current }}}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err != nil {
		t.Fatal(err)
	}
	host := router.ComfyUI(server.URL)
	host.BindingID = "binding"
	first, _ := p.comfyRecipeBinding(host, recipe.Alias())
	if first == nil {
		t.Fatal("discovery not attached")
	}
	for _, change := range []func(){func() { inventory["unexpected"] = true }, func() { delete(inventory, "unexpected"); inventory["recipes"] = []any{entry, entry} }, func() {
		inventory["recipes"] = []any{map[string]any{"recipe": recipe.data, "manifest_path": "/manifest?verify=1"}}
	}, func() { current = false }} {
		change()
		if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err == nil {
			t.Fatal("invalid inventory admitted")
		}
		if got, _ := p.comfyRecipeBinding(host, recipe.Alias()); got != first {
			t.Fatal("invalid inventory partially replaced snapshot")
		}
	}
	current = true
	inventory["recipes"] = []any{}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.comfyRecipeBinding(host, recipe.Alias()); got != nil {
		t.Fatal("removed inventory retained recipe")
	}
}

func TestComfyDiscoveryAbsentExtensionRetiresOnlyDiscoveredBinding(t *testing.T) {
	status := http.StatusOK
	recipe := recipeFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"schema": "oa.comfy.recipes@1", "recipes": []any{map[string]any{"recipe": recipe.data, "manifest_path": "/manifest"}}})
		}
	}))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	explicit := ComfyRecipeBinding{"explicit", server.URL, "/manifest", recipe, false}
	if err := p.ReplaceComfyRecipes([]ComfyRecipeBinding{explicit}); err != nil {
		t.Fatal(err)
	}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		status = failure
		if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err == nil || len(p.comfyBindings()) != 2 {
			t.Fatal("failure erased inventory or was swallowed")
		}
	}
	status = http.StatusNotFound
	if err := p.DiscoverComfyRecipes(context.Background(), "other", server.URL); err != nil || len(p.comfyBindings()) != 2 {
		t.Fatal("404 erased another binding")
	}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err != nil {
		t.Fatal(err)
	}
	if got := p.comfyBindings(); len(got) != 1 || got[0].BindingID != "explicit" {
		t.Fatal("404 failed scoped retirement or erased explicit configuration")
	}
}

func TestComfyDiscoveryRedirectCannotFetchOtherHost(t *testing.T) {
	foreign := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign++ }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err == nil || foreign != 0 {
		t.Fatal("inventory redirected authority")
	}
}

func TestComfyGeneratedDiscoveryAndUnverifiedReadiness(t *testing.T) {
	inventoryFile := os.Getenv("OA_COMFY_RECIPE_INVENTORY_FIXTURE")
	readinessFile := os.Getenv("OA_COMFY_RECIPE_READINESS_FIXTURE")
	if inventoryFile == "" || readinessFile == "" {
		t.Skip("set source-bound Python inventory/readiness fixtures")
	}
	inventory, err := os.ReadFile(inventoryFile)
	if err != nil {
		t.Fatal(err)
	}
	readiness, err := os.ReadFile(readinessFile)
	if err != nil {
		t.Fatal(err)
	}
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		if r.URL.Path == "/abstraction/recipes" {
			_, _ = w.Write(inventory)
			return
		}
		if r.URL.Query().Has("verify") {
			t.Error("readonly receiver requested model hashing")
		}
		_, _ = w.Write(readiness)
	}))
	defer server.Close()
	p := &Provider{cfg: Config{HTTP: server.Client(), ValidateComfyBinding: func(context.Context, string, string) bool { return true }}}
	if err := p.DiscoverComfyRecipes(context.Background(), "binding", server.URL); err != nil {
		t.Fatal(err)
	}
	if len(p.comfyBindings()) == 0 {
		t.Fatal("generated inventory empty")
	}
	if aliases, err := p.ReadyComfyRecipeAliases(context.Background(), "binding", server.URL); err == nil || len(aliases) > 0 {
		t.Fatal("synthetic unverified sample became ready")
	}
	for _, request := range requests {
		if request == "/prompt" {
			t.Fatal("unverified sample executed")
		}
	}
}
