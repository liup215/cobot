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
