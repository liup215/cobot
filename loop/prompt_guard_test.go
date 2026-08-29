package loop

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// These tests guard against drift between the system prompt and the default
// tool registry. The prompt is free prose, so nothing structurally stops a
// sentence from advertising a tool the Registry does not implement (this
// actually happened: list_code_definition_names was described in the
// CAPABILITIES section while unregistered, so every call would have failed
// with "tool not found"). These tests fail fast when the two diverge again.

// toolNameShape matches snake_case identifiers — the shape of every built-in
// tool name ("read_file", "list_code_definition_names", ...).
var toolNameShape = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)

// nonToolIdentifiers lists snake_case words the system prompt legitimately
// contains that are NOT tools: tool input-schema property names, prompt
// markup and prose identifiers. A candidate token that is neither registered
// nor listed here fails the guard, so every new entry is a deliberate,
// reviewable decision: either a tool was mentioned in prose without being
// registered (register it or drop the mention), or a genuinely non-tool
// identifier was introduced (add it here).
var nonToolIdentifiers = map[string]bool{
	"environment_details": true, // prose: name of the Cline-style env block
	"file_pattern":        true, // input-schema property (search/definition tools)
	"requires_approval":   true, // input-schema property (execute_command)
	"start_line":          true, // parameter referenced by tool descriptions
	"end_line":            true, // parameter referenced by tool descriptions
	"tool_calls":          true, // prose: native function-calling field
	"tool_name":           true, // XML fallback placeholder tag <tool_name>
}

// loopToolDescs mirrors the tool-description building RunWithCallback performs
// before handing them to the system prompt: registry-filtered for the mode,
// plan-mode description filtering applied.
func loopToolDescs(registry *tools.Registry, mode Mode) []prompts.ToolDescription {
	availableTools := registry.GetForMode(string(mode))
	toolDescs := make([]prompts.ToolDescription, 0, len(availableTools))
	for _, tool := range availableTools {
		toolDescs = append(toolDescs, prompts.ToolDescription{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: string(tool.InputSchema()),
		})
	}
	if mode == ModePlan {
		toolDescs = filterPlanModeTools(toolDescs)
	}
	return toolDescs
}

// buildLoopSystemPrompt builds the system prompt exactly the way
// RunWithCallback does: through BaseAgent.systemPromptFor with the default
// registry, no custom rules and no override (the defaults this guard is
// responsible for).
func buildLoopSystemPrompt(mode Mode) string {
	agent := &BaseAgent{mode: mode, toolRegistry: tools.InitDefaultRegistry()}
	return agent.systemPromptFor(loopToolDescs(agent.toolRegistry, mode))
}

// scrubEnvPaths removes machine-specific paths that GetSystemPrompt embeds
// (current working directory, home directory) so token extraction does not
// depend on the checkout location.
func scrubEnvPaths(prompt string) string {
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		prompt = strings.ReplaceAll(prompt, cwd, ".")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		prompt = strings.ReplaceAll(prompt, home, ".")
	}
	return prompt
}

// registeredToolNames returns the full set of tool names in the default
// registry, regardless of mode.
func registeredToolNames() map[string]bool {
	names := make(map[string]bool)
	for _, tool := range tools.InitDefaultRegistry().GetAll() {
		names[tool.Name()] = true
	}
	return names
}

// TestSystemPromptOnlyMentionsRegisteredTools asserts that every tool name
// the system prompt tells the model about exists in the default registry,
// in both act and plan mode.
func TestSystemPromptOnlyMentionsRegisteredTools(t *testing.T) {
	registered := registeredToolNames()

	for _, mode := range []Mode{ModeAct, ModePlan} {
		prompt := scrubEnvPaths(buildLoopSystemPrompt(mode))

		for _, candidate := range toolNameShape.FindAllString(prompt, -1) {
			if registered[candidate] || nonToolIdentifiers[candidate] {
				continue
			}
			t.Errorf("mode %s: system prompt mentions %q but it is not registered in tools.InitDefaultRegistry(); either register the tool or drop the mention (add it to nonToolIdentifiers only if it is not a tool)",
				mode, candidate)
		}
	}
}

// TestSystemPromptDescribesEveryRegisteredTool asserts the reverse direction:
// every tool the registry exposes for a mode is described in that mode's
// system prompt, so the model is never silently left without a tool that the
// registry would have executed.
func TestSystemPromptDescribesEveryRegisteredTool(t *testing.T) {
	for _, mode := range []Mode{ModeAct, ModePlan} {
		registry := tools.InitDefaultRegistry()
		prompt := buildLoopSystemPrompt(mode)

		var missing []string
		for _, tool := range registry.GetForMode(string(mode)) {
			if !strings.Contains(prompt, tool.Name()) {
				missing = append(missing, tool.Name())
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			t.Errorf("mode %s: registry tools never mentioned in the system prompt: %v", mode, missing)
		}
	}
}

// toolHeadingPattern matches the "## <tool name>" headings of the AVAILABLE
// TOOLS section the loop renders from the registry.
var toolHeadingPattern = regexp.MustCompile(`(?m)^## ([a-z0-9_]+)$`)

// TestAvailableToolsSectionMatchesRegistry asserts that the AVAILABLE TOOLS
// section of the prompt only advertises tools the registry allows in the
// current mode (mode filtering stays consistent between the loop, the
// prompt catalogue and the registry's AllowedModes).
func TestAvailableToolsSectionMatchesRegistry(t *testing.T) {
	for _, mode := range []Mode{ModeAct, ModePlan} {
		registry := tools.InitDefaultRegistry()
		prompt := buildLoopSystemPrompt(mode)

		headings := toolHeadingPattern.FindAllStringSubmatch(prompt, -1)
		if len(headings) == 0 {
			t.Fatalf("mode %s: no AVAILABLE TOOLS section found in the system prompt", mode)
		}
		for _, h := range headings {
			if !registry.IsAllowed(string(mode), h[1]) {
				t.Errorf("mode %s: prompt describes tool %q which the registry does not allow in this mode", mode, h[1])
			}
		}
	}
}

// ─── SystemPromptOverride guard (host takeover) ──────────────────────────────

// In override mode the host authors the whole prompt, so the static
// prompt↔registry catalog checks do not apply: the host text may legitimately
// mention any identifier it wants (e.g. Maestro's submit_plan /
// update_step / report_progress). What must still hold — and what this test
// pins — is that the loop still renders the AVAILABLE TOOLS section from the
// registry, that it advertises exactly the tools allowed in the current mode,
// and that no default-prompt section leaks into the host's text.
func TestSystemPromptOverrideKeepsToolSection(t *testing.T) {
	override := "你是嵌入式宿主注入的完整角色提示词。你可以调用 submit_plan、update_step 与 report_progress 工具向宿主汇报。"
	registry := tools.InitDefaultRegistry()

	for _, mode := range []Mode{ModeAct, ModePlan} {
		agent := &BaseAgent{
			mode:                 mode,
			toolRegistry:         registry,
			systemPromptOverride: override,
		}
		prompt := agent.systemPromptFor(loopToolDescs(registry, mode))

		// The host text survives verbatim and comes first.
		if !strings.HasPrefix(prompt, override) {
			t.Errorf("mode %s: override prompt does not start with the host text verbatim", mode)
		}

		// The loop-rendered tool section is present and consistent with the
		// registry's mode filtering.
		headings := toolHeadingPattern.FindAllStringSubmatch(prompt, -1)
		if len(headings) == 0 {
			t.Fatalf("mode %s: no AVAILABLE TOOLS section found in the override prompt", mode)
		}
		for _, h := range headings {
			if !registry.IsAllowed(string(mode), h[1]) {
				t.Errorf("mode %s: override prompt describes tool %q which the registry does not allow in this mode", mode, h[1])
			}
		}
		for _, tool := range registry.GetForMode(string(mode)) {
			if !strings.Contains(prompt, tool.Name()) {
				t.Errorf("mode %s: registry tool %q missing from the override prompt's tool section", mode, tool.Name())
			}
		}

		// No default-prompt section leaks into the host's prompt.
		defaultPrompt := buildLoopSystemPrompt(mode)
		if prompt == defaultPrompt {
			t.Fatalf("mode %s: override produced the default prompt unchanged", mode)
		}
		for _, section := range []string{"You are ", "CAPABILITIES", "EDITING FILES", "RULES"} {
			if strings.Contains(prompt, section) {
				t.Errorf("mode %s: default prompt section %q leaked into the override prompt", mode, section)
			}
		}

		// The static tool-name catalog assertions are intentionally skipped
		// here: the host-authored text may reference identifiers (submit_plan,
		// update_step, report_progress) that are host-side native tools rather
		// than registry entries. Asserting them would defeat the purpose of
		// host takeover. The loop-side contract (tool section ⊆ registry) is
		// what is guarded above.
	}
}

// A run with SystemPromptOverride set sends the host's prompt (plus the
// loop-rendered tool section) to the provider — verified on the actual
// request, not just on the prompt builder.
func TestRunWithSystemPromptOverrideSendsOverride(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	agent := newEchoCompletionAgent(t, prov)
	agent.systemPromptOverride = "宿主注入的角色提示词：只输出中文。"

	if _, err := agent.Run(context.Background(), "task"); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := prov.Requests()
	if len(reqs) == 0 {
		t.Fatal("provider captured no request")
	}
	sp := reqs[0].SystemPrompt
	if !strings.HasPrefix(sp, "宿主注入的角色提示词：只输出中文。") {
		t.Errorf("system prompt does not start with the host override: %.80q", sp)
	}
	if !strings.Contains(sp, "AVAILABLE TOOLS:") {
		t.Error("the request's system prompt lacks the loop-rendered AVAILABLE TOOLS section")
	}
	if strings.Contains(sp, "CAPABILITIES") {
		t.Error("default prompt section CAPABILITIES leaked into the override prompt")
	}
}
