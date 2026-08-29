package loop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

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
	if res.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", res.Duration)
	}
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
	agent, _ := newTestAgent(t,
		provider.TurnScript{Content: "partial analysis of the task"},
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
	if res2.FinalText != "second done" {
		t.Errorf("second FinalText = %q, want this run's deliverable only", res2.FinalText)
	}
	if res2.Turns != 2 { // "no tool used" correction turn + completion turn
		t.Errorf("second Turns = %d, want 2", res2.Turns)
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
