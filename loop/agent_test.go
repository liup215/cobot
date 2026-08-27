package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liup215/agentcore/provider"
	"github.com/liup215/agentcore/tools"
	"github.com/liup215/agentcore/types"
)

// ─── test helpers ────────────────────────────────────────────────────────────

// recordingCallback captures every callback event for assertions.
type recordingCallback struct {
	mu              sync.Mutex
	contents        []string
	reasonings      []string
	streamStarts    int
	toolStarts      []provider.ToolCall
	toolCompletions []toolCompletion
	errors          []error
	completed       int
	taskCreated     []string
}

type toolCompletion struct {
	tc     provider.ToolCall
	result string
}

func (c *recordingCallback) OnContent(delta string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.contents = append(c.contents, delta)
}
func (c *recordingCallback) OnReasoning(delta string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasonings = append(c.reasonings, delta)
}
func (c *recordingCallback) OnStreamStart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamStarts++
}
func (c *recordingCallback) OnToolCallStart(tc provider.ToolCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolStarts = append(c.toolStarts, tc)
}
func (c *recordingCallback) OnToolCallComplete(tc provider.ToolCall, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolCompletions = append(c.toolCompletions, toolCompletion{tc, result})
}
func (c *recordingCallback) AskFollowupQuestion(q string, o []string) (string, error) {
	return "answer", nil
}
func (c *recordingCallback) OnError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errors = append(c.errors, err)
}
func (c *recordingCallback) OnComplete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.completed++
}
func (c *recordingCallback) OnTaskCreated(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.taskCreated = append(c.taskCreated, id)
}
func (c *recordingCallback) snapshot() recordingCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Copy fields manually to avoid copying the embedded mutex.
	return recordingCallback{
		contents:        c.contents,
		reasonings:      c.reasonings,
		streamStarts:    c.streamStarts,
		toolStarts:      c.toolStarts,
		toolCompletions: c.toolCompletions,
		errors:          c.errors,
		completed:       c.completed,
		taskCreated:     c.taskCreated,
	}
}

// echoTool is a controllable read-only tool used by the loop tests. It is
// not on the pre-dispatch side-effect blacklist, so it exercises the
// pre-dispatch path.
type echoTool struct {
	mu        sync.Mutex
	execCount int
	seenInput string
}

func (t *echoTool) Name() string        { return "echo" }
func (t *echoTool) Description() string { return "echoes a fixed result" }
func (t *echoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
}
func (t *echoTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.execCount++
	t.seenInput = string(input)
	return "echo-result", nil
}
func (t *echoTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.execCount
}

// newTestAgent builds a BaseAgent over a ScriptedProvider with an echo tool
// and the built-in attempt_completion tool registered.
func newTestAgent(t *testing.T, turns ...provider.TurnScript) (*BaseAgent, *echoTool) {
	t.Helper()
	prov := provider.NewScriptedProvider(turns...)
	reg := tools.NewRegistry()
	echo := &echoTool{}
	if err := reg.Register(&tools.ToolInfo{
		Tool:         echo,
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{
		Tool:         tools.NewAttemptCompletionTool(),
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{
		Provider:     prov,
		ToolRegistry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent, echo
}

func completionCall(id, result string) provider.ToolCall {
	input, _ := json.Marshal(map[string]string{"result": result})
	return provider.ToolCall{ID: id, Name: types.ToolAttemptCompletion.String(), Input: string(input)}
}

func echoCall(id string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: "echo", Input: `{"x":"1"}`}
}

// ─── termination protocol (FR-L1) ────────────────────────────────────────────

// A model that calls attempt_completion ends the run: the conversation is
// marked complete, OnComplete fires exactly once, and the tool executed.
func TestAttemptCompletionTerminatesRun(t *testing.T) {
	agent, echo := newTestAgent(t,
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{echoCall("c1")},
			// Delay keeps stream 1 open long enough for the pre-dispatch
			// goroutine to record its result before the main loop consumes
			// it (mirrors real SSE timing).
			Delay: 30 * time.Millisecond,
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := cb.snapshot()
	if snap.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1", snap.completed)
	}
	if echo.count() != 1 {
		t.Errorf("echo executed %d times, want 1", echo.count())
	}
	msgs := agent.GetConversation().GetMessages()
	// Last message is the tool result; the assistant turn with the
	// attempt_completion call is right before it.
	if len(msgs) < 2 {
		t.Fatalf("conversation too short: %d messages", len(msgs))
	}
	last := msgs[len(msgs)-2]
	if last.Role != types.RoleAssistant || len(last.ToolCalls) == 0 {
		t.Fatalf("unexpected second-to-last message: %+v", last)
	}
	if last.ToolCalls[0].Name != types.ToolAttemptCompletion.String() {
		t.Errorf("last tool call = %s", last.ToolCalls[0].Name)
	}
	if !agent.GetConversation().IsComplete() {
		t.Error("conversation not marked complete")
	}
	// One attempt_completion result must be in the conversation.
	found := false
	for _, m := range msgs {
		if m.Role == types.RoleTool && m.ToolCallID == "c2" && strings.Contains(m.Content, "done") {
			found = true
		}
	}
	if !found {
		t.Error("attempt_completion tool result missing from conversation")
	}
}

// ask_followup_question must NOT terminate the loop (the model continues
// after receiving the user's answer).
func TestAskFollowupDoesNotTerminate(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{{
			ID:    "c1",
			Name:  types.ToolAskFollowupQuestion.String(),
			Input: `{"question":"why?","options":["a","b"]}`,
		}}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{
		Tool:         tools.NewAskFollowupQuestionTool(),
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{
		Tool:         tools.NewAttemptCompletionTool(),
		AllowedModes: []string{"plan", "act"},
	}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !agent.GetConversation().IsComplete() {
		t.Error("conversation should be complete after attempt_completion turn")
	}
	// The loop must have served two turns (followup then completion).
	if prov.TurnCount() != 2 {
		t.Errorf("turn count = %d, want 2", prov.TurnCount())
	}
}

// ─── correction loop (FR-L2) ─────────────────────────────────────────────────

// When tools are available but the model replies with plain text, the loop
// injects noToolsUsedMsg and gives the model another chance.
func TestNoToolsUsedInjectsCorrection(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{Content: "just talking, no tools"},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	var found bool
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleUser && strings.Contains(m.Content, noToolsUsedMsg) {
			found = true
		}
	}
	if !found {
		t.Error("noToolsUsedMsg not injected into conversation")
	}
}

// Three consecutive no-tool replies exhaust the correction budget and the
// run fails with a "max consecutive mistakes" error.
func TestMaxConsecutiveMistakesStopsRun(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{Content: "no tools 1"},
		provider.TurnScript{Content: "no tools 2"},
		provider.TurnScript{Content: "no tools 3"},
	)
	cb := &recordingCallback{}
	err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil || !strings.Contains(err.Error(), "max consecutive mistakes") {
		t.Fatalf("want max-mistakes error, got %v", err)
	}
}

// ─── pre-dispatch (FR-L3) ────────────────────────────────────────────────────

// A complete tool call inside the SSE stream is pre-dispatched: the tool
// executes exactly once (during the stream) and the main loop reuses the
// result without re-executing.
func TestPreDispatchExecutesOnce(t *testing.T) {
	agent, echo := newTestAgent(t,
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{echoCall("c1")},
			Delay:     30 * time.Millisecond,
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if echo.count() != 1 {
		t.Errorf("echo executed %d times, want exactly 1 (pre-dispatch + reuse)", echo.count())
	}
	snap := cb.snapshot()
	echoStarts := 0
	for _, tc := range snap.toolStarts {
		if tc.ID == "c1" {
			echoStarts++
		}
	}
	if echoStarts != 1 {
		t.Errorf("echo OnToolCallStart fired %d times, want exactly 1 (pre-dispatch)", echoStarts)
	}
	if len(snap.toolCompletions) != 2 {
		t.Fatalf("OnToolCallComplete fired %d times, want 2", len(snap.toolCompletions))
	}
	if snap.toolCompletions[0].result != "echo-result" {
		t.Errorf("echo result = %q", snap.toolCompletions[0].result)
	}
	if echo.seenInput != `{"x":"1"}` {
		t.Errorf("echo input = %q", echo.seenInput)
	}
}

// ─── ToolGuard (FR-T1) ───────────────────────────────────────────────────────

// A guard denial feeds "[DENIED] <reason>" back to the model as the tool
// result and the loop continues to the next turn.
func TestGuardDenyFeedsReasonBack(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	reg := tools.NewRegistry()
	echo := &echoTool{}
	reg.Register(&tools.ToolInfo{Tool: echo, AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}})
	agent, err := New(Options{
		Provider:     prov,
		ToolRegistry: reg,
		Guard: func(ctx context.Context, call GuardedCall) (bool, string, error) {
			if call.Name == "echo" {
				return false, "echo is not allowed today", nil
			}
			return true, "", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if echo.count() != 0 {
		t.Errorf("denied tool executed %d times", echo.count())
	}
	var denied string
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleTool && m.ToolCallID == "c1" {
			denied = m.Content
		}
	}
	if denied != "[DENIED] echo is not allowed today" {
		t.Errorf("deny result = %q", denied)
	}
	snap := cb.snapshot()
	if len(snap.toolCompletions) == 0 || snap.toolCompletions[0].result != denied {
		t.Errorf("deny not surfaced via OnToolCallComplete: %+v", snap.toolCompletions)
	}
	// Loop continued and completed.
	if snap.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1", snap.completed)
	}
}

// A guard returning ErrAbortRun aborts the entire run; the denied tool is
// never executed.
func TestGuardErrAbortRunAbortsRun(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}},
	)
	reg := tools.NewRegistry()
	echo := &echoTool{}
	reg.Register(&tools.ToolInfo{Tool: echo, AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}})
	agent, err := New(Options{
		Provider:     prov,
		ToolRegistry: reg,
		Guard: func(ctx context.Context, call GuardedCall) (bool, string, error) {
			return false, "", fmt.Errorf("policy: %w", ErrAbortRun)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if echo.count() != 0 {
		t.Errorf("tool executed despite ErrAbortRun")
	}
	snap := cb.snapshot()
	if snap.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1 (abort exits the loop cleanly)", snap.completed)
	}
	if agent.IsRunning() {
		t.Error("agent still marked running after abort")
	}
}

// An unexpected guard error (not ErrAbortRun) fails closed: the call is
// denied with the error text as the reason, but the run continues.
func TestGuardUnexpectedErrorFailsClosed(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	reg := tools.NewRegistry()
	echo := &echoTool{}
	reg.Register(&tools.ToolInfo{Tool: echo, AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}})
	agent, err := New(Options{
		Provider:     prov,
		ToolRegistry: reg,
		Guard: func(ctx context.Context, call GuardedCall) (bool, string, error) {
			if call.Name == types.ToolAttemptCompletion.String() {
				return true, "", nil // let the model end the run
			}
			return false, "", errors.New("redis down")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if echo.count() != 0 {
		t.Errorf("tool executed despite guard error")
	}
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleTool && m.ToolCallID == "c1" && m.Content != "[DENIED] redis down" {
			t.Errorf("deny result = %q", m.Content)
		}
	}
}

// ─── mode filtering ──────────────────────────────────────────────────────────

// Plan mode forbids act-only tools: the call is rejected with a
// "not allowed in plan mode" tool message and the run continues.
func TestPlanModeBlocksActOnlyTool(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{{
			ID:    "c1",
			Name:  "write_to_file",
			Input: `{"path":"x.txt","content":"hi"}`,
		}}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{{
			ID:    "c2",
			Name:  types.ToolPlanModeRespond.String(),
			Input: `{"response":"planned"}`,
		}}},
	)
	reg := tools.NewRegistry()
	reg.Register(&tools.ToolInfo{Tool: tools.NewWriteFileTool(), AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewPlanModeRespondTool(), AllowedModes: []string{"plan", "act"}})
	agent, err := New(Options{Provider: prov, ToolRegistry: reg, Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	var rejected string
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleTool && m.ToolCallID == "c1" {
			rejected = m.Content
		}
	}
	if !strings.Contains(rejected, "not allowed in plan mode") {
		t.Errorf("plan-mode rejection = %q", rejected)
	}
}

// ─── XML fallback ────────────────────────────────────────────────────────────

// When the model emits XML-style tool calls in plain content (no native
// tool_calls), the loop parses and executes them.
func TestXMLFallbackParsesToolCalls(t *testing.T) {
	agent, echo := newTestAgent(t,
		provider.TurnScript{Content: `<echo>{"x":"1"}</echo>`},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	cb := &recordingCallback{}
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if echo.count() != 1 {
		t.Errorf("echo executed %d times, want 1 via XML fallback", echo.count())
	}
}

// ─── provider error path ─────────────────────────────────────────────────────

// A stream error is surfaced via OnError and stops the run (after being
// persisted as a failed task when a Store is present).
func TestStreamErrorStopsRun(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{Err: errors.New("boom")},
	)
	cb := &recordingCallback{}
	err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want stream error, got %v", err)
	}
	snap := cb.snapshot()
	if len(snap.errors) == 0 {
		t.Error("OnError not called for stream error")
	}
	if snap.completed != 0 {
		t.Errorf("OnComplete fired %d times on error path, want 0", snap.completed)
	}
}

// ─── abort ───────────────────────────────────────────────────────────────────

func TestAbortStopsRun(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}, Delay: 20 * time.Millisecond},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "done")}},
	)
	cb := &recordingCallback{}
	go func() {
		time.Sleep(5 * time.Millisecond)
		agent.Abort()
	}()
	if err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if agent.IsRunning() {
		t.Error("agent still running after abort")
	}
}
