package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/liup215/cobot/types"
)

// RunResult is the structured outcome of one agent run. Run and
// RunWithCallback return it alongside the terminal error, so a host does not
// have to reassemble the outcome from the conversation transcript
// (GetConversation) or from callback events.
//
// The result is tied to the run that produced it — there is deliberately no
// separate RunResult() query method, which would need "result of which run"
// semantics and stale-state rules. A result is returned even when the run
// fails or is canceled, so hosts can report partial progress; only an
// "agent is already running" rejection returns a nil result.
//
// The zero value is a valid "nothing happened" result (what a run reports
// when the context was already canceled before the first turn).
type RunResult struct {
	// FinalText is the text the agent delivered to the host:
	//
	//   - a run ended by attempt_completion reports that tool call's
	//     `result` parameter — the deliverable the model produced for the
	//     task (not the decorated tool output stored in the conversation);
	//   - a plan-mode run ended by plan_mode_respond reports its
	//     `response` parameter;
	//   - any other ending (ask_followup_question, Abort, stream error,
	//     canceled context) falls back, best effort, to the last assistant
	//     text produced during this run; empty when the model produced no
	//     text.
	//
	// The deliverable is captured when the assistant turn is recorded, so
	// compaction that rewrites the transcript mid-run can neither blank it
	// nor surface an earlier run's text.
	FinalText string

	// InputTokens is the sum of the input-token usage the provider reported
	// for each completed turn of this run. Usage is per request, so input
	// tokens include re-sent context. Conversation.GetActualTokens retains
	// only the latest turn, which is why the loop accumulates each turn's
	// final usage report here; providers that never report usage leave both
	// counters at 0.
	InputTokens int

	// OutputTokens is the sum of the output-token usage the provider
	// reported for each completed turn of this run. See InputTokens.
	OutputTokens int

	// Turns is the number of completed LLM turns of this run: one per
	// provider stream that was read to the end. Retried connection attempts
	// and turns cut short by an error or a canceled context are not counted.
	Turns int

	// Duration is the wall-clock time of the run, measured from the start of
	// Run/RunWithCallback until the result is assembled.
	Duration time.Duration

	// Err is the terminal error of the run; nil when it ended normally. It
	// always mirrors the error returned by Run/RunWithCallback.
	Err error
}

// runStats accumulates per-run observability while the loop executes. It is
// owned by the RunWithCallback goroutine (processStream and executeToolCalls
// run on the same goroutine; pre-dispatch goroutines never touch it), so the
// plain fields need no lock.
type runStats struct {
	startTime time.Time

	turns        int
	inputTokens  int
	outputTokens int

	// finalText is the deliverable of a run-ending tool call
	// (attempt_completion `result`, plan_mode_respond `response`), captured
	// when the assistant turn carrying it is recorded. A later terminal call
	// overwrites an earlier one, so a retried ending reports the attempt that
	// actually ended the run.
	finalText string
	// lastAssistantText is the most recent assistant text of this run; the
	// best-effort deliverable when no terminal tool call was made.
	lastAssistantText string
}

// newRunStats starts accounting for a run.
func newRunStats() *runStats {
	return &runStats{startTime: time.Now()}
}

// addUsage records one turn's provider-reported usage. Streams deliver usage
// once, on the final chunk, as the cumulative total for that turn — the value
// is therefore added exactly once per turn, at the end of the stream.
func (r *runStats) addUsage(input, output int) {
	r.inputTokens += input
	r.outputTokens += output
}

// canceledError returns a wrapped context error when ctx is done and nil
// otherwise. The wrapper keeps errors.Is(err, context.Canceled) and
// errors.Is(err, context.DeadlineExceeded) working for the caller, while
// making the origin of the failure obvious in logs and task records.
func canceledError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("agent run canceled: %w", err)
	}
	return nil
}

// recordAssistantTurn captures what the model delivered in an assistant turn
// at the moment that turn is appended to the conversation: the deliverable of
// a terminal tool call (attempt_completion `result`, plan_mode_respond
// `response`) and the latest assistant text.
//
// Capturing here — instead of re-scanning the transcript when the result is
// assembled — keeps the outcome stable under mid-run compaction. Trimming
// rewrites and re-indexes the transcript, so a message index recorded at run
// start no longer delimits this run's slice of the conversation: the run's own
// messages can be dropped (blanking the result) and an under-shooting scan
// would resurface the previous run's deliverable.
func (r *runStats) recordAssistantTurn(content string, calls []types.ToolCall) {
	for _, tc := range calls {
		if text, ok := terminalToolText(tc); ok {
			r.finalText = text
		}
	}
	if strings.TrimSpace(content) != "" {
		r.lastAssistantText = content
	}
}

// finalDeliverable returns the text the agent delivered during this run: the
// terminal tool call's deliverable when there was one, otherwise the last
// assistant text it produced.
func (r *runStats) finalDeliverable() string {
	if r.finalText != "" {
		return r.finalText
	}
	return r.lastAssistantText
}

// buildRunResult assembles the structured outcome of the run. err is the
// terminal error (nil on success) and is stored in both the result and the
// return value of RunWithCallback.
func (a *BaseAgent) buildRunResult(run *runStats, err error) *RunResult {
	return &RunResult{
		FinalText:    run.finalDeliverable(),
		InputTokens:  run.inputTokens,
		OutputTokens: run.outputTokens,
		Turns:        run.turns,
		Duration:     time.Since(run.startTime),
		Err:          err,
	}
}

// canceledRunResult is the single exit point for a run whose context is done.
// It reports ok=false when the context is still alive; otherwise it marks the
// stored task failed (best effort, like every other run failure), surfaces
// the wrapped context error through OnError and returns the partial result
// the host should report.
func (a *BaseAgent) canceledRunResult(ctx context.Context, run *runStats, callback StreamCallback) (res *RunResult, err error, canceled bool) {
	cerr := canceledError(ctx)
	if cerr == nil {
		return nil, nil, false
	}
	// A cancel WE fired — Abort() cancels the run's context to interrupt a
	// blocked provider stream / shell command IMMEDIATELY — keeps the abort
	// contract: non-error outcome, OnComplete teardown, task marked aborted
	// (exactly what the flag-only path did at the loop top). Externally
	// canceled contexts stay errors.
	if a.abort.Load() {
		a.failTask("aborted")
		if callback != nil {
			callback.OnComplete()
		}
		return a.buildRunResult(run, nil), nil, true
	}
	a.failTask(cerr.Error())
	if callback != nil {
		callback.OnError(cerr)
	}
	return a.buildRunResult(run, cerr), cerr, true
}

// failTask marks the stored task as failed, best effort: store errors are
// logged and never abort the run, and a missing task record is a no-op.
func (a *BaseAgent) failTask(reason string) {
	if a.store == nil || a.taskID == "" {
		return
	}
	if err := a.store.FailTask(a.taskID, reason); err != nil {
		logWarnf("Failed to mark task as failed: %v", err)
	}
}

// terminalToolText extracts the deliverable of a run-ending tool call
// (attempt_completion's `result`, plan_mode_respond's `response`). ok is
// false for every other tool, for unparseable input and for an empty
// deliverable, so callers fall back to the last assistant text.
func terminalToolText(tc types.ToolCall) (text string, ok bool) {
	var param string
	switch tc.Name {
	case types.ToolAttemptCompletion.String():
		param = "result"
	case types.ToolPlanModeRespond.String():
		param = "response"
	default:
		return "", false
	}

	var payload map[string]any
	if err := json.Unmarshal(tc.Input, &payload); err != nil {
		return "", false
	}
	s, _ := payload[param].(string)
	return s, s != ""
}
