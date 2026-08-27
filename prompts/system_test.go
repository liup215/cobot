package prompts

import (
	"sort"
	"testing"

	"github.com/liup215/cobot/tools"
)

// The static tool catalogue in this package (GetToolDescriptions and the
// mode-filtered variants) is a hand-maintained mirror of the tools that
// tools.InitDefaultRegistry() actually registers. Nothing structural keeps
// them in sync — the loop builds the AVAILABLE TOOLS section from the
// registry, but the catalogue is public API that callers may rely on. These
// tests fail whenever the mirror drifts, in either direction: a tool
// described here but never registered (the model would call a tool that
// does not exist), or a registered tool missing from the catalogue.

// toolNameSet extracts the name set from a tool description list.
func toolNameSet(t *testing.T, descs []ToolDescription) map[string]bool {
	set := make(map[string]bool, len(descs))
	for _, d := range descs {
		if set[d.Name] {
			t.Errorf("duplicate tool description for %q", d.Name)
		}
		set[d.Name] = true
	}
	return set
}

// registryToolNames collects tool names from the default registry.
func registryToolNames(toolsList []tools.Tool) map[string]bool {
	set := make(map[string]bool, len(toolsList))
	for _, t := range toolsList {
		set[t.Name()] = true
	}
	return set
}

// assertSameSet fails with a readable diff when the two name sets differ.
func assertSameToolSet(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()

	var missingFromCatalogue, phantom []string
	for name := range want {
		if !got[name] {
			missingFromCatalogue = append(missingFromCatalogue, name)
		}
	}
	for name := range got {
		if !want[name] {
			phantom = append(phantom, name)
		}
	}
	if len(missingFromCatalogue) == 0 && len(phantom) == 0 {
		return
	}
	sort.Strings(missingFromCatalogue)
	sort.Strings(phantom)
	t.Errorf("%s drifted from tools.InitDefaultRegistry():\n  registered but not in catalogue: %v\n  in catalogue but not registered (register it or drop it): %v",
		what, missingFromCatalogue, phantom)
}

// TestToolCatalogueMatchesDefaultRegistry pins GetToolDescriptions (the full
// catalogue of every built-in tool) to the default registry's complete set.
func TestToolCatalogueMatchesDefaultRegistry(t *testing.T) {
	registry := tools.InitDefaultRegistry()
	got := toolNameSet(t, GetToolDescriptions())
	want := registryToolNames(registry.GetAll())
	assertSameToolSet(t, "GetToolDescriptions", got, want)
}

// TestActModeCatalogueMatchesRegistry pins GetActModeToolDescriptions to the
// act-mode subset of the default registry.
func TestActModeCatalogueMatchesRegistry(t *testing.T) {
	registry := tools.InitDefaultRegistry()
	got := toolNameSet(t, GetActModeToolDescriptions())
	want := registryToolNames(registry.GetForMode("act"))
	assertSameToolSet(t, "GetActModeToolDescriptions", got, want)
}

// TestPlanModeCatalogueMatchesRegistry pins GetPlanModeToolDescriptions to
// the plan-mode subset of the default registry.
func TestPlanModeCatalogueMatchesRegistry(t *testing.T) {
	registry := tools.InitDefaultRegistry()
	got := toolNameSet(t, GetPlanModeToolDescriptions())
	want := registryToolNames(registry.GetForMode("plan"))
	assertSameToolSet(t, "GetPlanModeToolDescriptions", got, want)
}
