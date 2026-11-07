package loop

import (
	"strings"
	"testing"

	"github.com/liup215/cobot/skills"
	"github.com/liup215/cobot/tools"
)

// The SKILLS section is appended to the system prompt in BOTH prompt paths
// (host override and default) so a host that takes over the prompt still
// gets progressive-disclosure skill advertising without restating it.
func TestSystemPromptIncludesSkillsSection(t *testing.T) {
	skill := skills.Skill{
		Name:        "question-authoring",
		Description: "Create and edit exam questions. Use when authoring questions.",
		Path:        `C:\data\skills\question-authoring\SKILL.md`,
	}
	registry := tools.InitDefaultRegistry()

	t.Run("override path", func(t *testing.T) {
		agent := &BaseAgent{
			mode:                 ModeAct,
			toolRegistry:         registry,
			systemPromptOverride: "host persona",
			skills:               []skills.Skill{skill},
		}
		prompt := agent.systemPromptFor(loopToolDescs(registry, ModeAct))
		if !strings.Contains(prompt, "SKILLS") {
			t.Error("override prompt missing SKILLS section")
		}
		if !strings.Contains(prompt, "- question-authoring: Create and edit exam questions.") {
			t.Errorf("override prompt missing skill bullet:\n%s", prompt)
		}
		if !strings.Contains(prompt, `SKILL.md: C:\data\skills\question-authoring\SKILL.md`) {
			t.Error("override prompt missing SKILL.md path")
		}
		// Section comes after the host text and after the tool section.
		if strings.Index(prompt, "SKILLS") < strings.Index(prompt, "host persona") {
			t.Error("SKILLS section must come after the host text")
		}
	})

	t.Run("default path", func(t *testing.T) {
		agent := &BaseAgent{
			mode:         ModeAct,
			toolRegistry: registry,
			skills:       []skills.Skill{skill},
		}
		prompt := agent.systemPromptFor(loopToolDescs(registry, ModeAct))
		if !strings.Contains(prompt, "SKILLS") || !strings.Contains(prompt, "question-authoring") {
			t.Error("default prompt missing SKILLS section")
		}
	})

	t.Run("no skills means no section", func(t *testing.T) {
		agent := &BaseAgent{mode: ModeAct, toolRegistry: registry}
		prompt := agent.systemPromptFor(loopToolDescs(registry, ModeAct))
		if strings.Contains(prompt, "\nSKILLS\n") {
			t.Error("prompt must not contain a SKILLS section when no skills are set")
		}
	})
}

// SetSkills swaps the advertised set between runs and clears cleanly.
func TestSetSkillsSwapsSection(t *testing.T) {
	agent := &BaseAgent{mode: ModeAct, toolRegistry: tools.InitDefaultRegistry()}

	agent.SetSkills([]skills.Skill{{Name: "a", Description: "A skill.", Path: "/p/a/SKILL.md"}})
	if got := agent.skillsSection(); !strings.Contains(got, "- a: A skill.") {
		t.Errorf("after SetSkills, section = %q", got)
	}

	agent.SetSkills(nil)
	if got := agent.skillsSection(); got != "" {
		t.Errorf("after clearing, section = %q, want empty", got)
	}
}

// disable-model-invocation skills are discovered but never advertised.
func TestSkillsSectionSkipsDisabledSkills(t *testing.T) {
	agent := &BaseAgent{mode: ModeAct, toolRegistry: tools.InitDefaultRegistry()}
	agent.SetSkills([]skills.Skill{{
		Name: "hidden", Description: "Not for the model.", Path: "/p/h/SKILL.md",
		DisableModelInvocation: true,
	}})
	if got := agent.skillsSection(); got != "" {
		t.Errorf("disabled-only skill must yield empty section, got %q", got)
	}
}

// The default prompt still describes every registered tool when skills are
// present (the guard's core invariant must not regress with the new tail
// section).
func TestPromptGuardInvariantWithSkills(t *testing.T) {
	agent := &BaseAgent{
		mode:         ModeAct,
		toolRegistry: tools.InitDefaultRegistry(),
		skills:       []skills.Skill{{Name: "x", Description: "X.", Path: "/p/x/SKILL.md"}},
	}
	prompt := agent.systemPromptFor(loopToolDescs(agent.toolRegistry, ModeAct))
	for _, tool := range agent.toolRegistry.GetForMode(string(ModeAct)) {
		if !strings.Contains(prompt, tool.Name()) {
			t.Errorf("tool %q missing from prompt with skills", tool.Name())
		}
	}
}
