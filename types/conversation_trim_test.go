package types

import (
	"strings"
	"testing"
)

// pinEstimator makes token counts equal to rune counts so the budget math in
// these tests is exact and readable.
func pinEstimator(t *testing.T) {
	t.Helper()
	orig := Estimator
	Estimator = func(s string) int { return len([]rune(s)) }
	t.Cleanup(func() { Estimator = orig })
}

// TestTrimToBudgetStopsAtTheTarget pins the budget-aware hard trim: it must
// land at the given target (the loop passes the hard watermark), not at
// MaxTokens. Trimming to the window itself leaves the request at 100% of the
// window — the point where a provider starts silently dropping tokens — and
// re-arms the compaction trigger for the very next iteration.
func TestTrimToBudgetStopsAtTheTarget(t *testing.T) {
	pinEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 1000
	conv.PerMessageSoftCap = 0
	for i := 0; i < 8; i++ {
		conv.AddMessage(Message{Role: RoleUser, Content: strings.Repeat("u", 200)})
		conv.AddMessage(Message{Role: RoleAssistant, Content: strings.Repeat("a", 200)})
	}
	if before := conv.GetTotalTokens(); before <= 1000 {
		t.Fatalf("fixture is not over the window: %d", before)
	}

	conv.TrimToBudget(600)
	if got := conv.GetTotalTokens(); got > 600 {
		t.Fatalf("TrimToBudget(600) left %d tokens", got)
	}
	if conv.GetTotalTokens() >= conv.MaxTokens {
		t.Fatalf("the trim did nothing useful: %d", conv.GetTotalTokens())
	}
}

// TestTrimToBudgetNeverLeavesAnOrphanToolResult pins the wire-format
// invariant: both the OpenAI-compatible and the Anthropic formats reject a
// tool result whose requesting assistant message is gone. A front-dropping
// trim splits tool_call/tool_result pairs unless it cleans up after itself.
func TestTrimToBudgetNeverLeavesAnOrphanToolResult(t *testing.T) {
	pinEstimator(t)

	conv := NewConversation()
	conv.MaxTokens = 1000
	conv.PerMessageSoftCap = 0
	conv.AddMessage(Message{Role: RoleSystem, Content: "system"})
	conv.AddMessage(Message{Role: RoleUser, Content: "task"})
	for i := 0; i < 4; i++ {
		conv.AddMessage(Message{Role: RoleAssistant, Content: strings.Repeat("a", 100), ToolCalls: []ToolCall{{
			ID:    "call-1",
			Name:  "read_file",
			Input: []byte(`{"path":"x"}`),
		}}})
		conv.AddMessage(Message{Role: RoleTool, ToolCallID: "call-1", Content: strings.Repeat("t", 400)})
		conv.AddMessage(Message{Role: RoleUser, Content: strings.Repeat("u", 100)})
	}

	conv.TrimToBudget(400)

	if len(conv.Messages) == 0 {
		t.Fatalf("the trim removed everything")
	}
	// Whatever survived, the first non-system message must not be a tool
	// result.
	for i, m := range conv.Messages {
		if m.Role == RoleSystem {
			continue
		}
		if m.Role == RoleTool {
			t.Fatalf("message %d is an orphan tool result after the trim: %+v", i, conv.Messages)
		}
		break
	}
}
