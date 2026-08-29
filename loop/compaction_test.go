package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// recordingStrategy is a compact.Strategy stub that records every Compact
// call, so tests can verify that the loop wires Options.Compaction into the
// token-budget trigger points.
type recordingStrategy struct {
	mu       sync.Mutex
	compacts int
	lastConv *types.Conversation
}

func (s *recordingStrategy) ShouldCompact(*types.Conversation) bool { return true }

func (s *recordingStrategy) Compact(_ context.Context, conv *types.Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compacts++
	s.lastConv = conv
	return nil
}

func (s *recordingStrategy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compacts
}

func (s *recordingStrategy) conversation() *types.Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastConv
}

// TestOptionsCompactionRunsInjectedStrategyAtBudgetHook pins the
// Options.Compaction wiring at the 60% pre-request budget hook
// (enforceTokenBudget): the injected strategy runs against the live
// conversation before the first provider request. The budget is sized so only
// this hook can fire: the conversation alone stays below the 80% auto-compact
// line and the 100% pre-send hard trim, while conversation + system prompt +
// tool descriptions cross the 60% budget line.
func TestOptionsCompactionRunsInjectedStrategyAtBudgetHook(t *testing.T) {
	strategy := &recordingStrategy{}
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{
		Tool:         tools.NewAttemptCompletionTool(),
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{
		Provider:     prov,
		ToolRegistry: reg,
		Compaction:   strategy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.GetCompaction().(*recordingStrategy); !ok {
		t.Fatalf("Options.Compaction was not stored: got %T", agent.GetCompaction())
	}

	// Measure the request overhead the loop adds on top of the conversation
	// (system prompt + marshaled tool descriptions) — the same arithmetic
	// enforceTokenBudget uses — then set MaxTokens to conversation + overhead.
	// The upcoming request (= conversation + overhead) is then above the 60%
	// line of MaxTokens, while the conversation alone stays far below 80%.
	availableTools := reg.GetForMode(string(ModeAct))
	toolDescs := make([]prompts.ToolDescription, 0, len(availableTools))
	for _, tool := range availableTools {
		toolDescs = append(toolDescs, prompts.ToolDescription{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: string(tool.InputSchema()),
		})
	}
	overhead := types.Estimator(prompts.GetSystemPrompt(string(ModeAct), toolDescs, "", prompts.DefaultAgentName))
	if toolJSON, err := json.Marshal(availableTools); err == nil {
		overhead += types.Estimator(string(toolJSON))
	}
	const prompt = "fix the login bug"
	agent.conversation.MaxTokens = types.Estimator(prompt) + overhead
	agent.conversation.ResponseBuffer = 0

	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), prompt, cb); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := strategy.callCount(); got != 1 {
		t.Fatalf("injected strategy Compact called %d times, want 1 (60%% budget hook)", got)
	}
	if strategy.conversation() != agent.GetConversation() {
		t.Error("the strategy must be handed the agent's live conversation")
	}
	hookFired := false
	for _, reason := range cb.snapshot().compactionStarts {
		if strings.Contains(reason, "60% threshold") {
			hookFired = true
		}
	}
	if !hookFired {
		t.Errorf("no 60%% budget compaction event fired, got %v", cb.snapshot().compactionStarts)
	}
}
