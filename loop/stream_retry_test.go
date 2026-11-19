package loop

// Mid-stream transient-failure retries (the "error reading stream: read tcp
// … wsarecv: … forcibly closed" class observed against Cloudflare-fronted
// gateways): the run loop re-issues the request when a stream dies from a
// retryable network error AND the abandoned attempt delivered no complete
// tool call. The budget is consecutive-failure scoped and independent of the
// empty-response mistake budget.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// midStreamResetError mirrors what the OpenAI SSE reader emits when the
// gateway/CDN drops the connection mid-read: the real net error wrapped
// with the provider's "error reading stream" text.
func midStreamResetError() error {
	return fmt.Errorf("error reading stream: %w", &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: errors.New("wsarecv: An existing connection was forcibly closed by the remote host"),
	})
}

// newMidStreamRetryAgent builds an agent whose provider is scripted and
// whose retry schedule is zero-delay (fast tests); budget = len(turns-1)
// schedule entries, matching production's len(RetryDelays) rule.
func newMidStreamRetryAgent(t *testing.T, delays []time.Duration, turns ...provider.TurnScript) (*BaseAgent, *provider.ScriptedProvider) {
	t.Helper()
	prov := provider.NewScriptedProvider(turns...)
	reg := tools.NewRegistry()
	if err := reg.Register(&tools.ToolInfo{
		Tool:         &echoTool{},
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
		RetryDelays:  delays,
	})
	if err != nil {
		t.Fatal(err)
	}
	return agent, prov
}

// A stream killed by a connection reset before delivering anything durable
// is retried automatically: the run completes, OnRetry reports the attempt,
// and no error/correction state is recorded.
func TestMidStreamResetRetriesUntilSuccess(t *testing.T) {
	agent, prov := newMidStreamRetryAgent(t,
		[]time.Duration{0, 0, 0},
		provider.TurnScript{Err: midStreamResetError()},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	cb := &recordingCallback{}
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := prov.TurnCount(); got != 2 {
		t.Errorf("provider turns = %d, want 2 (one failed + one retry)", got)
	}
	snap := cb.snapshot()
	if len(snap.retries) != 1 || snap.retries[0].attempt != 1 {
		t.Errorf("retries = %+v, want one attempt=1 event", snap.retries)
	}
	if len(snap.errors) != 0 {
		t.Errorf("OnError fired on a recovered stream: %v", snap.errors)
	}
	if snap.completed != 1 {
		t.Errorf("OnComplete = %d, want 1", snap.completed)
	}
	if agent.consecutiveMistakes != 0 {
		t.Errorf("consecutiveMistakes = %d, want 0 (stream retries must not burn the correction budget)", agent.consecutiveMistakes)
	}
}

// Partial TEXT in the abandoned attempt does not block the retry — the
// frontend resets its stream buffer on the next stream:start and the failed
// attempt never reached the conversation.
func TestMidStreamResetRetriesAfterPartialContent(t *testing.T) {
	agent, prov := newMidStreamRetryAgent(t,
		[]time.Duration{0, 0},
		provider.TurnScript{Content: "partial answer that never landed", Err: midStreamResetError()},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c1", "done")}},
	)
	cb := &recordingCallback{}
	if _, err := agent.RunWithCallback(context.Background(), "task", cb); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := prov.TurnCount(); got != 2 {
		t.Errorf("provider turns = %d, want 2", got)
	}
	if snap := cb.snapshot(); snap.completed != 1 || len(snap.errors) != 0 {
		t.Errorf("completed=%d errors=%v, want completed with no errors", snap.completed, snap.errors)
	}
}

// Safety valve: a stream that delivered a COMPLETE tool call may already
// have pre-dispatched it — re-asking could duplicate side effects, so the
// run terminates exactly as before the retry feature existed.
func TestMidStreamResetNotRetriedAfterToolCall(t *testing.T) {
	agent, prov := newMidStreamRetryAgent(t,
		[]time.Duration{0, 0, 0},
		provider.TurnScript{ToolCalls: []provider.ToolCall{echoCall("c1")}, Err: midStreamResetError()},
	)
	cb := &recordingCallback{}
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil {
		t.Fatal("want error when the stream dies after a complete tool call, got nil")
	}
	if !strings.Contains(err.Error(), "error reading stream") {
		t.Errorf("err = %v, want the stream error", err)
	}
	if got := prov.TurnCount(); got != 1 {
		t.Errorf("provider turns = %d, want 1 (no retry after a delivered tool call)", got)
	}
	snap := cb.snapshot()
	if len(snap.retries) != 0 {
		t.Errorf("retries = %+v, want none", snap.retries)
	}
	if len(snap.errors) != 1 {
		t.Errorf("OnError = %d, want 1", len(snap.errors))
	}
}

// Exhausting the budget terminates with the ORIGINAL stream error — the
// empty-response circuit breaker text must never appear: the budgets are
// independent.
func TestMidStreamRetriesExhausted(t *testing.T) {
	agent, prov := newMidStreamRetryAgent(t,
		[]time.Duration{0, 0}, // budget = 2
		provider.TurnScript{Err: midStreamResetError()},
	)
	cb := &recordingCallback{}
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil {
		t.Fatal("want error after exhausting retries, got nil")
	}
	if !strings.Contains(err.Error(), "error reading stream") {
		t.Errorf("err = %v, want the stream error", err)
	}
	if strings.Contains(err.Error(), "max consecutive mistakes") {
		t.Errorf("err = %v, must not consume the empty-response mistake budget", err)
	}
	// initial attempt + 2 retries
	if got := prov.TurnCount(); got != 3 {
		t.Errorf("provider turns = %d, want 3", got)
	}
	snap := cb.snapshot()
	if len(snap.retries) != 2 {
		t.Errorf("retries = %d, want 2", len(snap.retries))
	}
	if agent.consecutiveMistakes != 0 {
		t.Errorf("consecutiveMistakes = %d, want 0", agent.consecutiveMistakes)
	}
}

// Permanent (non-network) mid-stream errors are not retried.
func TestMidStreamPermanentErrorNotRetried(t *testing.T) {
	agent, prov := newMidStreamRetryAgent(t,
		[]time.Duration{0, 0, 0},
		provider.TurnScript{Err: errors.New("bad request: malformed tool arguments")},
	)
	cb := &recordingCallback{}
	_, err := agent.RunWithCallback(context.Background(), "task", cb)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if got := prov.TurnCount(); got != 1 {
		t.Errorf("provider turns = %d, want 1 (permanent errors never retry)", got)
	}
	if snap := cb.snapshot(); len(snap.retries) != 0 {
		t.Errorf("retries = %+v, want none", snap.retries)
	}
}
