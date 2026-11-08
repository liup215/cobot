// Package catalog is cobot's built-in vendor model directory — the DATA
// layer that sits beside the provider package (the FUNCTION/protocol layer).
//
// Architecture split (deliberate, per project design):
//
//	provider/  — how to talk: wire protocols (openai-compatible, anthropic),
//	             request/response/stream handling, the registry of protocol
//	             kinds. Anything that needs HTTP lives there, not here.
//	catalog/   — who offers what: built-in vendors, their default endpoints,
//	             their model ids, and per-model limits (context window /
//	             output cap). Pure data — no HTTP, no user configuration.
//
// Built-in vendor providers vs custom endpoints:
//
//   - A built-in vendor ("deepseek", "anthropic", …) is described here:
//     Vendor.Protocol says which provider registry kind speaks its wire,
//     Vendor.BaseURL is the default endpoint, and every Model carries its
//     ContextWindow so hosts never guess (pi ships the same metadata in its
//     bundled catalog).
//   - A custom endpoint (user-supplied base_url/api_key/model) is NOT part
//     of this catalog. The host builds it via provider.ProviderConfig and
//     owns its context window itself (e.g. the desktop stores
//     llm_providers[].context_window per entry). Catalog lookup is an
//     optional convenience for known vendors; ResolveContextWindow falls
//     back exactly the way pi does (unknown model → DefaultContextWindow).
//
// The bundled data snapshot follows pi's model catalog semantics: context
// window in tokens (DefaultContextWindow when unknown), max output tokens
// per model (DefaultMaxOutputTokens when unknown).
package catalog

import "sync"

// Defaults mirror pi's documented models.json semantics: a model without a
// known context window is assumed to have DefaultContextWindow tokens, and
// its output cap defaults to DefaultMaxOutputTokens.
const (
	// DefaultContextWindow is the assumed context budget for a model the
	// catalog does not know (pi: contextWindow default 128000).
	DefaultContextWindow = 128000
	// DefaultMaxOutputTokens is the assumed per-response output cap
	// (pi: maxTokens default 16384).
	DefaultMaxOutputTokens = 16384
)

// Model is one catalog model entry: identity plus the limits hosts need to
// budget context (compaction watermarks, usage displays).
type Model struct {
	// ID is the provider-facing model id (what goes in the "model" field).
	ID string `json:"id"`
	// Name is a human-readable label for pickers.
	Name string `json:"name"`
	// ContextWindow is the model's total context budget in tokens.
	ContextWindow int `json:"context_window"`
	// MaxOutputTokens is the maximum tokens a single response may emit.
	MaxOutputTokens int `json:"max_output_tokens"`
}

// Vendor is one built-in vendor provider and its model list.
type Vendor struct {
	// Name is the catalog id ("deepseek"); hosts store it on their saved
	// endpoints (desktop: llm_providers[].vendor) to re-resolve metadata.
	Name string `json:"name"`
	// DisplayName is the UI label.
	DisplayName string `json:"display_name"`
	// Protocol is the provider registry kind that speaks this vendor's
	// wire ("openai" for OpenAI-compatible endpoints, "anthropic" for the
	// native Messages API).
	Protocol string `json:"protocol"`
	// BaseURL is the vendor's default endpoint.
	BaseURL string `json:"base_url"`
	// Models is the vendor's model list, sorted by ID.
	Models []Model `json:"models"`
}

// index is built once from the generated data table.
type index struct {
	vendors map[string]Vendor
	// byID indexes every model by exact model id; first vendor in the
	// table wins on collisions (best-effort — prefer vendor-qualified
	// lookups).
	byID map[string]Model
}

var (
	builtOnce sync.Once
	built     index
)

func buildIndex() index {
	ix := index{vendors: make(map[string]Vendor, len(builtinVendors)), byID: make(map[string]Model, 256)}
	for _, v := range builtinVendors {
		// Defensive copies: callers may range over returned data freely;
		// nobody mutates the shared table.
		models := make([]Model, len(v.Models))
		copy(models, v.Models)
		v.Models = models
		ix.vendors[v.Name] = v
		for _, m := range v.Models {
			if _, dup := ix.byID[m.ID]; !dup {
				ix.byID[m.ID] = m
			}
		}
	}
	return ix
}

// ensureIndex builds the lookup tables on first use (data-race free).
func ensureIndex() index {
	builtOnce.Do(func() { built = buildIndex() })
	return built
}

// Vendors returns the built-in vendor list (a fresh copy each call).
func Vendors() []Vendor {
	ix := ensureIndex()
	out := make([]Vendor, 0, len(ix.vendors))
	for _, v := range builtinVendors {
		models := make([]Model, len(v.Models))
		copy(models, v.Models)
		v.Models = models
		out = append(out, v)
	}
	return out
}

// Lookup returns a built-in vendor by catalog name.
func Lookup(name string) (Vendor, bool) {
	v, ok := ensureIndex().vendors[name]
	return v, ok
}

// FindModel returns one model of a vendor.
func FindModel(vendorName, modelID string) (Model, bool) {
	v, ok := Lookup(vendorName)
	if !ok {
		return Model{}, false
	}
	for _, m := range v.Models {
		if m.ID == modelID {
			return m, true
		}
	}
	return Model{}, false
}

// FindModelAnywhere scans every vendor for an exact model id — the
// vendor-less fallback for legacy saved endpoints that never recorded a
// vendor name. Returns the owning vendor name too.
func FindModelAnywhere(modelID string) (string, Model, bool) {
	ix := ensureIndex()
	m, ok := ix.byID[modelID]
	if !ok {
		return "", Model{}, false
	}
	for _, v := range builtinVendors {
		for _, mm := range v.Models {
			if mm.ID == m.ID {
				return v.Name, m, true
			}
		}
	}
	return "", m, false
}

// ResolveContextWindow resolves a model's context budget the way pi does:
//
//	vendor+model known      → the catalog's window
//	vendor known, model not → fallback (a custom model on a vendor endpoint
//	                          is a custom configuration — host owns it)
//	vendor unknown/empty    → exact-id scan across all vendors (legacy
//	                          entries without a recorded vendor)
//	nothing matches         → fallback, DefaultContextWindow when fallback
//	                          is not a positive number
func ResolveContextWindow(vendorName, modelID string, fallback int) int {
	if vendorName != "" {
		if v, ok := Lookup(vendorName); ok {
			for _, m := range v.Models {
				if m.ID == modelID {
					return m.ContextWindow
				}
			}
			return normalizedFallback(fallback)
		}
	}
	if modelID != "" {
		if _, m, ok := FindModelAnywhere(modelID); ok {
			return m.ContextWindow
		}
	}
	return normalizedFallback(fallback)
}

func normalizedFallback(fallback int) int {
	if fallback > 0 {
		return fallback
	}
	return DefaultContextWindow
}
