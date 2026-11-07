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

	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// ─── test helpers ────────────────────────────────────────────────────────────

// recordingCallback captures every callback event for assertions.
type recordingCallback struct {
	mu               sync.Mutex
	contents         []string
	reasonings       []string
	streamStarts     int
	toolStarts       []provider.ToolCall
	toolCompletions  []toolCompletion
	errors           []error
	completed        int
	taskCreated      []string
	updates          []toolUpdate
	compactionStarts []string
	compactionEnds   []string
	retries          []retryEvent
}

type toolCompletion struct {
	tc     provider.ToolCall
	result string
}

type toolUpdate struct {
	tc     provider.ToolCall
	status string
}

type retryEvent struct {
	attempt int
	err     error
	delay   time.Duration
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
func (c *recordingCallback) OnToolCallUpdate(tc provider.ToolCall, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = append(c.updates, toolUpdate{tc, status})
}
func (c *recordingCallback) OnCompactionStart(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compactionStarts = append(c.compactionStarts, reason)
}
func (c *recordingCallback) OnCompactionEnd(summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.compactionEnds = append(c.compactionEnds, summary)
}
func (c *recordingCallback) OnRetry(attempt int, err error, delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retries = append(c.retries, retryEvent{attempt, err, delay})
}

// snapshot returns a copy of the recorded events without copying the mutex.
func (c *recordingCallback) snapshot() recordingCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	return recordingCallback{
		contents:         c.contents,
		reasonings:       c.reasonings,
		streamStarts:     c.streamStarts,
		toolStarts:       c.toolStarts,
		toolCompletions:  c.toolCompletions,
		errors:           c.errors,
		completed:        c.completed,
		taskCreated:      c.taskCreated,
		updates:          c.updates,
		compactionStarts: c.compactionStarts,
		compactionEnds:   c.compactionEnds,
		retries:          c.retries,
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
			Name:  "write",
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
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
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
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
	// Abort() stays the non-error cancellation channel: the run ends without
	// an error and still reports a structured result.
	res, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil {
		t.Fatal("RunWithCallback returned a nil result on the abort path")
	}
	if res.Err != nil {
		t.Errorf("result.Err = %v, want nil (abort is not an error)", res.Err)
	}
	if res.Duration <= 0 {
		t.Errorf("result.Duration = %v, want > 0", res.Duration)
	}
	if agent.IsRunning() {
		t.Error("agent still running after abort")
	}
}

// ─── automatic stream retry (FR-T4) ──────────────────────────────────────────

// flakyProvider fails the first N CreateMessageStream calls with a
// retryable HTTPError, then delegates to the wrapped provider.
type flakyProvider struct {
	inner        provider.Provider
	failuresLeft int
	status       int
	calls        int
	mu           sync.Mutex
}

func (f *flakyProvider) CreateMessageStream(ctx context.Context, req *provider.MessageRequest) (<-chan provider.StreamChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failuresLeft {
		return nil, &provider.HTTPError{Status: f.status, Message: "temporary failure"}
	}
	return f.inner.CreateMessageStream(ctx, req)
}
func (f *flakyProvider) CreateMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	return f.inner.CreateMessage(ctx, req)
}
func (f *flakyProvider) SupportsTools() bool     { return f.inner.SupportsTools() }
func (f *flakyProvider) GetModel() string        { return f.inner.GetModel() }
func (f *flakyProvider) GetProviderName() string { return f.inner.GetProviderName() }

func newFlakyAgent(t *testing.T, failures int, status int, delays []time.Duration) (*BaseAgent, *echoTool, *flakyProvider) {
	t.Helper()
	inner := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	flaky := &flakyProvider{inner: inner, failuresLeft: failures, status: status}
	reg := tools.NewRegistry()
	echo := &echoTool{}
	reg.Register(&tools.ToolInfo{Tool: echo, AllowedModes: []string{"act"}})
	reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}})
	agent, err := New(Options{Provider: flaky, ToolRegistry: reg, RetryDelays: delays})
	if err != nil {
		t.Fatal(err)
	}
	return agent, echo, flaky
}

// Retryable failures (503) are retried until the connection succeeds; each
// retry is reported via OnRetry and the run then completes normally.
func TestRetryRecoversFromConnectionErrors(t *testing.T) {
	agent, _, flaky := newFlakyAgent(t, 2, 503, []time.Duration{0, 0})
	cb := &recordingCallback{}
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if flaky.calls != 3 { // 2 failures + 1 success
		t.Errorf("provider called %d times, want 3", flaky.calls)
	}
	snap := cb.snapshot()
	if len(snap.retries) != 2 {
		t.Fatalf("OnRetry fired %d times, want 2", len(snap.retries))
	}
	if snap.retries[0].attempt != 1 || snap.retries[1].attempt != 2 {
		t.Errorf("retry attempts = %d, %d; want 1, 2", snap.retries[0].attempt, snap.retries[1].attempt)
	}
	if snap.completed != 1 {
		t.Errorf("OnComplete fired %d times, want 1", snap.completed)
	}
}

// 4xx parameter errors are permanent: the retry wrapper gives up
// immediately and each main-loop round burns a mistake until the run
// terminates at the consecutive-mistakes limit.
func TestNoRetryOn4xx(t *testing.T) {
	agent, _, flaky := newFlakyAgent(t, 99, 400, []time.Duration{0})
	cb := &recordingCallback{}
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil {
		t.Fatal("want error for 400, got nil")
	}
	if !strings.Contains(err.Error(), "max consecutive mistakes") {
		t.Errorf("want mistakes-limit error, got %v", err)
	}
	if flaky.calls != 3 {
		t.Errorf("provider called %d times, want 3 (one per main-loop round, no retries)", flaky.calls)
	}
	if len(cb.snapshot().retries) != 0 {
		t.Error("OnRetry fired for a 4xx error")
	}
}

// An empty non-nil RetryDelays slice disables retrying entirely.
func TestRetryDisabledWithEmptyDelays(t *testing.T) {
	agent, _, flaky := newFlakyAgent(t, 3, 503, []time.Duration{})
	cb := &recordingCallback{}
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err == nil {
		t.Fatal("want error when retries are disabled")
	}
	// No retries: each of the 3 main-loop rounds creates one stream, then
	// the consecutive-mistakes limit terminates the run.
	if flaky.calls != 3 {
		t.Errorf("provider called %d times, want 3", flaky.calls)
	}
	if len(cb.snapshot().retries) != 0 {
		t.Error("OnRetry fired although retries are disabled")
	}
}

// ─── compaction + tool update callbacks ──────────────────────────────────────

// When the token budget is exceeded the loop emits OnCompactionStart/End.
func TestCompactionEventsFire(t *testing.T) {
	// Tiny budget: a single big user message crosses the 60% line.
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	agent.conversation.MaxTokens = 200
	agent.conversation.ResponseBuffer = 0
	cb := &recordingCallback{}
	if _, err := agent.RunWithCallback(context.Background(), "a very long task prompt that exceeds the tiny token budget for sure", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	snap := cb.snapshot()
	if len(snap.compactionStarts) == 0 {
		t.Error("OnCompactionStart never fired")
	}
	if len(snap.compactionEnds) == 0 {
		t.Error("OnCompactionEnd never fired")
	}
}

// ─── agent identity in the system prompt ─────────────────────────────────────

// agentNameAgent builds a minimal BaseAgent (attempt_completion only) with
// the given Options.AgentName, returning it plus the scripted provider so
// the captured request can be inspected.
func agentNameAgent(t *testing.T, agentName string) (*BaseAgent, *provider.ScriptedProvider) {
	t.Helper()
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
		AgentName:    agentName,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent, prov
}

// Options.AgentName is announced in the system prompt's role section; an
// empty name falls back to prompts.DefaultAgentName.
func TestAgentNameInSystemPrompt(t *testing.T) {
	cases := []struct {
		name      string
		agentName string
		want      string
	}{
		{"default", "", "You are " + prompts.DefaultAgentName + ","},
		{"custom", "maestro-worker", "You are maestro-worker,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, prov := agentNameAgent(t, tc.agentName)
			if _, err := agent.Run(context.Background(), "task"); err != nil {
				t.Fatalf("run: %v", err)
			}
			reqs := prov.Requests()
			if len(reqs) == 0 {
				t.Fatal("provider captured no request")
			}
			sp := reqs[0].SystemPrompt
			if !strings.HasPrefix(sp, tc.want) {
				head := sp
				if len(head) > 80 {
					head = head[:80]
				}
				t.Errorf("system prompt = %q, want identity prefix %q", head, tc.want)
			}
		})
	}
}
