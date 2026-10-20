package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	router "github.com/openabstractions/abstraction-router/go"
)

const comfyRecipeLimit = 1 << 20

// ComfyRecipeBinding attaches admitted provider data to one current local
// declaration. ManifestPath names only that host's readonly extension route.
type ComfyRecipeBinding struct {
	BindingID    string
	ExpectedBase string
	ManifestPath string
	Recipe       *ComfyRecipe
	discovered   bool
}

// ComfyRecipe is immutable provider-private data, constructed only by parsing.
// No graph, component location or endpoint is accepted from an image request.
type ComfyRecipe struct{ data comfyRecipeData }
type comfyRecipeSet struct {
	bindings []ComfyRecipeBinding
}

// ReplaceComfyRecipes is the native composition owner's refresh seam. Validate
// and copy the entire replacement before publishing one immutable snapshot.
// Operations already admitted retain their snapshot and still recheck current
// declaration authority immediately before submission.
func (p *Provider) ReplaceComfyRecipes(bindings []ComfyRecipeBinding) error {
	cfg := Config{ComfyRecipes: bindings, ValidateComfyBinding: p.cfg.ValidateComfyBinding}
	if err := validateComfyRecipes(&cfg); err != nil {
		return err
	}
	p.comfyRecipes.Store(&comfyRecipeSet{bindings: cfg.ComfyRecipes})
	return nil
}

func (p *Provider) comfyBindings() []ComfyRecipeBinding {
	if snapshot := p.comfyRecipes.Load(); snapshot != nil {
		return snapshot.bindings
	}
	return p.cfg.ComfyRecipes
}

func (r *ComfyRecipe) Alias() string          { return r.data.ModelAlias }
func (r *ComfyRecipe) ID() string             { return r.data.ID }
func (r *ComfyRecipe) Variant() string        { return r.data.Variant }
func (r *ComfyRecipe) ManifestSHA256() string { return r.data.ManifestSHA256 }

type comfyComponent struct {
	Role       string `json:"role"`
	Folder     string `json:"folder"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
	SourcePath string `json:"source_path"`
}
type comfySlot struct {
	Node  string `json:"node"`
	Input string `json:"input"`
}
type comfyNode struct {
	Class  string         `json:"class_type"`
	Inputs map[string]any `json:"inputs"`
}
type comfyRecipeData struct {
	Schema     string `json:"schema"`
	ID         string `json:"id"`
	Variant    string `json:"variant"`
	ModelAlias string `json:"model_alias"`
	Template   struct {
		Name     string `json:"name"`
		Revision string `json:"revision"`
		URL      string `json:"url"`
		SHA256   string `json:"sha256"`
	} `json:"template"`
	ManifestSHA256 string               `json:"manifest_sha256"`
	GraphSHA256    string               `json:"graph_sha256"`
	Components     []comfyComponent     `json:"components"`
	Graph          map[string]comfyNode `json:"graph"`
	Slots          map[string]comfySlot `json:"slots"`
	Output         string               `json:"output_node"`
	Request        struct {
		Kind  string   `json:"kind"`
		Count int64    `json:"count"`
		Sizes []string `json:"sizes"`
	} `json:"request"`
}

// strictComfyJSON rejects duplicates at every nesting level before typed decode.
func strictComfyJSON(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > comfyRecipeLimit || !utf8.Valid(raw) {
		return errors.New("comfy recipe: data bound")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("comfy recipe: depth")
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delimiter == '{' {
			keys := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("comfy recipe: duplicate key")
				}
				keys[name] = true
				if err = walk(depth + 1); err != nil {
					return err
				}
			}
		} else if delimiter == '[' {
			for dec.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
		} else {
			return errors.New("comfy recipe: delimiter")
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("comfy recipe: trailing data")
	}
	dec = json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	return dec.Decode(target)
}
func comfyCanonical(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
}
func comfyHash(v any) string {
	sum := sha256.Sum256(comfyCanonical(v))
	return hex.EncodeToString(sum[:])
}
func comfyDigest(v string) bool {
	raw, err := hex.DecodeString(v)
	return err == nil && len(raw) == 32 && v == strings.ToLower(v)
}
func comfyName(v string) bool { return len(v) > 0 && len(v) <= 256 && nameChars(v, "_-.") }
func comfyRelative(v string) bool {
	return v != "" && len(v) <= 1024 && !strings.ContainsAny(v, "\\\x00?#") && !strings.HasPrefix(v, "/") && path.Clean(v) == v && v != "." && v != ".." && !strings.HasPrefix(v, "../") && !strings.Contains(v, ":")
}

// ParseComfyRecipe validates the closed generated schema and graph once. The
// configured host/base supplies admission authority; hashes identify the snapshot.
func ParseComfyRecipe(raw []byte) (*ComfyRecipe, error) {
	var d comfyRecipeData
	if err := strictComfyJSON(raw, &d); err != nil {
		return nil, err
	}
	fail := func() (*ComfyRecipe, error) { return nil, errors.New("comfy recipe: invalid declaration") }
	if d.Schema != "oa.comfy.recipe@1" || !comfyName(d.ID) || !comfyName(d.Variant) || !comfyName(d.ModelAlias) || !comfyDigest(d.ManifestSHA256) || !comfyDigest(d.GraphSHA256) || !comfyDigest(d.Template.SHA256) || !comfyName(d.Template.Name) || !comfyName(d.Template.Revision) {
		return fail()
	}
	u, err := url.Parse(d.Template.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || len(d.Template.URL) > 2048 {
		return fail()
	}
	if d.Request.Kind != "generate" || d.Request.Count != 1 || len(d.Request.Sizes) == 0 || len(d.Request.Sizes) > 16 {
		return fail()
	}
	sizes := map[string]bool{}
	for _, size := range d.Request.Sizes {
		w, h, ok := imageDimensions(size)
		if !ok || w == 0 || h == 0 || sizes[size] {
			return fail()
		}
		sizes[size] = true
	}
	if len(d.Graph) == 0 || len(d.Graph) > 128 || comfyHash(d.Graph) != d.GraphSHA256 || len(d.Components) != 3 || len(d.Slots) != 4 {
		return fail()
	}
	roles := map[string]struct{ folder, class, input string }{"diffusion_model": {"diffusion_models", "UNETLoader", "unet_name"}, "text_encoder": {"text_encoders", "CLIPLoader", "clip_name"}, "vae": {"vae", "VAELoader", "vae_name"}}
	seen := map[string]bool{}
	for _, component := range d.Components {
		shape, ok := roles[component.Role]
		if !ok || seen[component.Role] || component.Folder != shape.folder || !comfyRelative(component.Filename) || component.Size <= 0 || component.Size > 1<<40 || !comfyDigest(component.SHA256) || !comfyRelative(component.Repository) || !comfyName(component.Revision) || !comfyRelative(component.SourcePath) {
			return fail()
		}
		seen[component.Role] = true
		count := 0
		for _, node := range d.Graph {
			if node.Class == shape.class {
				count++
				if node.Inputs[shape.input] != component.Filename {
					return fail()
				}
			}
		}
		if count != 1 {
			return fail()
		}
	}
	if d.Graph[d.Output].Class != "SaveImage" {
		return fail()
	}
	targets := map[string]bool{}
	for _, key := range []string{"prompt", "width", "height", "seed"} {
		slot, ok := d.Slots[key]
		node, present := d.Graph[slot.Node]
		value, exists := node.Inputs[slot.Input]
		target := slot.Node + "/" + slot.Input
		if !ok || !present || !exists || !comfyName(slot.Input) || targets[target] {
			return fail()
		}
		targets[target] = true
		if key == "prompt" {
			if _, ok := value.(string); !ok || node.Class != "CLIPTextEncode" || slot.Input != "text" {
				return fail()
			}
		} else {
			if key == "seed" && (node.Class != "RandomNoise" || slot.Input != "noise_seed") || key != "seed" && (node.Class != "PrimitiveInt" || slot.Input != "value") {
				return fail()
			}
			number, ok := value.(json.Number)
			if !ok {
				return fail()
			}
			n, err := strconv.ParseInt(number.String(), 10, 64)
			if err != nil || n < 0 || key != "seed" && (n < 64 || n > 4096 || n%8 != 0) {
				return fail()
			}
		}
	}
	// Every edge names a real node and a bounded output index; no cycles or
	// unreachable effect nodes are admitted alongside the selected output.
	edges := map[string][]string{}
	for id, node := range d.Graph {
		if !comfyName(id) || !comfyName(node.Class) || len(node.Inputs) > 64 {
			return fail()
		}
		for key, value := range node.Inputs {
			if !comfyName(key) {
				return fail()
			}
			switch v := value.(type) {
			case []any:
				if len(v) != 2 {
					return fail()
				}
				from, ok := v[0].(string)
				index, valid := v[1].(json.Number)
				if !ok || !valid {
					return fail()
				}
				n, err := strconv.Atoi(index.String())
				if _, exists := d.Graph[from]; !exists || err != nil || n < 0 || n > 63 {
					return fail()
				}
				edges[id] = append(edges[id], from)
			case string:
				if len(v) > 32768 {
					return fail()
				}
			case json.Number:
				if len(v.String()) > 32 {
					return fail()
				}
			case bool:
			default:
				return fail()
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return false
		}
		if visited[id] {
			return true
		}
		visiting[id] = true
		for _, from := range edges[id] {
			if !visit(from) {
				return false
			}
		}
		visiting[id] = false
		visited[id] = true
		return true
	}
	if !visit(d.Output) || len(visited) != len(d.Graph) {
		return fail()
	}
	if !validComfySemantics(d.Graph, d.Output) {
		return fail()
	}
	// Request controls must feed the executing sampler, including its positive
	// conditioning. Merely reaching the output through a zeroed branch is insufficient.
	for _, node := range d.Graph {
		controls := map[string]string{}
		switch node.Class {
		case "CFGGuider":
			controls["positive"] = "prompt"
		case "Flux2Scheduler", "EmptyFlux2LatentImage":
			controls["width"], controls["height"] = "width", "height"
		case "SamplerCustomAdvanced":
			controls["noise"] = "seed"
		}
		for input, control := range controls {
			edge, ok := node.Inputs[input].([]any)
			if !ok || len(edge) != 2 || edge[0] != d.Slots[control].Node {
				return fail()
			}
		}
	}
	return &ComfyRecipe{data: d}, nil
}

// This finite protocol subset admits stock loader/sampler/output operations.
// A host cannot turn recipe discovery into custom-node or filesystem execution.
func validComfySemantics(graph map[string]comfyNode, output string) bool {
	integer := func(v any, low, high int64) bool {
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		value, err := n.Int64()
		return err == nil && value >= low && value <= high
	}
	link := func(v any, classes ...string) bool {
		edge, ok := v.([]any)
		if !ok || len(edge) != 2 {
			return false
		}
		id, ok := edge[0].(string)
		if !ok || !integer(edge[1], 0, 0) {
			return false
		}
		return slices.Contains(classes, graph[id].Class)
	}
	text := func(v any, allowed ...string) bool {
		value, ok := v.(string)
		return ok && slices.Contains(allowed, value)
	}
	dimension := func(v any) bool { return integer(v, 64, 4096) || link(v, "PrimitiveInt") }
	for id, node := range graph {
		fields := map[string]func(any) bool{}
		switch node.Class {
		case "UNETLoader":
			fields = map[string]func(any) bool{"unet_name": func(v any) bool { s, ok := v.(string); return ok && comfyRelative(s) }, "weight_dtype": func(v any) bool { return text(v, "default") }}
		case "CLIPLoader":
			fields = map[string]func(any) bool{"clip_name": func(v any) bool { s, ok := v.(string); return ok && comfyRelative(s) }, "type": func(v any) bool { return text(v, "flux2") }, "device": func(v any) bool { return text(v, "default", "cpu") }}
		case "VAELoader":
			fields = map[string]func(any) bool{"vae_name": func(v any) bool { s, ok := v.(string); return ok && comfyRelative(s) }}
		case "PrimitiveInt":
			fields = map[string]func(any) bool{"value": func(v any) bool { return integer(v, 64, 4096) }}
		case "RandomNoise":
			fields = map[string]func(any) bool{"noise_seed": func(v any) bool { return integer(v, 0, 1<<63-1) }}
		case "KSamplerSelect":
			fields = map[string]func(any) bool{"sampler_name": func(v any) bool { return text(v, "euler") }}
		case "CLIPTextEncode":
			fields = map[string]func(any) bool{"text": func(v any) bool { s, ok := v.(string); return ok && len(s) <= 32768 }, "clip": func(v any) bool { return link(v, "CLIPLoader") }}
		case "ConditioningZeroOut":
			fields = map[string]func(any) bool{"conditioning": func(v any) bool { return link(v, "CLIPTextEncode") }}
		case "CFGGuider":
			fields = map[string]func(any) bool{"cfg": func(v any) bool {
				n, ok := v.(json.Number)
				if !ok {
					return false
				}
				f, e := n.Float64()
				return e == nil && f > 0 && f <= 30
			}, "model": func(v any) bool { return link(v, "UNETLoader") }, "positive": func(v any) bool { return link(v, "CLIPTextEncode") }, "negative": func(v any) bool { return link(v, "ConditioningZeroOut") }}
		case "Flux2Scheduler":
			fields = map[string]func(any) bool{"steps": func(v any) bool { return integer(v, 1, 150) }, "width": dimension, "height": dimension}
		case "EmptyFlux2LatentImage":
			fields = map[string]func(any) bool{"batch_size": func(v any) bool { return integer(v, 1, 1) }, "width": dimension, "height": dimension}
		case "SamplerCustomAdvanced":
			fields = map[string]func(any) bool{"noise": func(v any) bool { return link(v, "RandomNoise") }, "guider": func(v any) bool { return link(v, "CFGGuider") }, "sampler": func(v any) bool { return link(v, "KSamplerSelect") }, "sigmas": func(v any) bool { return link(v, "Flux2Scheduler") }, "latent_image": func(v any) bool { return link(v, "EmptyFlux2LatentImage") }}
		case "VAEDecode":
			fields = map[string]func(any) bool{"samples": func(v any) bool { return link(v, "SamplerCustomAdvanced") }, "vae": func(v any) bool { return link(v, "VAELoader") }}
		case "SaveImage":
			if id != output {
				return false
			}
			fields = map[string]func(any) bool{"images": func(v any) bool { return link(v, "VAEDecode") }, "filename_prefix": func(v any) bool { s, ok := v.(string); return ok && s != "" && len(s) <= 128 && nameChars(s, "_-") }}
		default:
			return false
		}
		if len(fields) != len(node.Inputs) {
			return false
		}
		for key, check := range fields {
			value, exists := node.Inputs[key]
			if !exists || !check(value) {
				return false
			}
		}
	}
	return true
}

func validComfyManifestPath(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Fragment != "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || !comfyRelative(strings.TrimPrefix(u.Path, "/")) || len(value) > 2048 {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || query.Has("verify") {
		return false
	}
	for key, values := range query {
		if !comfyName(key) || len(values) != 1 || len(values[0]) > 256 {
			return false
		}
	}
	return true
}

func validateComfyRecipes(cfg *Config) error {
	if cfg.ValidateHostBinding != nil {
		validate := cfg.ValidateHostBinding
		legacy := cfg.ValidateComfyBinding
		cfg.ValidateComfyBinding = func(ctx context.Context, bindingID, base string) bool {
			return (legacy == nil || legacy(ctx, bindingID, base)) && validate(ctx, bindingID, "comfyui", base)
		}
	}
	if len(cfg.ComfyRecipes) > 64 || len(cfg.ComfyRecipes) > 0 && cfg.ValidateComfyBinding == nil {
		return errors.New("inference: current Comfy binding validation required")
	}
	bindings := append([]ComfyRecipeBinding(nil), cfg.ComfyRecipes...)
	seen := map[string]bool{}
	for i, b := range bindings {
		base, err := url.Parse(b.ExpectedBase)
		if b.BindingID == "" || len(b.BindingID) > 512 || base == nil || err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.TrimRight(base.Path, "/") != "" || !validComfyManifestPath(b.ManifestPath) || b.Recipe == nil {
			return errors.New("inference: invalid Comfy recipe binding")
		}
		recipe, err := ParseComfyRecipe(comfyCanonical(b.Recipe.data))
		if err != nil {
			return err
		}
		b.Recipe = recipe
		key := b.BindingID + "\n" + recipe.Alias()
		if seen[key] {
			return errors.New("inference: duplicate Comfy recipe binding")
		}
		seen[key] = true
		bindings[i] = b
	}
	cfg.ComfyRecipes = bindings
	return nil
}

func (p *Provider) comfyRecipeBinding(host *router.Host, model string) (*ComfyRecipeBinding, string) {
	known := false
	bindings := p.comfyBindings()
	for i := range bindings {
		b := &bindings[i]
		if b.Recipe.Alias() != model {
			continue
		}
		known = true
		if host.Name == "comfyui" && !host.Hosted && host.BindingID == b.BindingID && host.Base == b.ExpectedBase {
			return b, ""
		}
	}
	if known {
		return nil, "comfyui:binding"
	}
	return nil, ""
}

// ReadyHostAliases composes discovery and readiness at the protocol owner.
// Other kinds have no recipe discovery and never probe this adapter's route.
func (p *Provider) ReadyHostAliases(ctx context.Context, bindingID, kind, base string) ([]string, error) {
	if kind != "comfyui" {
		return nil, nil
	}
	if err := p.DiscoverComfyRecipes(ctx, bindingID, base); err != nil {
		return nil, err
	}
	return p.ReadyComfyRecipeAliases(ctx, bindingID, base)
}

// ReadyComfyRecipeAliases supplies only currently verified aliases to native
// router composition. Recipe admission and manifest checks have one owner.
func (p *Provider) ReadyComfyRecipeAliases(ctx context.Context, bindingID, expectedBase string) ([]string, error) {
	if p.cfg.ValidateComfyBinding != nil && !p.cfg.ValidateComfyBinding(ctx, bindingID, expectedBase) {
		return nil, errors.New("comfyui:binding")
	}
	var aliases []string
	for _, binding := range p.comfyBindings() {
		if binding.BindingID != bindingID || binding.ExpectedBase != expectedBase {
			continue
		}
		if reason := p.verifyComfyRecipe(ctx, &binding); reason != "" {
			if reason == "comfyui:not-ready" {
				continue
			}
			return nil, errors.New(reason)
		}
		aliases = append(aliases, binding.Recipe.Alias())
	}
	return aliases, nil
}

// DiscoverComfyRecipes reads the current admitted host's provider inventory.
// That host/base is the authority; hashes identify immutable recipe snapshots.
// Discovery and readiness never initiate model hashing or acquisition.
func (p *Provider) DiscoverComfyRecipes(ctx context.Context, bindingID, expectedBase string) error {
	if bindingID == "" || len(bindingID) > 512 || p.cfg.ValidateComfyBinding == nil || !p.cfg.ValidateComfyBinding(ctx, bindingID, expectedBase) {
		return errors.New("comfyui:binding")
	}
	base, err := url.Parse(expectedBase)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || strings.TrimRight(base.Path, "/") != "" {
		return errors.New("comfyui:binding")
	}
	client := *p.cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(expectedBase, "/")+"/abstraction/recipes", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return p.replaceDiscoveredComfyRecipes(ctx, bindingID, expectedBase, nil)
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("comfyui:inventory-unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, comfyRecipeLimit+1))
	if err != nil || len(raw) > comfyRecipeLimit {
		return errors.New("comfyui:inventory-bound")
	}
	var inventory struct {
		Schema  string `json:"schema"`
		Recipes []struct {
			Recipe       json.RawMessage `json:"recipe"`
			ManifestPath string          `json:"manifest_path"`
		} `json:"recipes"`
	}
	if err := strictComfyJSON(raw, &inventory); err != nil {
		return err
	}
	if inventory.Schema != "oa.comfy.recipes@1" || inventory.Recipes == nil || len(inventory.Recipes) > 64 {
		return errors.New("comfyui:inventory")
	}
	bindings := make([]ComfyRecipeBinding, 0, len(inventory.Recipes))
	aliases, paths := map[string]bool{}, map[string]bool{}
	for _, entry := range inventory.Recipes {
		recipe, err := ParseComfyRecipe(entry.Recipe)
		if err != nil {
			return err
		}
		if !validComfyManifestPath(entry.ManifestPath) {
			return errors.New("comfyui:manifest-path")
		}
		u, _ := url.Parse(entry.ManifestPath)
		canonicalPath := u.Path + "?" + u.Query().Encode()
		if aliases[recipe.Alias()] || paths[canonicalPath] {
			return errors.New("comfyui:duplicate-inventory")
		}
		aliases[recipe.Alias()] = true
		paths[canonicalPath] = true
		bindings = append(bindings, ComfyRecipeBinding{bindingID, expectedBase, entry.ManifestPath, recipe, true})
	}
	return p.replaceDiscoveredComfyRecipes(ctx, bindingID, expectedBase, bindings)
}

func (p *Provider) replaceDiscoveredComfyRecipes(ctx context.Context, bindingID, expectedBase string, bindings []ComfyRecipeBinding) error {
	if !p.cfg.ValidateComfyBinding(ctx, bindingID, expectedBase) {
		return errors.New("comfyui:binding")
	}
	for {
		previous := p.comfyRecipes.Load()
		old := p.cfg.ComfyRecipes
		if previous != nil {
			old = previous.bindings
		}
		merged := append([]ComfyRecipeBinding(nil), bindings...)
		for _, binding := range old {
			if !binding.discovered || binding.BindingID != bindingID || binding.ExpectedBase != expectedBase {
				merged = append(merged, binding)
			}
		}
		cfg := Config{ComfyRecipes: merged, ValidateComfyBinding: p.cfg.ValidateComfyBinding}
		if err := validateComfyRecipes(&cfg); err != nil {
			return err
		}
		if p.comfyRecipes.CompareAndSwap(previous, &comfyRecipeSet{bindings: cfg.ComfyRecipes}) {
			return nil
		}
	}
}

// verifyComfyCheckpoint proves actual loader eligibility before the compatible
// checkpoint branch. A removed recipe alias in a router cache cannot bypass it.
func (p *Provider) verifyComfyCheckpoint(ctx context.Context, host *router.Host, model string) string {
	if p.cfg.ValidateComfyBinding != nil && !p.cfg.ValidateComfyBinding(ctx, host.BindingID, host.Base) {
		return "comfyui:binding"
	}
	filename := strings.TrimPrefix(model, "checkpoints/")
	if !comfyRelative(filename) {
		return "comfyui:checkpoint"
	}
	client := *p.cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(host.Base, "/")+"/models/checkpoints", nil)
	if err != nil {
		return "comfyui:checkpoint"
	}
	response, err := client.Do(request)
	if err != nil {
		return "comfyui:checkpoint-unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "comfyui:checkpoint-unavailable"
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, comfyRecipeLimit+1))
	if err != nil || len(raw) > comfyRecipeLimit {
		return "comfyui:checkpoint-unavailable"
	}
	var names []string
	if err := strictComfyJSON(raw, &names); err != nil || len(names) > 16384 {
		return "comfyui:checkpoint"
	}
	if !slices.Contains(names, filename) {
		return "comfyui:recipe-required"
	}
	return ""
}
func (r *ComfyRecipe) requestReason(req wire.ImageRequest) string {
	size := req.Size
	if size == "auto" {
		size = "512x512"
	}
	if req.Mode != wire.ImageModeGenerate || req.Count != r.data.Request.Count || !slices.Contains(r.data.Request.Sizes, size) {
		return "comfyui:request"
	}
	// This schema fixes sampler, guidance, steps and seed in the provider recipe.
	if len(req.RequiredExtensions) > 0 || len(req.Extensions) > 0 {
		return "comfyui:controls"
	}
	return ""
}
func (r *ComfyRecipe) workflow(req wire.ImageRequest) map[string]any {
	var graph map[string]any
	decoder := json.NewDecoder(bytes.NewReader(comfyCanonical(r.data.Graph)))
	decoder.UseNumber()
	_ = decoder.Decode(&graph)
	width, height, _ := imageDimensions(req.Size)
	if width == 0 {
		width, height = 512, 512
	}
	for key, value := range map[string]any{"prompt": req.Prompt, "width": width, "height": height} {
		slot := r.data.Slots[key]
		graph[slot.Node].(map[string]any)["inputs"].(map[string]any)[slot.Input] = value
	}
	return graph
}

func (p *Provider) verifyComfyRecipe(ctx context.Context, b *ComfyRecipeBinding) string {
	if !p.cfg.ValidateComfyBinding(ctx, b.BindingID, b.ExpectedBase) {
		return "comfyui:binding"
	}
	// A copy prevents the supplier manifest from redirecting this same-host read.
	client := *p.cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.ExpectedBase+b.ManifestPath, nil)
	if err != nil {
		return "comfyui:manifest"
	}
	response, err := client.Do(request)
	if err != nil {
		return "comfyui:manifest-unavailable"
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "comfyui:manifest-unavailable"
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, comfyRecipeLimit+1))
	if err != nil || len(raw) > comfyRecipeLimit {
		return "comfyui:manifest-unavailable"
	}
	var manifest struct {
		ID             string `json:"id"`
		Variant        string `json:"variant"`
		ManifestSHA256 string `json:"manifest_sha256"`
		Ready          *bool  `json:"ready"`
		Assets         []struct {
			comfyComponent
			State string `json:"state"`
		} `json:"assets"`
	}
	// Readiness has display/template/total fields outside this projection. The
	// duplicate/depth guard still applies; pins are compared explicitly below.
	var object map[string]json.RawMessage
	if err := strictComfyJSON(raw, &object); err != nil {
		return "comfyui:manifest"
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return "comfyui:manifest"
	}
	if manifest.ID != b.Recipe.ID() || manifest.Variant != b.Recipe.Variant() || manifest.ManifestSHA256 != b.Recipe.ManifestSHA256() || len(manifest.Assets) != 3 {
		return "comfyui:manifest"
	}
	seen := map[string]bool{}
	allReady := true
	for _, asset := range manifest.Assets {
		if !slices.Contains([]string{"ready", "missing", "present_unverified", "invalid"}, asset.State) || seen[asset.Role] {
			return "comfyui:components"
		}
		allReady = allReady && asset.State == "ready"
		seen[asset.Role] = true
		found := false
		for _, expected := range b.Recipe.data.Components {
			if asset.comfyComponent == expected {
				found = true
			}
		}
		if !found {
			return "comfyui:components"
		}
	}
	if manifest.Ready == nil || *manifest.Ready != allReady {
		return "comfyui:manifest"
	}
	if !allReady {
		return "comfyui:not-ready"
	}
	return ""
}

func (p *Provider) runBoundComfyImage(ctx context.Context, host *router.Host, model string, req wire.ImageRequest, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	b, reason := p.comfyRecipeBinding(host, model)
	if reason != "" {
		return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, reason)
	}
	return p.runAdmittedComfyImage(ctx, host, model, req, b, bounds, progress)
}

func (p *Provider) runAdmittedComfyImage(ctx context.Context, host *router.Host, model string, req wire.ImageRequest, b *ComfyRecipeBinding, bounds imageOutputBounds, progress func(int64, string)) imageBackendReply {
	if b == nil {
		if reason := p.verifyComfyCheckpoint(ctx, host, model); reason != "" {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, reason)
		}
		if p.cfg.ValidateComfyBinding != nil && !p.cfg.ValidateComfyBinding(ctx, host.BindingID, host.Base) {
			return imageBackendFailure(wire.ReplyOutcomeUnavailable, "comfyui:binding")
		}
		return runComfyImage(ctx, p.cfg.HTTP, host, strings.TrimPrefix(model, "checkpoints/"), req, bounds, progress)
	}
	if host.BindingID != b.BindingID || host.Base != b.ExpectedBase || model != b.Recipe.Alias() {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "comfyui:binding")
	}
	var reason string
	if reason = b.Recipe.requestReason(req); reason != "" {
		return imageBackendFailure(wire.ReplyOutcomeUnsupportedFeature, reason)
	}
	if reason = p.verifyComfyRecipe(ctx, b); reason != "" {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, reason)
	}
	if !p.cfg.ValidateComfyBinding(ctx, b.BindingID, b.ExpectedBase) {
		return imageBackendFailure(wire.ReplyOutcomeUnavailable, "comfyui:binding")
	}
	return runComfyGraph(ctx, p.cfg.HTTP, host, req, b.Recipe.workflow(req), b.Recipe.data.Output, bounds, progress)
}
