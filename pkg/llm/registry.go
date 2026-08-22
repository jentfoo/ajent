package llm

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
)

// Registry is the merged model catalogue and the providers serving it.
type Registry struct {
	mu       sync.RWMutex
	models   []Model
	byKey    map[string]int
	byAlias  map[string]int
	entries  map[string]ProviderConfig
	flavors  map[string]Flavor
	env      func(string) string
	file     File
	cache    map[string]CacheEntry
	active   Model
	hasModel bool
}

// RegistryOptions configures a Registry.
type RegistryOptions struct {
	Env func(string) string // defaults to os.Getenv
}

// NewRegistry builds a registry from a models file and any cached discovery
// results. It performs no network calls, so an offline start still works.
func NewRegistry(f File, cache map[string]CacheEntry, opts RegistryOptions) (*Registry, []string) {
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}
	r := &Registry{env: env, file: f, cache: cache}
	warnings := r.rebuild(f, cache)

	if f.DefaultModel != "" {
		if m, err := r.Resolve(f.DefaultModel); err == nil {
			r.SetActive(m)
		} else {
			warnings = append(warnings, "defaultModel "+f.DefaultModel+" not found")
		}
	}
	if !r.hasModel && len(r.models) > 0 {
		r.SetActive(r.models[0])
	}
	return r, warnings
}

// rebuild recomputes the model list from configuration plus cache.
func (r *Registry) rebuild(f File, cache map[string]CacheEntry) []string {
	var warnings []string
	var models []Model
	entries := make(map[string]ProviderConfig, len(f.Providers))
	flavors := make(map[string]Flavor, len(f.Providers))

	for name, cfg := range f.Providers {
		if cfg.Disabled {
			continue
		}
		flavor := flavorFor(name, cfg)
		if cfg.Flavor == FlavorUnknown {
			// the provider key may still name a known flavor, so report what won
			warnings = append(warnings, fmt.Sprintf("provider %s: unknown flavor ignored, using %s; want one of %s",
				name, flavor, strings.Join(flavorChoices(), ", ")))
		}
		def := flavorDefaults[flavor]
		dialect := dialectFor(cfg, def.dialect)
		if dialect == DialectUnknown {
			// a wrong dialect is a wrong protocol, so disable rather than guess
			warnings = append(warnings, fmt.Sprintf("provider %s: unsupported api, provider disabled; want one of %s",
				name, strings.Join(dialectChoices(), ", ")))
			continue
		}
		ctx := modelContext{provider: name, dialect: dialect, baseURL: resolveBaseURL(cfg.BaseURL, def.baseURL)}
		entries[name] = cfg
		flavors[name] = flavor

		for _, w := range compatWarnings(cfg.Compat, dialect) {
			warnings = append(warnings, "provider "+name+": "+w)
		}

		var discovered []ModelConfig
		if e, ok := cache[name]; ok {
			discovered = e.Models
		}
		merged, mw := mergeModels(cfg.Models, discovered, cfg.ModelOverrides)
		for _, w := range mw {
			warnings = append(warnings, "provider "+name+": "+w)
		}
		if len(merged) == 0 {
			warnings = append(warnings, zeroModelsWarning(name, flavor, orBool(def.discover, cfg.Discover)))
		}
		for _, mc := range merged {
			if mc.API == DialectUnknown {
				warnings = append(warnings, fmt.Sprintf("provider %s model %s: unsupported api, model skipped; want one of %s",
					name, mc.ID, strings.Join(dialectChoices(), ", ")))
				continue
			}
			modelDialect, _ := modelEndpoint(ctx, mc)
			for _, w := range compatWarnings(mc.Compat, modelDialect) {
				warnings = append(warnings, fmt.Sprintf("provider %s model %s: %s", name, mc.ID, w))
			}
			models = append(models, resolveModel(ctx, def.caps, cfg.Compat, mc))
		}
	}

	slices.SortFunc(models, func(a, b Model) int {
		if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries, r.flavors = entries, flavors
	r.models = models
	r.byKey = make(map[string]int, len(models))
	r.byAlias = make(map[string]int, len(models))
	for i, m := range models {
		r.byKey[strings.ToLower(m.Key())] = i
		for _, a := range m.Aliases {
			r.byAlias[strings.ToLower(a)] = i
		}
	}
	return warnings
}

// zeroModelsWarning names the ways to populate a provider that resolved empty.
func zeroModelsWarning(name string, flavor Flavor, discover bool) string {
	_, discoverable := discoverySpecs[flavor]
	switch {
	case discoverable && discover:
		return "provider " + name + ` has no models yet; discovery has not returned, or add a "models" array`
	case discoverable:
		return "provider " + name + ` has no models; add a "models" array or set "discover": true`
	default:
		return "provider " + name + ` has no models; add a "models" array`
	}
}

// mergeModels combines the declared, discovered and overridden entries for one
// provider, returning the merged list and warnings for unusable overrides.
//
// When a provider declares models, that list is the whole list: discovery may
// fill gaps in those entries but never adds or removes one. Discovery supplies
// the full list only for a provider that declares nothing. Overrides adjust a
// discovered entry, so an id the user declared is not a valid target.
func mergeModels(declared, discovered []ModelConfig, overrides map[string]ModelOverride) ([]ModelConfig, []string) {
	if len(declared) == 0 {
		return applyOverrides(discovered, overrides), nil
	}
	// a declared list is the whole list, so no override on this provider is reachable
	warnings := unreachableOverrides(declared, discovered, overrides)

	byID := bulk.SliceToIndexBy(func(m ModelConfig) string { return m.ID }, discovered)
	out := make([]ModelConfig, len(declared))
	for i, d := range declared {
		if disc, ok := byID[d.ID]; ok {
			d = enrichModel(d, disc)
		}
		out[i] = d
	}
	return out, warnings
}

// applyOverrides layers each matching override onto a copy of the discovered list.
func applyOverrides(discovered []ModelConfig, overrides map[string]ModelOverride) []ModelConfig {
	if len(overrides) == 0 {
		return discovered
	}
	out := slices.Clone(discovered)
	for i, d := range out {
		if ov, ok := overrides[d.ID]; ok {
			out[i] = applyOverride(d, ov)
		}
	}
	return out
}

// unreachableOverrides names every override a declared model list makes inert, so
// a user is not left wondering why nothing changed. An id that is neither declared
// nor discovered stays silent: discovery is asynchronous, so an id that has not
// arrived yet is not a mistake.
func unreachableOverrides(declared, discovered []ModelConfig, overrides map[string]ModelOverride) []string {
	if len(overrides) == 0 {
		return nil
	}
	declaredIDs := bulk.SliceToSetBy(func(m ModelConfig) string { return m.ID }, declared)
	discoveredIDs := bulk.SliceToSetBy(func(m ModelConfig) string { return m.ID }, discovered)

	var out []string
	for _, id := range slices.Sorted(maps.Keys(overrides)) {
		if _, ok := declaredIDs[id]; ok {
			out = append(out, fmt.Sprintf("modelOverrides %q is also declared in models, the declaration wins", id))
		} else if _, ok := discoveredIDs[id]; ok {
			out = append(out, fmt.Sprintf("modelOverrides %q is not in models, which is the whole list for this provider; declare it to use the override", id))
		}
	}
	return out
}

// applyOverride layers a partial override over a discovered entry, the override
// winning. Headers, sampling params and compat merge per key rather than replace.
func applyOverride(mc ModelConfig, ov ModelOverride) ModelConfig {
	if ov.Name != "" {
		mc.Name = ov.Name
	}
	if ov.Reasoning != nil {
		mc.Reasoning = ov.Reasoning
	}
	if len(ov.Input) > 0 {
		mc.Input = slices.Clone(ov.Input)
	}
	if ov.ContextWindow != nil {
		mc.ContextWindow = ov.ContextWindow
	}
	if ov.MaxTokens != nil {
		mc.MaxTokens = ov.MaxTokens
	}
	if len(ov.LevelMap) > 0 {
		mc.LevelMap = maps.Clone(ov.LevelMap)
	}
	mc.Compat = mergeCompat(mc.Compat, ov.Compat)
	mc.Headers = mergeStrings(mc.Headers, ov.Headers)
	mc.SamplingParams = mergeAny(mc.SamplingParams, ov.SamplingParams)
	return mc
}

// mergeStrings overlays src keys onto a clone of base.
func mergeStrings(base, src map[string]string) map[string]string {
	if len(src) == 0 {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]string, len(src))
	}
	maps.Copy(out, src)
	return out
}

// mergeAny overlays src keys onto a clone of base.
func mergeAny(base, src map[string]any) map[string]any {
	if len(src) == 0 {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]any, len(src))
	}
	maps.Copy(out, src)
	return out
}

// enrichModel fills fields the declared entry left unset from a discovered one,
// which is how a llama.cpp endpoint supplies the real loaded context length.
func enrichModel(declared, discovered ModelConfig) ModelConfig {
	if declared.Name == "" {
		declared.Name = discovered.Name
	}
	if declared.ContextWindow == nil {
		declared.ContextWindow = discovered.ContextWindow
	}
	if declared.MaxTokens == nil {
		declared.MaxTokens = discovered.MaxTokens
	}
	if declared.ContextReserve == nil && discovered.ContextReserve != nil {
		declared.ContextReserve = discovered.ContextReserve
	}
	if declared.Reasoning == nil {
		declared.Reasoning = discovered.Reasoning
	}
	if len(declared.Input) == 0 {
		declared.Input = discovered.Input
	}
	if len(declared.Aliases) == 0 {
		declared.Aliases = discovered.Aliases
	}
	declared.Compat = mergeCompat(discovered.Compat, declared.Compat)
	if len(declared.LevelMap) == 0 {
		declared.LevelMap = discovered.LevelMap
	}
	if len(declared.ThinkingBudgets) == 0 {
		declared.ThinkingBudgets = discovered.ThinkingBudgets
	}
	return declared
}

// Refresh runs discovery and rebuilds the model list from the result, returning
// the updated cache to persist and any warnings.
//
// It never blocks startup: callers run it in the background and the registry
// swaps under its lock when it finishes. The active model is kept if it still
// exists, so a refresh never changes what the next request uses.
func (r *Registry) Refresh(ctx context.Context, opts DiscoverOptions) (map[string]CacheEntry, []string) {
	if opts.Env == nil {
		opts.Env = r.env
	}
	r.mu.RLock()
	file, prev := r.file, r.cache
	r.mu.RUnlock()

	cache, warnings := Discover(ctx, file, prev, opts)

	active := r.Active()
	warnings = append(warnings, r.rebuild(file, cache)...)

	r.mu.Lock()
	r.cache = cache
	r.mu.Unlock()

	if active.ID != "" {
		if m, err := r.Resolve(active.Key()); err == nil {
			r.SetActive(m)
		}
	}
	return cache, warnings
}

// Models returns every known model, sorted by provider then id.
func (r *Registry) Models() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.models)
}

// Active returns the model requests default to.
func (r *Registry) Active() Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.active
}

// SetActive changes the model requests default to.
func (r *Registry) SetActive(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active, r.hasModel = m, true
}

// Resolve returns the model named by an alias, a provider/id pair, a bare id or
// a unique id suffix. An ambiguous name is an error rather than a coin flip.
func (r *Registry) Resolve(name string) (Model, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(name))
	if q == "" {
		return Model{}, ErrUnknownModel
	}
	if i, ok := r.byAlias[q]; ok {
		return r.models[i], nil
	}
	if i, ok := r.byKey[q]; ok {
		return r.models[i], nil
	}
	for _, match := range []func(Model) bool{
		func(m Model) bool { return strings.EqualFold(m.ID, q) },
		func(m Model) bool { return strings.HasSuffix(strings.ToLower(m.ID), q) },
		func(m Model) bool { return strings.Contains(strings.ToLower(m.Key()), q) },
		func(m Model) bool { return strings.Contains(strings.ToLower(m.Name), q) },
	} {
		found, err := r.unique(match, name)
		if err != nil {
			return Model{}, err
		} else if found != nil {
			return *found, nil
		}
	}
	return Model{}, ErrUnknownModel
}

// unique returns the single model matching pred, nil when none match, or an
// error naming the candidates when several do. Caller holds the read lock.
func (r *Registry) unique(pred func(Model) bool, name string) (*Model, error) {
	var hits []int
	for i, m := range r.models {
		if pred(m) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return &r.models[hits[0]], nil
	default:
		candidates := make([]string, len(hits))
		for i, h := range hits {
			candidates[i] = r.models[h].Key()
		}
		return nil, &ErrAmbiguousModel{Name: name, Candidates: candidates}
	}
}

// ProviderConfigFor returns the configuration and flavor serving a model.
func (r *Registry) ProviderConfigFor(m Model) (ProviderConfig, Flavor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.entries[m.Provider]
	return cfg, r.flavors[m.Provider], ok
}

// ProviderNames returns every configured provider name, sorted.
func (r *Registry) ProviderNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := bulk.MapKeysSlice(r.entries)
	slices.Sort(out)
	return out
}
