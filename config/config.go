// Package config is the agent's configuration store — pi's three-file
// model, generalized for any host:
//
//	settings.json  — the SELECTION only: defaultProvider + defaultModel
//	auth.json      — keys for built-in (catalog) providers
//	models.json    — custom provider definitions (endpoint/api/key/models)
//
// Why this lives in the agent library: load, write, resolve and provider
// construction are agent-generic plumbing every host would re-implement.
// The host passes its home directory once (Open) and then only talks to the
// selection/keys/custom-models through this API — keys and endpoints never
// cross the host's IPC bridge, and the active provider object is BUILT here
// so endpoint/protocol/window resolution can never run after construction
// (the class of bug that sent an opencode key to api.openai.com).
//
// Precedence mirrors pi: the built-in catalog is the ABSOLUTE source of
// metadata for vendors it knows (protocol, endpoint, context window);
// auth.json only contributes the key. models.json defines providers the
// catalog does not have; a custom model's window is an explicit declaration,
// refined by an exact model-id match the catalog knows.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File names (pi-compatible).
const (
	SettingsFile = "settings.json"
	AuthFile     = "auth.json"
	ModelsFile   = "models.json"
)

// DefaultMaxTokens is the output cap used when no per-request value is set
// (the desktop's historical default).
const DefaultMaxTokens = 4096

// Settings is settings.json — the selection, nothing else.
type Settings struct {
	DefaultProvider string `json:"defaultProvider"`
	DefaultModel    string `json:"defaultModel"`
}

// AuthEntry is one auth.json provider key record (pi shape).
type AuthEntry struct {
	Type string `json:"type"` // "api_key"
	Key  string `json:"key"`
}

// Auth is the auth.json root: provider id → key record.
type Auth map[string]AuthEntry

// CustomProvider is one models.json provider definition (pi shape):
// endpoint + protocol + key + model list for a provider the catalog lacks.
type CustomProvider struct {
	BaseURL string        `json:"baseUrl"`
	API     string        `json:"api"` // "openai-completions" (pi) / registry kind
	APIKey  string        `json:"apiKey,omitempty"`
	Models  []CustomModel `json:"models"`
}

// CustomModel is a custom model declaration. ContextWindow is the declared
// budget for models no catalog knows (0 → library default).
type CustomModel struct {
	ID            string `json:"id"`
	Reasoning     bool   `json:"reasoning,omitempty"`
	ContextWindow int    `json:"contextWindow,omitempty"`
}

// Models is the models.json root.
type Models struct {
	Providers map[string]CustomProvider `json:"providers"`
}

// Store is an open configuration home. Safe for concurrent use; every
// setter writes through (atomic temp+rename) so a host crash never leaves a
// half-written file.
type Store struct {
	home string

	mu       sync.Mutex
	settings Settings
	auth     Auth
	models   Models
}

// Open loads the three files from home. Missing files are fine (zero-value
// defaults; nothing is created until the first save). A malformed file is
// an error — silently re-writing it would destroy the user's data.
func Open(home string) (*Store, error) {
	if home == "" {
		return nil, fmt.Errorf("config: empty home directory")
	}
	s := &Store{home: home, auth: Auth{}, models: Models{Providers: map[string]CustomProvider{}}}
	if err := readJSON(filepath.Join(home, SettingsFile), &s.settings); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(home, AuthFile), &s.auth); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(home, ModelsFile), &s.models); err != nil {
		return nil, err
	}
	if s.models.Providers == nil {
		s.models.Providers = map[string]CustomProvider{}
	}
	return s, nil
}

// Home returns the directory the store reads and writes.
func (s *Store) Home() string { return s.home }

// Settings returns the current selection.
func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// Key returns the auth.json key for a BUILT-IN provider ("" when none).
func (s *Store) Key(providerID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth[providerID].Key
}

// Custom returns a copy of the models.json provider map.
func (s *Store) Custom() map[string]CustomProvider {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]CustomProvider, len(s.models.Providers))
	for id, p := range s.models.Providers {
		out[id] = p
	}
	return out
}

// SetDefault persists the selection (settings.json).
func (s *Store) SetDefault(provider, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = Settings{DefaultProvider: provider, DefaultModel: model}
	return s.writeLocked(SettingsFile, s.settings)
}

// SetKey persists a built-in provider's key (auth.json). An empty key
// removes the entry.
func (s *Store) SetKey(providerID, key string) error {
	if providerID == "" {
		return fmt.Errorf("config: empty provider id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		delete(s.auth, providerID)
	} else {
		if s.auth == nil {
			s.auth = Auth{}
		}
		s.auth[providerID] = AuthEntry{Type: "api_key", Key: key}
	}
	return s.writeLocked(AuthFile, s.auth)
}

// UpsertCustomProvider persists one custom provider (models.json).
func (s *Store) UpsertCustomProvider(id string, p CustomProvider) error {
	if id == "" {
		return fmt.Errorf("config: empty provider id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.models.Providers == nil {
		s.models.Providers = map[string]CustomProvider{}
	}
	s.models.Providers[id] = p
	return s.writeLocked(ModelsFile, s.models)
}

// DeleteCustomProvider removes one custom provider (models.json).
func (s *Store) DeleteCustomProvider(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.models.Providers, id)
	return s.writeLocked(ModelsFile, s.models)
}

// readJSON loads one file; missing → nil (caller keeps defaults).
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", filepath.Base(path), err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("config: parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeLocked writes one file atomically (caller holds s.mu).
func (s *Store) writeLocked(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", name, err)
	}
	data = append(data, '\n')
	path := filepath.Join(s.home, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: replace %s: %w", name, err)
	}
	return nil
}
