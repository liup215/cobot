package catalog

import "testing"

func TestBuiltinVendorsWellFormed(t *testing.T) {
	vs := Vendors()
	if len(vs) < 10 {
		t.Fatalf("only %d vendors — catalog generation looks broken", len(vs))
	}
	seen := map[string]bool{}
	for _, v := range vs {
		if v.Name == "" || v.DisplayName == "" || v.BaseURL == "" {
			t.Errorf("vendor %+v missing identity fields", v)
		}
		if v.Protocol != "openai" && v.Protocol != "anthropic" {
			t.Errorf("vendor %s: protocol %q not speakable by the provider registry", v.Name, v.Protocol)
		}
		if seen[v.Name] {
			t.Errorf("duplicate vendor name %s", v.Name)
		}
		seen[v.Name] = true
		if len(v.Models) == 0 {
			t.Errorf("vendor %s has no models", v.Name)
		}
		last := ""
		for _, m := range v.Models {
			if m.ID == "" {
				t.Errorf("vendor %s: empty model id", v.Name)
			}
			if m.ContextWindow <= 0 {
				t.Errorf("vendor %s/%s: context window %d must be positive", v.Name, m.ID, m.ContextWindow)
			}
			if m.MaxOutputTokens <= 0 {
				t.Errorf("vendor %s/%s: max output %d must be positive", v.Name, m.ID, m.MaxOutputTokens)
			}
			if last != "" && m.ID < last {
				t.Errorf("vendor %s: models not sorted (%s after %s)", v.Name, m.ID, last)
			}
			last = m.ID
		}
	}
}

func TestLookupAndFindModel(t *testing.T) {
	if _, ok := Lookup("no-such-vendor"); ok {
		t.Fatal("unknown vendor must not resolve")
	}
	v, ok := Lookup("openai")
	if !ok {
		t.Fatal("openai vendor missing")
	}
	m := v.Models[0]
	got, ok := FindModel("openai", m.ID)
	if !ok || got.ContextWindow != m.ContextWindow {
		t.Fatalf("FindModel(%q) = %+v, %v", m.ID, got, ok)
	}
	if _, ok := FindModel("openai", "definitely-not-a-model"); ok {
		t.Fatal("unknown model must not resolve")
	}
}

func TestFindModelAnywhere(t *testing.T) {
	anth, _ := Lookup("anthropic")
	m := anthropicModel(t, anth)
	vendor, got, ok := FindModelAnywhere(m.ID)
	if !ok || vendor != "anthropic" || got.ContextWindow != m.ContextWindow {
		t.Fatalf("FindModelAnywhere(%q) = %q %+v %v", m.ID, vendor, got, ok)
	}
	if _, _, ok := FindModelAnywhere("no-such-model"); ok {
		t.Fatal("unknown model must not resolve globally")
	}
}

func anthropicModel(t *testing.T, v Vendor) Model {
	t.Helper()
	for _, m := range v.Models {
		return m
	}
	t.Fatal("anthropic has no models")
	return Model{}
}

func TestResolveContextWindow(t *testing.T) {
	v, _ := Lookup("anthropic")
	m := anthropicModel(t, v)

	// vendor + model known → catalog window (NOT the fallback).
	if got := ResolveContextWindow("anthropic", m.ID, 1); got != m.ContextWindow {
		t.Errorf("vendor+model resolve = %d, want %d", got, m.ContextWindow)
	}
	// vendor known, model unknown → fallback (custom model, host owns it).
	if got := ResolveContextWindow("anthropic", "my-finetune", 64000); got != 64000 {
		t.Errorf("vendor-only resolve = %d, want fallback 64000", got)
	}
	// vendor empty → global exact-id scan (legacy entries).
	if got := ResolveContextWindow("", m.ID, 1); got != m.ContextWindow {
		t.Errorf("vendorless resolve = %d, want %d", got, m.ContextWindow)
	}
	// nothing matches → fallback; non-positive fallback → default.
	if got := ResolveContextWindow("nope", "nope", 42000); got != 42000 {
		t.Errorf("miss resolve = %d, want 42000", got)
	}
	if got := ResolveContextWindow("", "", 0); got != DefaultContextWindow {
		t.Errorf("cold resolve = %d, want DefaultContextWindow %d", got, DefaultContextWindow)
	}
}

func TestDefaultsMatchDocumentedSemantics(t *testing.T) {
	if DefaultContextWindow != 128000 {
		t.Errorf("DefaultContextWindow = %d, pi documents 128000", DefaultContextWindow)
	}
	if DefaultMaxOutputTokens != 16384 {
		t.Errorf("DefaultMaxOutputTokens = %d, pi documents 16384", DefaultMaxOutputTokens)
	}
}

// Vendors() hands out copies: mutating a returned slice must not corrupt the
// shared table (the desktop ranges over it for UI lists on every modal open).
func TestVendorsReturnsCopies(t *testing.T) {
	a := Vendors()
	a[0].Models[0].ContextWindow = 1
	b := Vendors()
	if b[0].Models[0].ContextWindow == 1 {
		t.Fatal("mutating a returned vendor leaked into the shared catalog")
	}
}

// Adopt = vendor attribution by model id + endpoint (legacy entries carry
// only those two facts — the pi-style reference migration depends on it).
func TestAdopt(t *testing.T) {
	ds, ok := Lookup("deepseek")
	if !ok || len(ds.Models) == 0 {
		t.Fatal("deepseek catalog missing")
	}
	m := ds.Models[0]

	if v, ok := Adopt(m.ID, ds.BaseURL); !ok || v.Name != "deepseek" {
		t.Errorf("exact endpoint adopt failed: %q %v", v.Name, ok)
	}
	// Trailing slash must not matter.
	if v, ok := Adopt(m.ID, ds.BaseURL+"/"); !ok || v.Name != "deepseek" {
		t.Errorf("trailing-slash adopt failed: %q %v", v.Name, ok)
	}
	// Same model on a DIFFERENT endpoint stays custom.
	if _, ok := Adopt(m.ID, "https://proxy.example/v1"); ok {
		t.Error("different endpoint must not adopt a vendor")
	}
	if _, ok := Adopt("no-such-model", ds.BaseURL); ok {
		t.Error("unknown model must not adopt")
	}
	if _, ok := Adopt("", ""); ok {
		t.Error("empty input must not adopt")
	}
}
