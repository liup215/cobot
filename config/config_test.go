package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liup215/cobot/catalog"
	"github.com/liup215/cobot/provider"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpenMissingFilesStartsCleanWithoutCreating(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.Settings(); got != (Settings{}) {
		t.Errorf("settings = %+v, want zero", got)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("Open must not create files, found %d", len(entries))
	}
}

func TestRoundTripSelectionKeyAndCustomProvider(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetDefault("opencode-go", "mimo-v2.6-flash"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}
	if err := s.SetKey("opencode-go", "oc_sk_secret"); err != nil {
		t.Fatalf("SetKey: %v", err)
	}
	if err := s.UpsertCustomProvider("depu", CustomProvider{
		BaseURL: "https://gateway.ai.depu.school",
		API:     "openai-completions",
		APIKey:  "sk-ee",
		Models:  []CustomModel{{ID: "llm-ultra", ContextWindow: 128000}},
	}); err != nil {
		t.Fatalf("UpsertCustomProvider: %v", err)
	}

	// Fresh read: everything persisted.
	s2, err := Open(home)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Settings(); got.DefaultProvider != "opencode-go" || got.DefaultModel != "mimo-v2.6-flash" {
		t.Errorf("settings = %+v", got)
	}
	if got := s2.Key("opencode-go"); got != "oc_sk_secret" {
		t.Errorf("key = %q", got)
	}
	if p, ok := s2.Custom()["depu"]; !ok || p.BaseURL == "" || p.Models[0].ID != "llm-ultra" {
		t.Errorf("custom provider lost: %+v", p)
	}

	// Empty key removes the entry; no .tmp litter after write-through saves.
	if err := s2.SetKey("opencode-go", ""); err != nil {
		t.Fatalf("SetKey delete: %v", err)
	}
	if got := s2.Key("opencode-go"); got != "" {
		t.Errorf("key not deleted: %q", got)
	}
	for _, f := range []string{SettingsFile, AuthFile, ModelsFile} {
		if _, err := os.Stat(filepath.Join(home, f+".tmp")); err == nil {
			t.Errorf("%s.tmp left behind", f)
		}
	}
}

func TestOpenRejectsMalformedFile(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, SettingsFile), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err == nil {
		t.Fatal("malformed settings.json must not be silently ignored")
	}
}

func TestResolveBuiltinUsesCatalogAndAuth(t *testing.T) {
	v, ok := catalog.Lookup("opencode-go")
	if !ok || v.BaseURL == "" {
		t.Fatalf("catalog vendor missing: %+v", v)
	}
	s := openStore(t)
	// Selection without key → user-actionable error.
	if err := s.SetDefault("opencode-go", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(); err == nil || !strings.Contains(err.Error(), AuthFile) {
		t.Fatalf("missing key error = %v, want mention of %s", err, AuthFile)
	}
	if err := s.SetKey("opencode-go", "oc_sk_x"); err != nil {
		t.Fatal(err)
	}
	act, err := s.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if act.BaseURL != v.BaseURL || act.Kind != v.Protocol || act.APIKey != "oc_sk_x" {
		t.Errorf("builtin resolve = %+v, want catalog base %q kind %q", act, v.BaseURL, v.Protocol)
	}
	if act.Custom || act.Vendor != "opencode-go" {
		t.Errorf("builtin identity wrong: %+v", act)
	}
	// Empty defaultModel → the vendor's first model.
	if act.Model != v.Models[0].ID {
		t.Errorf("model = %q, want vendor default %q", act.Model, v.Models[0].ID)
	}
	// Catalog window, not a declaration.
	if want := catalog.ResolveContextWindow("opencode-go", act.Model, 0); act.ContextWindow != want {
		t.Errorf("window = %d, want catalog %d", act.ContextWindow, want)
	}
}

func TestResolveCustomProviderFromModelsJSON(t *testing.T) {
	s := openStore(t)
	if err := s.SetDefault("depu", "my-finetune"); err != nil {
		t.Fatal(err)
	}
	// Unknown model → the DECLARED window.
	if err := s.UpsertCustomProvider("depu", CustomProvider{
		BaseURL: "https://gateway.ai.depu.school",
		API:     "openai-completions",
		APIKey:  "sk-ee",
		Models:  []CustomModel{{ID: "my-finetune", ContextWindow: 42000}},
	}); err != nil {
		t.Fatal(err)
	}
	act, err := s.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !act.Custom || act.Kind != "openai" || act.BaseURL == "" || act.ContextWindow != 42000 {
		t.Errorf("custom resolve = %+v", act)
	}

	// A model id the catalog knows → catalog window beats the declaration
	// (catalog absolute), endpoint stays the custom one.
	known, _ := catalog.Lookup("anthropic")
	if err := s.SetDefault("depu", known.Models[0].ID); err != nil {
		t.Fatal(err)
	}
	act, err = s.Resolve()
	if err != nil {
		t.Fatalf("Resolve known model: %v", err)
	}
	if act.BaseURL != "https://gateway.ai.depu.school" {
		t.Errorf("custom endpoint lost: %+v", act)
	}
	if want := catalog.ResolveContextWindow("", known.Models[0].ID, 0); act.ContextWindow != want {
		t.Errorf("known custom model window = %d, want catalog %d", act.ContextWindow, want)
	}
}

func TestResolveUnknownProviderFails(t *testing.T) {
	s := openStore(t)
	if err := s.SetDefault("ghost", "whatever"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(); err == nil {
		t.Fatal("provider in neither catalog nor models.json must error")
	}
}

func TestNewProviderBuildsWithResolvedConfig(t *testing.T) {
	v, _ := catalog.Lookup("opencode-go")
	s := openStore(t)
	if err := s.SetDefault("opencode-go", "mimo-v2.6-flash"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKey("opencode-go", "oc_sk_x"); err != nil {
		t.Fatal(err)
	}

	var seen provider.ProviderConfig
	p, act, err := s.NewProvider(ProviderOptions{
		Decorate: func(c *provider.ProviderConfig) {
			c.Headers = map[string]string{"User-Agent": "host-app"}
			seen = *c
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if p == nil {
		t.Fatal("nil provider")
	}
	// The ordering guard: the config the registry received is RESOLVED.
	if seen.BaseURL != v.BaseURL || seen.APIKey != "oc_sk_x" || seen.Model != "mimo-v2.6-flash" {
		t.Errorf("provider built with unresolved config: base=%q key=%q model=%q",
			seen.BaseURL, seen.APIKey, seen.Model)
	}
	if seen.MaxTokens != DefaultMaxTokens {
		t.Errorf("default output cap = %d, want %d", seen.MaxTokens, DefaultMaxTokens)
	}
	if seen.Headers["User-Agent"] != "host-app" {
		t.Error("decorator hook not applied")
	}
	if act.ContextWindow < 1_000_000 {
		t.Errorf("active window = %d, want the catalog's 1M", act.ContextWindow)
	}
}

func TestSettingsJSONShapeMatchesPi(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefault("opencode-go", "mimo-v2.6-flash"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, SettingsFile))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["defaultProvider"] != "opencode-go" || m["defaultModel"] != "mimo-v2.6-flash" {
		t.Errorf("settings.json shape = %s, want exactly {defaultProvider, defaultModel}", raw)
	}
}
