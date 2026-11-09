package loop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liup215/cobot/compact"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// completionCallType is completionCall for the loop's internal message shape
// (what processStream hands to runStats.recordAssistantTurn).
func completionCallType(id, result string) types.ToolCall {
	input, _ := json.Marshal(map[string]string{"result": result})
	return types.ToolCall{ID: id, Name: types.ToolAttemptCompletion.String(), Input: input}
}

// ─── RunResult (structured run outcome) ──────────────────────────────────────

// A run ended by attempt_completion reports the tool call's `result`
// parameter as FinalText, the number of completed turns and the per-turn
// usage summed across the run; the terminal error is nil.
func TestRunResultFromAttemptCompletion(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{
			ToolCalls:    []provider.ToolCall{echoCall("c1")},
			InputTokens:  100,
			OutputTokens: 10,
		},
		provider.TurnScript{
			ToolCalls:    []provider.ToolCall{completionCall("c2", "the final answer")},
			InputTokens:  200,
			OutputTokens: 20,
		},
	)
	res, err := agent.RunWithCallback(context.Background(), "task", &recordingCallback{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil {
		t.Fatal("result is nil")
	}
	if res.Err != nil {
		t.Errorf("result.Err = %v, want nil", res.Err)
	}
	if res.FinalText != "the final answer" {
		t.Errorf("FinalText = %q, want the attempt_completion result parameter", res.FinalText)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	// Per-turn usage is summed across turns (100+200 in / 10+20 out).
	if res.InputTokens != 300 || res.OutputTokens != 30 {
		t.Errorf("usage = %d/%d, want 300/30", res.InputTokens, res.OutputTokens)
	}
	// Duration is covered by TestRunResultDurationMeasuredFromRunStart: a
	// scripted run finishes inside the platform clock granularity, so a
	// behavioral "> 0" assertion here is flaky (Windows timer granularity
	// makes a sub-millisecond run report exactly 0).
	// The conversation-level actual tokens keep last-turn semantics; the run
	// result is the one that aggregates.
	in, out := agent.GetConversation().GetActualTokens()
	if in != 200 || out != 20 {
		t.Errorf("conversation actual tokens = %d/%d, want last turn 200/20", in, out)
	}
}

// A run that ends abnormally (stream error) still returns a usable result:
// the last assistant text as FinalText and the error mirrored into RunResult.Err.
func TestRunResultAbnormalEndFallsBackToLastAssistantText(t *testing.T) {
	// A plain-text reply now completes the run (pi semantics), so the
	// abnormal-end fallback needs a turn that does NOT end the run: text
	// WITH a tool call (echo executes), then the failing request.
	agent, _ := newTestAgent(t,
		provider.TurnScript{Content: "partial analysis of the task", ToolCalls: []provider.ToolCall{echoCall("e1")}},
		provider.TurnScript{Err: errors.New("boom")},
	)
	res, err := agent.RunWithCallback(context.Background(), "task", &recordingCallback{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want stream error, got %v", err)
	}
	if res == nil {
		t.Fatal("result must be non-nil on the error path (partial progress)")
	}
	if res.Err != err {
		t.Errorf("result.Err = %v, want the returned error %v", res.Err, err)
	}
	if res.FinalText != "partial analysis of the task" {
		t.Errorf("FinalText = %q, want the last assistant text of this run", res.FinalText)
	}
	// The second turn never completed, so only the first counts.
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
}

// ask_followup_question is not a terminal deliverable: the completion that
// ends the run wins over an earlier assistant text.
func TestRunResultAskFollowupThenCompletion(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{
			Content: "before asking",
			ToolCalls: []provider.ToolCall{{
				ID:    "c1",
				Name:  types.ToolAskFollowupQuestion.String(),
				Input: `{"question":"which branch?","options":["main","dev"]}`,
			}},
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "went with main")}},
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
	res, err := agent.RunWithCallback(context.Background(), "task", &recordingCallback{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalText != "went with main" {
		t.Errorf("FinalText = %q, want the terminal attempt_completion result", res.FinalText)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
}

// A plan-mode run ended by plan_mode_respond reports the tool call's
// `response` parameter.
func TestRunResultPlanModeRespond(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{{
			ID:    "c1",
			Name:  types.ToolPlanModeRespond.String(),
			Input: `{"response":"the plan: step one, then step two"}`,
		}}},
	)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewPlanModeRespondTool(), AllowedModes: []string{"plan"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg, Mode: ModePlan})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Run(context.Background(), "plan something")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalText != "the plan: step one, then step two" {
		t.Errorf("FinalText = %q, want the plan_mode_respond response parameter", res.FinalText)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
}

// Results are scoped to the run that produced them: a second Run on the same
// agent must not report the first run's deliverable.
func TestRunResultScopedToRun(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "first done")}},
		provider.TurnScript{Content: "second run analysis"},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "second done")}},
	)
	res1, err := agent.Run(context.Background(), "task one")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res1.FinalText != "first done" {
		t.Errorf("first FinalText = %q", res1.FinalText)
	}
	res2, err := agent.Run(context.Background(), "task two")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The plain-text turn completes run 2 immediately (pi semantics — no
	// correction round), and its FinalText must stay scoped to THIS run.
	if res2.FinalText != "second run analysis" {
		t.Errorf("second FinalText = %q, want this run's own text answer", res2.FinalText)
	}
	if res2.Turns != 1 {
		t.Errorf("second Turns = %d, want 1 (text answer ends the run)", res2.Turns)
	}
}

// A context that is already canceled ends the run before anything is
// recorded: no user message, no LLM request, wrapped context.Canceled.
func TestRunResultAlreadyCanceledContext(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	agent := newEchoCompletionAgent(t, prov)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := agent.GetConversation().MessageCount()
	res, err := agent.RunWithCallback(ctx, "task", &recordingCallback{})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want wrapped context.Canceled", err)
	}
	if res == nil {
		t.Fatal("result must be non-nil")
	}
	if res.Err != err {
		t.Errorf("result.Err = %v, want the returned error", res.Err)
	}
	if res.Turns != 0 {
		t.Errorf("Turns = %d, want 0", res.Turns)
	}
	if got := agent.GetConversation().MessageCount(); got != before {
		t.Errorf("message count = %d, want unchanged %d", got, before)
	}
	if prov.TurnCount() != 0 {
		t.Errorf("provider turns = %d, want 0", prov.TurnCount())
	}
	if agent.IsRunning() {
		t.Error("agent still marked running")
	}
}

// Duration is the wall-clock time of the run. Asserted directly against a
// runStats whose start is backdated, because a scripted run finishes inside
// the platform clock granularity: on Windows a sub-millisecond run reports
// Duration == 0, so a behavioral "> 0" assertion would be flaky (~10-30% on
// -count=10).
func TestRunResultDurationMeasuredFromRunStart(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)

	run := newRunStats()
	run.startTime = time.Now().Add(-25 * time.Millisecond)
	if res := agent.buildRunResult(run, nil); res.Duration < 25*time.Millisecond {
		t.Errorf("Duration = %v, want >= 25ms (measured from the run start)", res.Duration)
	}
}

// The deliverable is captured when the assistant turn is recorded, so
// compaction that rewrites the transcript mid-run can neither blank the
// result nor resurface an earlier run's text. Regression for a defect found
// by an out-of-repo probe: with the previous index-into-transcript
// implementation, a run whose conversation was trimmed before its first turn
// reported an empty FinalText even though it ended with attempt_completion.
func TestRunResultFinalTextCapturedAtTurnTime(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c0", "stale")}},
	)
	// An earlier run's deliverable is still sitting in the conversation.
	if _, err := agent.Run(context.Background(), "first"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	run := newRunStats()
	run.recordAssistantTurn("", []types.ToolCall{completionCallType("c1", "the real deliverable")})

	// Worst-case compaction: this run's own messages are rewritten away.
	agent.conversation.Clear()

	if res := agent.buildRunResult(run, nil); res.FinalText != "the real deliverable" {
		t.Errorf("FinalText = %q, want the deliverable captured when the turn was recorded", res.FinalText)
	}
}

// A run that produced no terminal tool call must not inherit the deliverable
// of a terminal tool call that is still lying around in the shared
// conversation from an earlier run.
func TestRunResultFinalTextDoesNotLeakPreviousRun(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c0", "previous run deliverable")}},
	)
	if _, err := agent.Run(context.Background(), "first"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	run := newRunStats()
	run.recordAssistantTurn("this run's analysis", nil)

	res := agent.buildRunResult(run, nil)
	if res.FinalText != "this run's analysis" {
		t.Errorf("FinalText = %q, want this run's own text (no cross-run leak)", res.FinalText)
	}
}

// A retried ending reports the terminal call that actually ended the run: a
// later attempt_completion overwrites the deliverable of an earlier,
// abandoned one (e.g. one denied by the guard).
func TestRunResultLaterTerminalCallWins(t *testing.T) {
	run := newRunStats()
	run.recordAssistantTurn("", []types.ToolCall{completionCallType("c1", "first attempt")})
	run.recordAssistantTurn("", []types.ToolCall{completionCallType("c2", "second attempt")})
	if got := run.finalDeliverable(); got != "second attempt" {
		t.Errorf("FinalText = %q, want the latest terminal call %q", got, "second attempt")
	}
}

// ─── RunResult under mid-run compaction ──────────────────────────────────────

// countOneTokenPerRune swaps in a deterministic estimator (1 token per rune)
// and restores the package default on cleanup, so the token-budget thresholds
// that drive compaction are a function of the content this test writes.
func countOneTokenPerRune(t *testing.T) {
	t.Helper()
	prev := types.Estimator
	types.Estimator = func(s string) int { return len([]rune(s)) }
	t.Cleanup(func() { types.Estimator = prev })
}

// probeAgent is a minimal agent with a tiny context window, used to drive the
// loop's own compaction deterministically.
func newProbeAgent(t *testing.T, prov provider.Provider, maxTokens int) *BaseAgent {
	t.Helper()
	agent := newEchoCompletionAgent(t, prov)
	agent.conversation.MaxTokens = maxTokens
	agent.conversation.ResponseBuffer = 0
	agent.systemPromptOverride = "You are a probe." // keep the system prompt out of the budget arithmetic
	return agent
}

// A run whose conversation is compacted mid-run still reports its own
// deliverable. Regression: result assembly used to rescan the transcript from
// a message index recorded at run start, but trimming re-indexes the
// transcript — a normally finished run could report an empty FinalText even
// though it ended with attempt_completion.
//
// rewriteHistoryStrategy makes the mid-run rewrite deterministic regardless
// of the built-in Trim's retention rules (which protect the first user
// message and the recent rounds): Compact replaces the history with only its
// last two messages, so the run-start transcript is provably gone when result
// assembly runs.
type rewriteHistoryStrategy struct{ compacted bool }

func (s *rewriteHistoryStrategy) ShouldCompact(*types.Conversation) bool { return true }

func (s *rewriteHistoryStrategy) Compact(_ context.Context, conv *types.Conversation) (compact.Result, error) {
	msgs := conv.GetMessages()
	if len(msgs) > 2 {
		msgs = msgs[len(msgs)-2:]
	}
	conv.ReplaceMessages(msgs)
	s.compacted = true
	return compact.Result{}, nil
}

func TestRunResultAfterMidRunCompaction(t *testing.T) {
	countOneTokenPerRune(t)
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "first deliverable")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c3", "second deliverable")}},
	)
	strategy := &rewriteHistoryStrategy{}
	agent := newProbeAgent(t, prov, 500)
	agent.compaction = strategy

	if _, err := agent.Run(context.Background(), "task one"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	cb := &recordingCallback{}
	res, err := agent.RunWithCallback(context.Background(), strings.Repeat("x", 200), cb)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	// Guard the scenario: the injected strategy must have rewritten the
	// transcript during the second run, or this test proves nothing.
	if !strategy.compacted {
		t.Fatal("test setup: no compaction ran during the second run")
	}
	for _, m := range agent.GetConversation().GetMessages() {
		if m.Role == types.RoleUser && m.Content == "task one" {
			t.Fatal("test setup: the first run's messages were not compacted away")
		}
	}

	if res.FinalText != "second deliverable" {
		t.Errorf("FinalText = %q, want this run's attempt_completion result (mid-run compaction must not blank it)", res.FinalText)
	}
}

// A run that fails after mid-run compaction must not resurface an earlier
// run's deliverable: with the previous index-into-transcript assembly the
// under-shooting scan walked the whole transcript and reported the first
// run's attempt_completion result as this run's FinalText.
func TestRunResultNoCrossRunLeakAfterCompaction(t *testing.T) {
	countOneTokenPerRune(t)
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "first deliverable")}},
		provider.TurnScript{Content: "second run analysis", ToolCalls: []provider.ToolCall{echoCall("c3")}},
		provider.TurnScript{Err: errors.New("boom")},
	)
	agent := newProbeAgent(t, prov, 1000)

	if _, err := agent.Run(context.Background(), "task one"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	startMsgCount := agent.conversation.MessageCount()

	cb := &recordingCallback{}
	res, err := agent.RunWithCallback(context.Background(), strings.Repeat("x", 500), cb)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("second run err = %v, want the stream error", err)
	}

	// Guard the scenario: the trim must have run during the second run, and
	// the first run's deliverable is still in the shared conversation (there
	// is something to leak).
	if len(cb.snapshot().compactionStarts) == 0 {
		t.Fatal("test setup: no compaction ran during the second run")
	}
	stillThere := false
	for _, m := range agent.GetConversation().GetMessages() {
		for _, tc := range m.ToolCalls {
			if tc.Name == types.ToolAttemptCompletion.String() && strings.Contains(string(tc.Input), "first deliverable") {
				stillThere = true
			}
		}
	}
	if !stillThere {
		t.Fatal("test setup: the first run's deliverable was compacted away; nothing to leak")
	}
	if agent.conversation.MessageCount() < startMsgCount {
		t.Fatalf("test setup: transcript shrank below the run-start count (%d < %d); the leak precondition is gone", agent.conversation.MessageCount(), startMsgCount)
	}

	if res.FinalText != "second run analysis" {
		t.Errorf("FinalText = %q, want this run's own last assistant text (no cross-run leak)", res.FinalText)
	}
}

// A run rejected because the agent is already running returns a nil result.
func TestRunResultNilWhenAlreadyRunning(t *testing.T) {
	agent, _ := newTestAgent(t,
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	release := make(chan struct{})
	blocking := &gateProvider{inner: provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	), gate: release, entered: make(chan struct{})}
	agent.provider = blocking

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := agent.RunWithCallback(context.Background(), "task", &recordingCallback{}); err != nil {
			t.Errorf("first run: %v", err)
		}
	}()
	<-blocking.entered // the run holds the lock

	res, err := agent.RunWithCallback(context.Background(), "second", &recordingCallback{})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v, want already-running error", err)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil for a rejected call", res)
	}
	close(release)
	<-done
}

// gateProvider blocks the first stream request until its gate is closed, so a
// test can hold a run open while calling into the agent from another
// goroutine.
type gateProvider struct {
	inner   provider.Provider
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (g *gateProvider) CreateMessageStream(ctx context.Context, req *provider.MessageRequest) (<-chan provider.StreamChunk, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.inner.CreateMessageStream(ctx, req)
}

func (g *gateProvider) CreateMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	return nil, errors.New("not used")
}

func (g *gateProvider) SupportsTools() bool     { return true }
func (g *gateProvider) GetModel() string        { return "gate" }
func (g *gateProvider) GetProviderName() string { return "gate" }

// newEchoCompletionAgent builds an agent with the echo + attempt_completion
// tools and the given scripted provider.
func newEchoCompletionAgent(t *testing.T, prov provider.Provider) *BaseAgent {
	t.Helper()
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{Tool: &echoTool{}, AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"plan", "act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}
