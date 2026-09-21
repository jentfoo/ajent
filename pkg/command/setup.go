package command

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tui"
)

// probeProvider is the wizard's discovery step, a variable so tests stub it.
var probeProvider = llm.ProbeProvider

// probeAll is the custom-URL discovery step, a variable so tests stub it.
var probeAll = llm.ProbeAll

// saveUserFile persists the models file, a variable so tests stub it.
var saveUserFile = llm.SaveUserFile

// setupChoice is one wizard row: a provider the registry already understands.
type setupChoice struct {
	name  string // provider key, and the flavor name
	label string
	local bool // server URL instead of an API key
}

// customProviderName is the menu row for a user-supplied OpenAI-compatible URL.
const customProviderName = "custom"

// providerSetupChoices is the wizard menu in onboarding order: the local
// servers, the custom URL row, then the hosted providers. Regional catalogue
// flavors (the -cn and token-plan variants) stay configurable in models.json
// but are not listed; the menu holds one row per vendor.
var providerSetupChoices = []setupChoice{
	{name: "llamacpp", label: "llama.cpp server", local: true},
	{name: "lmstudio", label: "LM Studio", local: true},
	{name: customProviderName, label: "Other (OpenAI-compatible URL)"},
	{name: "openrouter", label: "OpenRouter"},
	{name: "anthropic", label: "Anthropic"},
	{name: "openai", label: "OpenAI"},
	{name: "zai", label: "Z.AI (GLM)"},
	{name: "deepseek", label: "DeepSeek"},
	{name: "groq", label: "Groq"},
	{name: "together", label: "Together"},
	{name: "xai", label: "xAI (Grok)"},
	{name: "moonshotai", label: "Moonshot AI"},
	{name: "kimi", label: "Kimi (coding plan)"},
	{name: "mistral", label: "Mistral"},
	{name: "google", label: "Google Gemini"},
	{name: "cerebras", label: "Cerebras"},
	{name: "nvidia", label: "NVIDIA"},
	{name: "huggingface", label: "Hugging Face"},
	{name: "minimax", label: "MiniMax"},
	{name: "baseten", label: "Baseten"},
	{name: "ant-ling", label: "Ant Ling"},
	{name: "fireworks", label: "Fireworks"},
	{name: "qwen-token-plan", label: "Qwen Token Plan"},
	{name: "xiaomi", label: "Xiaomi MiMo"},
}

// discovers reports whether the wizard may probe the endpoint for its model
// list: the custom row always, hosted flavors per their catalogue entry.
func (ch setupChoice) discovers() bool {
	if ch.name == customProviderName {
		return true
	}
	prof, ok := llm.LookupFlavor(ch.name)
	return ok && prof.Discover
}

// group names the picker section a row renders under.
func (ch setupChoice) group() string {
	if ch.name == customProviderName {
		return "Other"
	}
	if ch.local {
		return "Local"
	}
	return "Hosted"
}

// ProviderSetup walks a first-time user onto their first provider: pick one,
// add a key or server URL, settle its models and write the user models file.
// The written provider is merged into any existing file and loaded into the
// live registry. It reports tui.ErrCancelled when dismissed.
func ProviderSetup(ctx context.Context, c Console) error {
	idx, err := c.Pick(ctx, "Choose a model provider", providerSetupItems(),
		tui.PickOptions{Placeholder: filterPlaceholder})
	if err != nil {
		return err
	}
	choice := providerSetupChoices[idx]

	cfg, err := setupCredentials(ctx, c, choice)
	if err != nil {
		return err
	}
	file, entry, err := setupModels(ctx, c, choice, cfg)
	if err != nil {
		return err
	}
	file, err = mergeUserFile(file)
	if err != nil {
		return err
	}
	if err := saveUserFile(file); err != nil {
		return fmt.Errorf("could not write %s: %w", modelsUserPath(), err)
	}

	cache := llm.LoadUserCache()
	if cache == nil {
		cache = make(map[string]llm.CacheEntry)
	}
	if len(entry.Models) > 0 {
		cache[choice.name] = entry // the probe result stands in for the first refresh
		if err := llm.SaveUserCache(cache); err != nil {
			c.Notify("could not save the model cache: "+err.Error(), levelWarn)
		}
	}
	for _, w := range c.Models().Load(file, cache) {
		c.Notify(w, levelWarn)
	}

	c.Print(fmt.Sprintf(
		"Provider **%s** is ready in `~/.ajent/%s`.\n\n- `/model` switches models\n- `/help` lists commands\n",
		choice.name, llm.ModelsFileName))
	return nil
}

// mergeUserFile lays the wizard's provider and default model over the
// existing user file, so an earlier configuration never loses a provider.
func mergeUserFile(file llm.File) (llm.File, error) {
	base, _, err := llm.LoadUserFile()
	if err != nil {
		return file, err
	}
	for name, cfg := range file.Providers {
		if base.Providers == nil {
			base.Providers = make(map[string]llm.ProviderConfig, 1)
		}
		base.Providers[name] = cfg
	}
	if file.DefaultModel != "" {
		base.DefaultModel = file.DefaultModel
	}
	return base, nil
}

// providerSetupItems renders the menu rows under their group headers.
func providerSetupItems() []tui.PickItem {
	items := make([]tui.PickItem, len(providerSetupChoices))
	for i, ch := range providerSetupChoices {
		items[i] = tui.PickItem{Label: ch.label, Detail: ch.name, Group: ch.group()}
	}
	return items
}

// setupCredentials collects the endpoint secret: the flavor's environment
// variable when already set, a pasted key for hosted providers, or a server
// URL for local ones. An empty hosted key is accepted, leaving the flavor's
// conventional variable to resolve at request time.
func setupCredentials(ctx context.Context, c Console, choice setupChoice) (llm.ProviderConfig, error) {
	var cfg llm.ProviderConfig
	if choice.name == customProviderName {
		cfg.Flavor = llm.FlavorGeneric
		url, err := c.Input(ctx, "Server URL", "http://localhost:8080/v1")
		if err != nil {
			return cfg, err
		}
		if u := strings.TrimSpace(url); u != "" {
			cfg.BaseURL = ensureScheme(u)
		} else {
			return cfg, tui.ErrCancelled // nothing to default to
		}
		discover := true
		cfg.Discover = &discover
		return cfg, nil
	}
	prof, ok := llm.LookupFlavor(choice.name)
	if !ok {
		return cfg, fmt.Errorf("unknown provider flavor %q", choice.name)
	}
	if choice.local {
		url, err := c.Input(ctx, "Server URL", prof.BaseURL)
		if err != nil {
			return cfg, err
		}
		if u := strings.TrimSpace(url); u != "" {
			cfg.BaseURL = inheritFlavorPath(prof.BaseURL, ensureScheme(u))
		}
		return cfg, nil
	}
	if prof.APIKeyEnv != "" && os.Getenv(prof.APIKeyEnv) != "" {
		cfg.APIKeyEnv = prof.APIKeyEnv // the env keeps the secret off disk
		c.Notify("using "+prof.APIKeyEnv+" from the environment", levelInfo)
		return cfg, nil
	}
	placeholder := "paste key"
	if prof.APIKeyEnv != "" {
		placeholder = "paste key, or leave empty to use $" + prof.APIKeyEnv + " later"
	}
	key, err := c.Input(ctx, apiKeyLabel(choice.name, prof.APIKeyEnv), placeholder)
	if err != nil {
		return cfg, err
	}
	cfg.APIKey = strings.TrimSpace(key)
	return cfg, nil
}

// setupModels settles the provider's models: discovery when the endpoint can be
// asked, otherwise one manually entered id for hosted providers. The probe
// result returns with the file so the caller can seed the discovery cache.
// Dismissing the model step keeps the entry; any discovered model is one
// /model away.
func setupModels(ctx context.Context, c Console, choice setupChoice, cfg llm.ProviderConfig) (llm.File, llm.CacheEntry, error) {
	var file llm.File
	var entry llm.CacheEntry

	if choice.discovers() {
		c.Notify("discovering models…", levelInfo)
		pctx, cancel := context.WithTimeout(ctx, llm.DiscoverTimeout)
		defer cancel()
		var perr error
		if choice.name == customProviderName {
			entry, perr = probeAll(pctx, cfg, llm.DiscoverOptions{})
		} else {
			entry, perr = probeProvider(pctx, choice.name, cfg, llm.DiscoverOptions{})
		}
		if perr != nil {
			c.Notify("discovery failed: "+perr.Error(), levelWarn)
		}
	}

	defaultModel := ""
	switch {
	case len(entry.Models) > 0:
		id, err := pickDiscoveredModel(ctx, c, entry.Models)
		if err != nil && !errors.Is(err, tui.ErrCancelled) {
			return file, entry, err
		}
		defaultModel = id
	case !choice.local:
		// hosted without a reachable list: one declared id keeps the entry usable
		id, err := c.Input(ctx, "Model id", "the model id from the provider's docs")
		if err != nil && !errors.Is(err, tui.ErrCancelled) {
			return file, entry, err
		}
		if id = strings.TrimSpace(id); id != "" {
			cfg.Models = []llm.ModelConfig{{ID: id}}
			defaultModel = id
		}
	default:
		// the server is simply not up yet; startup discovery fills the list
		c.Notify("models are discovered once the server is reachable", levelInfo)
	}

	file = llm.File{Providers: map[string]llm.ProviderConfig{choice.name: cfg}}
	file.DefaultModel = defaultModel
	return file, entry, nil
}

// pickDiscoveredModel shows the probed list and returns the chosen model id.
func pickDiscoveredModel(ctx context.Context, c Console, models []llm.ModelConfig) (string, error) {
	items := make([]tui.PickItem, len(models))
	for i, m := range models {
		items[i] = tui.PickItem{Label: m.ID, Detail: discoveredModelDetail(m)}
	}
	idx, err := c.Pick(ctx, "Choose a model", items, tui.PickOptions{Placeholder: filterPlaceholder})
	if err != nil {
		return "", err
	}
	return models[idx].ID, nil
}

// discoveredModelDetail names a discovered model's display name and window.
func discoveredModelDetail(m llm.ModelConfig) string {
	parts := make([]string, 0, 2)
	if m.Name != "" && m.Name != m.ID {
		parts = append(parts, m.Name)
	}
	if m.ContextWindow != nil && *m.ContextWindow > 0 {
		parts = append(parts, strutil.FormatTokens(*m.ContextWindow))
	}
	return strings.Join(parts, " · ")
}

// ensureScheme defaults a scheme-less server URL to http.
func ensureScheme(u string) string {
	if u == "" || strings.Contains(u, "://") {
		return u
	}
	return "http://" + u
}

// inheritFlavorPath appends the flavor default's path to a bare-host URL. A
// URL that already carries a path is kept unchanged.
func inheritFlavorPath(defaultURL, entered string) string {
	u, err := url.Parse(entered)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return entered
	}
	def, err := url.Parse(defaultURL)
	if err != nil || def.Path == "" || def.Path == "/" {
		return entered
	}
	return strings.TrimRight(entered, "/") + def.Path
}

// apiKeyLabel names the credential prompt for a hosted provider.
func apiKeyLabel(name, env string) string {
	if env == "" {
		return name + " API key"
	}
	return name + " API key ($" + env + ")"
}

// modelsUserPath returns the user models file path for messages.
func modelsUserPath() string {
	path, err := config.UserPath(llm.ModelsFileName)
	if err != nil {
		return "~/.ajent/" + llm.ModelsFileName
	}
	return path
}
