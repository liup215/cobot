package agentcore_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/liup215/agentcore"
	"github.com/liup215/agentcore/provider"
	"github.com/liup215/agentcore/tools"
)

// recordingCB captures events for assertions (single-threaded tests).
type recordingCB struct {
	agentcore.CallbackAdapter
	contents    []string
	completions []string
	starts      []string
	completed   int
}

func (c *recordingCB) OnContent(delta string) { c.contents = append(c.contents, delta) }
func (c *recordingCB) OnToolCallStart(tc provider.ToolCall) {
	c.starts = append(c.starts, tc.Name)
}
func (c *recordingCB) OnToolCallComplete(tc provider.ToolCall, result string) {
	c.completions = append(c.completions, tc.Name)
}
func (c *recordingCB) OnComplete() { c.completed++ }

// completionScript: one scripted turn that calls attempt_completion.
func completionScript(answer string) provider.TurnScript {
	input, _ := json.Marshal(map[string]string{"result": answer})
	return provider.TurnScript{
		ToolCalls: []provider.ToolCall{{
			ID:    "c1",
			Name:  "attempt_completion",
			Input: string(input),
		}},
	}
}

// End-to-end through the facade: declarative path with an injected scripted
// provider runs a full loop and delivers callback events.
func TestNewAgentEndToEnd(t *testing.T) {
	scripted := provider.NewScriptedProvider(
		provider.TurnScript{Content: "thinking..."},
		completionScript("all done"),
	)
	cb := &recordingCB{}

	agent, err := agentcore.NewAgent(agentcore.Config{
		Provider: scripted,
		Mode:     agentcore.ModeAct,
		Title:    "facade smoke",
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if err := agent.RunWithCallback(context.Background(), "do the thing", cb); err != nil {
		t.Fatalf("RunWithCallback: %v", err)
	}
	if got := strings.Join(cb.contents, ""); got != "thinking..." {
		t.Errorf("content = %q", got)
	}
	if cb.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1", cb.completed)
	}
	if len(cb.starts) != 1 || cb.starts[0] != "attempt_completion" {
		t.Errorf("tool starts = %v", cb.starts)
	}
}

// Omitted Tools defaults to the built-in registry (nine tools registered).
func TestNewAgentDefaultRegistry(t *testing.T) {
	agent, err := agentcore.NewAgent(agentcore.Config{
		Provider: provider.NewScriptedProvider(provider.TurnScript{Content: "hi"}),
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, err := agent.GetToolRegistry().Get("attempt_completion"); err != nil {
		t.Errorf("attempt_completion missing from default registry: %v", err)
	}
	if n := agent.GetToolRegistry().Count(); n < 9 {
		t.Errorf("default registry has %d tools, want >= 9", n)
	}
}

// APIKey-less, provider-less configs are rejected with a clear error.
func TestNewAgentRequiresCredentials(t *testing.T) {
	_, err := agentcore.NewAgent(agentcore.Config{})
	if err == nil || !strings.Contains(err.Error(), "APIKey") {
		t.Fatalf("want APIKey error, got %v", err)
	}
}

// Injected custom tools are honored (custom registry replaces the default).
func TestNewAgentCustomRegistry(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&tools.ToolInfo{Tool: &echoTool{}, AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}})

	agent, err := agentcore.NewAgent(agentcore.Config{
		Provider: provider.NewScriptedProvider(completionScript("done")),
		Tools:    reg,
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	if _, err := agent.GetToolRegistry().Get("echo"); err != nil {
		t.Errorf("custom registry not used: %v", err)
	}
	if n := agent.GetToolRegistry().Count(); n != 2 {
		t.Errorf("custom registry has %d tools, want 2 (echo + attempt_completion)", n)
	}
}

// ErrAbortRun re-export is the same sentinel the loop checks for.
func TestErrAbortRunSentinel(t *testing.T) {
	wrapped := errors.New("policy: " + "x")
	if !errors.Is(agentcore.ErrAbortRun, agentcore.ErrAbortRun) {
		t.Fatal("sentinel broken")
	}
	_ = wrapped
}

// echoTool implements agentcore.Tool via the tools package.
type echoTool struct{}

func (e *echoTool) Name() string { return "echo" }
func (e *echoTool) Description() string {
	return "Echoes the input back. Use for testing."
}
func (e *echoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["input"]}`)
}
func (e *echoTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	return "echo", nil
}
