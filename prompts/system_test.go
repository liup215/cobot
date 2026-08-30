package prompts

import (
	"strings"
	"testing"
)

// The agent identity is announced in the role section: an empty name falls
// back to DefaultAgentName, and a custom name replaces it verbatim. The
// legacy hard-coded identity must never leak through.
func TestGetSystemPromptAgentName(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		p := GetSystemPrompt("act", nil, "", "")
		if !strings.HasPrefix(p, "You are "+DefaultAgentName+",") {
			t.Errorf("prompt should open with the default agent identity, got %.120q", p)
		}
		if strings.Contains(p, "agentcore") {
			t.Error("legacy identity agentcore leaked into the system prompt")
		}
	})

	t.Run("custom", func(t *testing.T) {
		p := GetSystemPrompt("act", nil, "", "maestro-worker")
		if !strings.HasPrefix(p, "You are maestro-worker,") {
			t.Errorf("prompt should open with the custom agent identity, got %.120q", p)
		}
	})
}

// ─── ApplySystemPromptOverride (host takeover) ───────────────────────────────

// A non-empty override replaces the default prompt verbatim: it opens with the
// host's own text (no cobot role/identity sections prepended) and only the
// AVAILABLE TOOLS section rendered from the registry is appended after it.
func TestApplySystemPromptOverrideReplacesDefaultAndKeepsTools(t *testing.T) {
	override := "你是一个资深软件工程师。请用中文回答，并严格遵循宿主的输出规范。"
	tools := []ToolDescription{
		{
			Name:        "submit_plan",
			Description: "Submit the plan for the task.",
			InputSchema: `{"type":"object","properties":{"steps":{"type":"array"}}}`,
		},
		{Name: "update_step", Description: "Report step progress.", InputSchema: "{}"},
	}
	got := ApplySystemPromptOverride(override, tools)

	if !strings.HasPrefix(got, override) {
		t.Errorf("prompt must start with the host override verbatim, got %.80q", got)
	}
	if !strings.Contains(got, "AVAILABLE TOOLS:") {
		t.Error("the AVAILABLE TOOLS section must still be rendered in override mode")
	}
	if !strings.Contains(got, "## submit_plan\n") || !strings.Contains(got, "## update_step\n") {
		t.Errorf("tool descriptions missing from the override prompt:\n%s", got)
	}
	if !strings.Contains(got, `"properties":{"steps":{"type":"array"}}`) {
		t.Error("the tool input schema must be included verbatim")
	}
	// The default prompt's own sections must not leak in.
	for _, leaked := range []string{"You are ", "CAPABILITIES", "TOOL USE\n\nYou have access to tools", "EDITING FILES"} {
		if strings.Contains(got, leaked) {
			t.Errorf("default prompt section %q leaked into the override prompt", leaked)
		}
	}
}

// An empty override returns "" so callers fall back to GetSystemPrompt, and an
// override with no tools available renders the override alone.
func TestApplySystemPromptOverrideEdgeCases(t *testing.T) {
	if got := ApplySystemPromptOverride("", []ToolDescription{{Name: "x"}}); got != "" {
		t.Errorf("empty override = %q, want empty", got)
	}
	override := "host prompt with no tool section needed"
	if got := ApplySystemPromptOverride(override, nil); got != override {
		t.Errorf("override with no tools = %q, want the override alone %q", got, override)
	}
}
