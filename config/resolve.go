package config

// resolve.go — selection → provider object. This is the single place where
// "which provider am I" becomes "a configured Provider": key (auth.json or
// models.json), protocol and endpoint (catalog absolute for known vendors,
// models.json for custom ones) and the context window (catalog first,
// declaration for unknown models). The host never passes endpoints or keys
// into an object it builds itself — construction and resolution cannot
// drift apart.

import (
	"fmt"

	"github.com/liup215/cobot/catalog"
	"github.com/liup215/cobot/provider"
)

// Active is a fully resolved selection — everything needed to build the
// provider object, nothing left for the caller to complete.
type Active struct {
	// ProviderID is the selection id: a catalog vendor name for built-ins,
	// a models.json key for customs.
	ProviderID string
	// Vendor is the catalog id for built-in providers ("" for customs).
	Vendor string
	Model  string
	// Kind is the provider registry kind ("openai", "anthropic", …).
	Kind          string
	APIKey        string
	BaseURL       string
	ContextWindow int
	// Custom reports which side of the split produced the endpoint.
	Custom bool
}

// ProviderOptions decorates the provider object at build time; hosts inject
// their wire-level policy (user agent, static/dynamic headers, content
// transforms) through Decorate without leaking host code into resolution.
type ProviderOptions struct {
	// MaxTokens is the output cap; <=0 → DefaultMaxTokens.
	MaxTokens int
	// Decorate runs on the config after resolution, before registry Create.
	Decorate func(*provider.ProviderConfig)
}

// apiKinds maps pi's models.json `api` values (and raw aliases) onto
// provider registry kinds.
var apiKinds = map[string]string{
	"":                   "openai",
	"openai":             "openai",
	"openai-completions": "openai",
	"openai-compatible":  "openai",
	"anthropic":          "anthropic",
	"anthropic-messages": "anthropic",
}

func kindForAPI(api string) string {
	if k, ok := apiKinds[api]; ok {
		return k
	}
	return api // already a registry name, or unknown — registry reports it
}

// Resolve completes the default selection from the three files.
// Errors are user-actionable ("missing key", "unknown provider").
func (s *Store) Resolve() (Active, error) {
	s.mu.Lock()
	sel := s.settings
	s.mu.Unlock()

	if sel.DefaultProvider == "" {
		return Active{}, fmt.Errorf("config: no default provider configured")
	}

	// Built-in: catalog owns protocol/endpoint/window; auth.json owns the key.
	if v, ok := catalog.Lookup(sel.DefaultProvider); ok {
		model := sel.DefaultModel
		if model == "" && len(v.Models) > 0 {
			model = v.Models[0].ID
		}
		if model == "" {
			return Active{}, fmt.Errorf("config: no model for provider %q", sel.DefaultProvider)
		}
		key := s.Key(sel.DefaultProvider)
		if key == "" {
			return Active{}, fmt.Errorf("config: no API key for provider %q (add it to %s)", sel.DefaultProvider, AuthFile)
		}
		return Active{
			ProviderID:    sel.DefaultProvider,
			Vendor:        sel.DefaultProvider,
			Model:         model,
			Kind:          v.Protocol,
			APIKey:        key,
			BaseURL:       v.BaseURL,
			ContextWindow: catalog.ResolveContextWindow(sel.DefaultProvider, model, 0),
		}, nil
	}

	// Custom: models.json owns everything; the window is the model's
	// declaration refined by an exact catalog model-id match.
	s.mu.Lock()
	cp, ok := s.models.Providers[sel.DefaultProvider]
	s.mu.Unlock()
	if !ok {
		return Active{}, fmt.Errorf("config: provider %q is in neither %s nor %s",
			sel.DefaultProvider, "the catalog", ModelsFile)
	}
	if cp.BaseURL == "" {
		return Active{}, fmt.Errorf("config: custom provider %q has no baseUrl in %s", sel.DefaultProvider, ModelsFile)
	}
	model := sel.DefaultModel
	declared := 0
	if model == "" && len(cp.Models) > 0 {
		model = cp.Models[0].ID
	}
	if model == "" {
		return Active{}, fmt.Errorf("config: no model for provider %q", sel.DefaultProvider)
	}
	for _, m := range cp.Models {
		if m.ID == model {
			declared = m.ContextWindow
			break
		}
	}
	return Active{
		ProviderID:    sel.DefaultProvider,
		Model:         model,
		Kind:          kindForAPI(cp.API),
		APIKey:        cp.APIKey,
		BaseURL:       cp.BaseURL,
		ContextWindow: catalog.ResolveContextWindow("", model, declared),
		Custom:        true,
	}, nil
}

// NewProvider resolves the default selection and BUILDS the provider object
// in one step — resolution cannot run after construction. The returned
// Active lets the host record what it actually built (window for the pill,
// endpoint for diagnostics).
func (s *Store) NewProvider(opts ProviderOptions) (provider.Provider, Active, error) {
	act, err := s.Resolve()
	if err != nil {
		return nil, Active{}, err
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	cfg := provider.ProviderConfig{
		APIKey:    act.APIKey,
		Model:     act.Model,
		BaseURL:   act.BaseURL,
		MaxTokens: maxTokens,
	}
	if opts.Decorate != nil {
		opts.Decorate(&cfg)
	}
	provider.InitDefaultRegistry()
	p, err := provider.DefaultRegistry.Create(act.Kind, cfg)
	if err != nil {
		return nil, Active{}, fmt.Errorf("config: create provider %q: %w", act.ProviderID, err)
	}
	return p, act, nil
}
