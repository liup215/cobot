package provider

import (
	"fmt"
	"sync"
)

// ProviderFactory is a function that creates a provider instance
type ProviderFactory func(config ProviderConfig) (Provider, error)

// ProviderConfig is shared with the interface layer (see provider.go);
// the registry intentionally reuses the single definition.

// Registry manages provider factories
type Registry struct {
	factories map[string]ProviderFactory
	mu        sync.RWMutex
}

// NewRegistry creates a new provider registry
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]ProviderFactory),
	}
}

// Register adds a provider factory to the registry
func (r *Registry) Register(name string, factory ProviderFactory) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name == "" {
		return fmt.Errorf("provider name cannot be empty")
	}

	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("provider already registered: %s", name)
	}

	r.factories[name] = factory
	return nil
}

// Get retrieves a provider factory by name
func (r *Registry) Get(name string) (ProviderFactory, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	factory, exists := r.factories[name]
	if !exists {
		return nil, fmt.Errorf("provider not found: %s", name)
	}

	return factory, nil
}

// Create creates a provider instance
func (r *Registry) Create(name string, config ProviderConfig) (Provider, error) {
	factory, err := r.Get(name)
	if err != nil {
		return nil, err
	}

	return factory(config)
}

// List returns all registered provider names
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	return names
}

// Count returns the number of registered providers
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.factories)
}

// DefaultRegistry is the global default registry
var DefaultRegistry = NewRegistry()

// RegisterDefault registers a provider in the default registry
func RegisterDefault(name string, factory ProviderFactory) error {
	return DefaultRegistry.Register(name, factory)
}

// CreateDefault creates a provider from the default registry
func CreateDefault(name string, config ProviderConfig) (Provider, error) {
	return DefaultRegistry.Create(name, config)
}

// InitDefaultRegistry initializes the default registry with built-in providers
func InitDefaultRegistry() {
	// Register OpenAI-compatible provider
	// Supports OpenAI, OpenRouter, and any OpenAI-compatible API
	RegisterDefault("openai", func(config ProviderConfig) (Provider, error) {
		return NewOpenAIProvider(config.APIKey, config.Model, config.BaseURL), nil
	})

	// Register native Anthropic Messages provider (Claude models).
	// An empty API key falls back to the ANTHROPIC_API_KEY env var.
	RegisterDefault("anthropic", func(config ProviderConfig) (Provider, error) {
		return NewAnthropicProvider(config.APIKey, config.Model, config.BaseURL), nil
	})
}

// GetProvider creates a provider instance with the given configuration
func GetProvider(name string, apiKey string, model string) (Provider, error) {
	return GetProviderWithBaseURL(name, apiKey, model, "")
}

// GetProviderWithBaseURL creates a provider instance with base URL configuration
// This is useful for OpenAI-compatible providers that need a custom endpoint
func GetProviderWithBaseURL(name string, apiKey string, model string, baseURL string) (Provider, error) {
	// Initialize registry if not already done
	if DefaultRegistry.Count() == 0 {
		InitDefaultRegistry()
	}

	config := ProviderConfig{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: baseURL,
	}

	return DefaultRegistry.Create(name, config)
}

// GetProviderFromConfig creates a provider from a full ProviderConfig
func GetProviderFromConfig(name string, config ProviderConfig) (Provider, error) {
	// Initialize registry if not already done
	if DefaultRegistry.Count() == 0 {
		InitDefaultRegistry()
	}

	return DefaultRegistry.Create(name, config)
}

// SupportedProviders returns a list of supported provider names
func SupportedProviders() []string {
	// Initialize registry if not already done
	if DefaultRegistry.Count() == 0 {
		InitDefaultRegistry()
	}
	return DefaultRegistry.List()
}

// IsProviderSupported checks if a provider is supported
func IsProviderSupported(name string) bool {
	providers := SupportedProviders()
	for _, p := range providers {
		if p == name {
			return true
		}
	}
	return false
}
