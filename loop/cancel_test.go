package loop

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// ─── ctx cancellation semantics ──────────────────────────────────────────────

// hangingStreamProvider simulates an LLM endpoint that accepts the request but
// never sends anything: the stream stays open until the caller's context is
// canceled, then closes. A real provider's pump goroutine behaves the same way.
type hangingStreamProvider struct{}

func (p *hangingStreamProvider) CreateMessageStream(ctx context.Context, req *provider.MessageRequest) (<-chan provider.StreamChunk, error) {
	ch := make(chan provider.StreamChunk) // unbuffered, like the real providers
	go func() {
		defer close(ch)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second): // safety net so a bug fails, not hangs
		}
	}()
	return ch, nil
}

func (p *hangingStreamProvider) CreateMessage(ctx context.Context, req *provider.MessageRequest) (*provider.MessageResponse, error) {
	return nil, errors.New("not used")
}
func (p *hangingStreamProvider) SupportsTools() bool     { return true }
func (p *hangingStreamProvider) GetModel() string        { return "hanging" }
func (p *hangingStreamProvider) GetProviderName() string { return "hanging" }

// blockedTool blocks inside Execute until its context is canceled. The name is
// configurable so tests can place it on either side of the pre-dispatch split:
// "execute_command" is pre-dispatch-ineligible (single main-loop execution),
// any other name goes through the pre-dispatch goroutine.
type blockedTool struct {
	name    string
	started chan struct{}
}

func (b *blockedTool) Name() string {
	if b.name == "" {
		return "execute_command"
	}
	return b.name
}
func (b *blockedTool) Description() string { return "blocks until its context is canceled" }
func (b *blockedTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
}
func (b *blockedTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	close(b.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// waitForGoroutineBaseline polls until the goroutine count returns to the
// baseline (the test runner's own goroutine aside) or fails.
func waitForGoroutineBaseline(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines = %d after the run, baseline was %d (leak suspected)",
		runtime.NumGoroutine(), before)
}

// Canceling the run context while the provider stream is open must end the run
// promptly with a wrapped context.Canceled, mark the agent runnable again, and
// leave no goroutine behind.
func TestContextCancelDuringStream(t *testing.T) {
	agent := newEchoCompletionAgent(t, &hangingStreamProvider{})
	cb := &recordingCallback{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := runtime.NumGoroutine()
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = agent.RunWithCallback(ctx, "task", cb)
	}()

	time.Sleep(50 * time.Millisecond) // let the run block reading the stream
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunWithCallback did not return after ctx cancellation")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("err = %v, want a wrapped context.Canceled", runErr)
	}
	if agent.IsRunning() {
		t.Error("agent still marked running after a canceled run")
	}
	waitForGoroutineBaseline(t, before)

	// The agent stays usable: a fresh run on the same agent completes.
	agent.provider = provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c9", "recovered")}},
	)
	res, err := agent.RunWithCallback(context.Background(), "again", &recordingCallback{})
	if err != nil {
		t.Fatalf("run after cancel: %v", err)
	}
	if res.FinalText != "recovered" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "recovered")
	}
}

// Canceling during tool execution unblocks the tool, ends the run promptly
// with a wrapped context.Canceled, and issues no further LLM request (a
// cancellation is not a model mistake to be retried).
func TestContextCancelDuringToolExecution(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{ToolCalls: []provider.ToolCall{{
			ID:    "c1",
			Name:  "execute_command",
			Input: `{"command":"sleep 100"}`,
		}}},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "unreachable")}},
	)
	reg := tools.NewRegistry()
	tool := &blockedTool{started: make(chan struct{})} // pre-dispatch-ineligible name
	if err := reg.Register(&tools.ToolInfo{Tool: tool, AllowedModes: []string{"act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := agent.RunWithCallback(ctx, "task", &recordingCallback{})
		done <- err
	}()

	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("tool never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a wrapped context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWithCallback did not return after cancellation during tool execution")
	}
	// No further LLM request: the second scripted turn must be untouched.
	if got := prov.TurnCount(); got != 1 {
		t.Errorf("provider turns = %d, want 1 (no request after cancellation)", got)
	}
	if agent.IsRunning() {
		t.Error("agent still marked running after a canceled run")
	}
}

// A tool already pre-dispatched while the stream is still open must also be
// interruptible: cancellation unblocks it, the abandoned stream is drained so
// the provider pump can exit, and the run returns promptly without leaking.
func TestContextCancelDuringPreDispatch(t *testing.T) {
	prov := provider.NewScriptedProvider(
		provider.TurnScript{
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "slow_lookup", Input: `{"x":"1"}`}},
			// Delay keeps stream 1 open while the pre-dispatch goroutine runs.
			Delay: 400 * time.Millisecond,
		},
		provider.TurnScript{ToolCalls: []provider.ToolCall{completionCall("c2", "unreachable")}},
	)
	reg := tools.NewRegistry()
	tool := &blockedTool{name: "slow_lookup", started: make(chan struct{})}
	if err := reg.Register(&tools.ToolInfo{Tool: tool, AllowedModes: []string{"act"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&tools.ToolInfo{Tool: tools.NewAttemptCompletionTool(), AllowedModes: []string{"act"}}); err != nil {
		t.Fatal(err)
	}
	agent, err := New(Options{Provider: prov, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := agent.RunWithCallback(ctx, "task", &recordingCallback{})
		done <- err
	}()

	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-dispatched tool never started")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want a wrapped context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunWithCallback did not return after cancellation during pre-dispatch")
	}
	waitForGoroutineBaseline(t, before)
	if got := prov.TurnCount(); got != 1 {
		t.Errorf("provider turns = %d, want 1", got)
	}
}

// Abort stays the cooperative, non-error stop channel: it takes effect at the
// next checkpoint (stream end / tool boundary) and does NOT force-unblock an
// in-flight provider read — that is the run context's job. Aborting a hung
// stream leaves the run blocked; canceling the context then ends it with the
// wrapped context error, which wins over the pending abort flag.
func TestAbortDoesNotUnblockStreamReadButContextDoes(t *testing.T) {
	agent := newEchoCompletionAgent(t, &hangingStreamProvider{})
	cb := &recordingCallback{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = agent.RunWithCallback(ctx, "task", cb)
	}()

	time.Sleep(50 * time.Millisecond)
	agent.Abort()

	// Abort alone does not end a run that is blocked reading the stream.
	select {
	case <-done:
		t.Fatal("RunWithCallback returned from Abort alone; the abort flag is a checkpoint, not an interrupt")
	case <-time.After(150 * time.Millisecond):
	}

	// Canceling the context is what unblocks it — and the wrapped context
	// error wins over the pending abort flag.
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunWithCallback did not return after ctx cancellation")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("err = %v, want a wrapped context.Canceled", runErr)
	}
	if agent.IsRunning() {
		t.Error("agent still marked running after the canceled run")
	}
}
