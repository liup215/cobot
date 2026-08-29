package loop

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// ─── headless interaction (Maestro has no human to answer) ───────────────────

// headlessCallback is a callback whose AskFollowupQuestion always fails: the
// embedded-host situation where no human is available to answer.
type headlessCallback struct {
	recordingCallback
	err error
	// asks counts how many times the host was asked.
	asks atomic.Int32
}

func (c *headlessCallback) AskFollowupQuestion(q string, o []string) (string, error) {
	c.asks.Add(1)
	return "", c.err
}

func askCall(id, question string) provider.ToolCall {
	return provider.ToolCall{
		ID:    id,
		Name:  types.ToolAskFollowupQuestion.String(),
		Input: `{"question":` + strconv.Quote(question) + `,"options":["main","dev"]}`,
	}
}

// The pinned headless contract: when the AskFollowupQuestion callback returns
// an error, the error text is fed back to the model as the tool result and the
// run CONTINUES — the model keeps control and decides how to proceed (here it
// ends the run with attempt_completion). No panic, no deadlock, no run-level
// failure, no whole-run invalidation.
func TestAskFollowupErrorFeedsResultBackAndContinues(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{askCall("c1", "which branch should I use?")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "proceeded with a documented assumption")}},
	)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAskFollowupQuestionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	cb := &headlessCallback{err: errors.New("headless: no human to answer")}

	res, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err != nil {
		t.Fatalf("run: %v (an ask_followup handler error must not fail the run)", err)
	}
	if res == nil || res.Err != nil {
		t.Fatalf("result = %+v, want a clean result", res)
	}
	if cb.asks.Load() != 1 {
		t.Errorf("ask callback invoked %d times, want 1", cb.asks.Load())
	}
	if got := prov.TurnCount(); got != 2 {
		t.Errorf("turn count = %d, want 2 (the loop continued after the ask error)", got)
	}

	// The error text reached the model as the c1 tool result.
	fedBack := toolResultFor(t, agent, "c1")
	if !strings.Contains(fedBack, "headless: no human to answer") {
		t.Errorf("ask error not fed back as the tool result: %q", fedBack)
	}

	// The run ended normally: no run-level OnError, exactly one OnComplete,
	// and the model's own deliverable is the result.
	snap := cb.snapshot()
	if len(snap.errors) != 0 {
		t.Errorf("OnError fired %d times for an ask_followup tool error, want 0", len(snap.errors))
	}
	if snap.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1", snap.completed)
	}
	if res.FinalText != "proceeded with a documented assumption" {
		t.Errorf("FinalText = %q, want the model's attempt_completion result", res.FinalText)
	}
}

// Repeated ask_followup failures keep the loop alive: every error round-trips
// as a tool result and the model retains control of the conversation.
func TestAskFollowupErrorRoundTripsEachTime(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{askCall("c1", "first question?")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{askCall("c2", "second question?")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c3", "gave up asking, proceeded")}},
	)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAskFollowupQuestionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	cb := &headlessCallback{err: errors.New("headless: no human to answer")}

	res, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if cb.asks.Load() != 2 {
		t.Errorf("ask callback invoked %d times, want 2", cb.asks.Load())
	}
	if got := prov.TurnCount(); got != 3 {
		t.Errorf("turn count = %d, want 3", got)
	}
	for _, id := range []string{"c1", "c2"} {
		if got := toolResultFor(t, agent, id); !strings.Contains(got, "headless: no human to answer") {
			t.Errorf("tool result for %s = %q, want the fed-back error text", id, got)
		}
	}
	if res.FinalText != "gave up asking, proceeded" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if agent.GetConversation().IsComplete() != true {
		t.Error("conversation not marked complete after attempt_completion")
	}
}

// toolResultFor returns the tool-result message content recorded for the given
// tool call ID.
func toolResultFor(t *testing.T, agent *BaseAgent, callID string) string {
	t.Helper()
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleTool && m.ToolCallID == callID {
			return m.Content
		}
	}
	t.Fatalf("no tool result message found for call %s", callID)
	return ""
}
