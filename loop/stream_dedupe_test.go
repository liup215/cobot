package loop

// Duplicate-delivery defence in processStream. The provider already refuses to
// re-run its finish block (a repeated finish chunk is a gateway quirk), but a
// duplicate complete tool call must ALSO be dropped here: it is the seam every
// provider passes through, and the failure mode is not cosmetic — the user sees
// two identical cards and the tool body (an `ls`, a query, a write) runs twice.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// countingTool records how many times its body actually ran.
type countingTool struct {
	name string

	mu    sync.Mutex
	calls int
}

func (c *countingTool) Name() string        { return c.name }
func (c *countingTool) Description() string { return "counts executions" }
func (c *countingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (c *countingTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return "ok", nil
}

func (c *countingTool) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestProcessStreamDropsDuplicateCompleteToolCall(t *testing.T) {
	tool := &countingTool{name: "ls"}
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: tool, AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: &hangingStreamProvider{}, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}

	// Two complete deliveries of the SAME call, exactly what a re-run finish
	// block produces (same ID, same name, same input).
	duplicate := provider.ToolCall{ID: "call_dup", Name: "ls", Input: `{"path":"C:/tmp"}`}
	other := provider.ToolCall{ID: "call_other", Name: "ls", Input: `{"path":"C:/other"}`}

	ch := make(chan provider.StreamChunk, 6)
	ch <- provider.StreamChunk{ToolCall: &duplicate}
	ch <- provider.StreamChunk{ToolCall: &other}
	copied := duplicate
	ch <- provider.StreamChunk{ToolCall: &copied}
	ch <- provider.StreamChunk{Done: true, FinishReason: "tool_calls"}
	close(ch)

	cb := &recordingCallback{}
	delivered, err := agent.processStream(context.Background(), ch, cb, newRunStats())
	if err != nil {
		t.Fatalf("processStream: %v", err)
	}

	if delivered != 2 {
		t.Fatalf("delivered = %d complete tool calls, want 2 (duplicate dropped)", delivered)
	}
	if got := len(cb.toolStarts); got != 2 {
		t.Errorf("OnToolCallStart fired %d times, want 2 — a duplicate must not reach the UI", got)
	}

	// The assistant turn recorded in the transcript is what persistence writes
	// and what the next request replays: it must hold each call once.
	var recorded []string
	for _, m := range agent.conversation.GetMessages() {
		for _, tc := range m.ToolCalls {
			recorded = append(recorded, tc.ID)
		}
	}
	if len(recorded) != 2 {
		t.Fatalf("recorded tool calls = %v, want exactly [call_dup call_other]", recorded)
	}
	seen := map[string]int{}
	for _, id := range recorded {
		seen[id]++
	}
	if seen["call_dup"] != 1 {
		t.Errorf("call_dup recorded %d times, want 1", seen["call_dup"])
	}

	// Pre-dispatch runs the body in the background; give it a moment and then
	// assert the tool itself only ran once.
	agent.preDispatchWG.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for tool.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := tool.count(); got != 2 {
		t.Errorf("tool executed %d times, want 2 (one per distinct call, not per delivery)", got)
	}
}
