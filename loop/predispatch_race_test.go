package loop

// Reproduction: a side-effectful tool that arrives in the FINAL stream chunk is
// pre-dispatched (goroutine still running when the stream closes), and the main
// loop's takePreDispatchResult misses it because the goroutine has not recorded
// its result yet — so the body runs a SECOND time. Observed in the wild as
// duplicated syllabus/chapter/exam_node rows with only one tool result in the
// transcript.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// slowWriteTool simulates a network write (e.g. POST /v1/chapter/create).
type slowWriteTool struct {
	name  string
	delay time.Duration

	mu    sync.Mutex
	calls int
}

func (s *slowWriteTool) Name() string        { return s.name }
func (s *slowWriteTool) Description() string { return "a slow side-effectful write" }
func (s *slowWriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
}
func (s *slowWriteTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(s.delay):
	}
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	return "created successfully with ID: " + string(rune('0'+n)), nil
}

func (s *slowWriteTool) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestPreDispatchRaceDoesNotDoubleExecuteSlowWrite(t *testing.T) {
	write := &slowWriteTool{name: "chapter_create", delay: 200 * time.Millisecond}

	prov := provider.NewScriptedProvider(
		provider.TurnScript{
			// The tool call is the last thing the model emits, then Done:
			// the goroutine launched by pre-dispatch is still in flight when
			// executeToolCalls starts. Exactly the ESAT-batch pattern.
			ToolCalls: []provider.ToolCall{{ID: "call_w1", Name: "chapter_create", Input: `{"name":"1.1 Cells"}`}},
		},
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{completionCall("call_done", "finished")},
		},
	)

	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: write, AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}

	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := agent.RunWithCallback(ctx, "create the chapter", &recordingCallback{}); err != nil {
		t.Fatalf("RunWithCallback: %v", err)
	}

	if got := write.count(); got != 1 {
		t.Fatalf("chapter_create executed %d times, want 1 — the pre-dispatched run was re-executed by the main loop", got)
	}
}
