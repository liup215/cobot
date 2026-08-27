package loop

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/liup215/cobot/prompts"
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

// buildLoopSystemPrompt builds the system prompt exactly the way
// RunWithCallback does: registry-filtered tool descriptions for the mode,
// plan-mode description filtering, no custom rules and no skills (the
// defaults this guard is responsible for).
func buildLoopSystemPrompt(mode Mode) string {
	registry := tools.InitDefaultRegistry()
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
	return prompts.GetSystemPrompt(string(mode), toolDescs, "", nil)
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
