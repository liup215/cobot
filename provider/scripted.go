package provider

import (
	"context"
	"sync"
	"time"
)

// TurnScript is one canned LLM turn for ScriptedProvider.
type TurnScript struct {
	// Content is the assistant text; it is streamed as a few chunks.
	Content string

	// Reasoning is streamed via ReasoningContent before the text.
	Reasoning string

	// ToolCalls are complete tool calls emitted in the stream (each as a
	// non-partial ToolCall chunk, like a finished OpenAI tool_call).
	ToolCalls []ToolCall

	// Err, when set, is delivered as a stream error (chunk.Error) after
	// any content/reasoning/toolCalls of this turn have been emitted.
	Err error

	// Delay, when non-zero, is slept before the Done chunk; tests can use
	// it to exercise pre-dispatch overlap. Keep it tiny in tests.
	Delay time.Duration

	// InputTokens / OutputTokens, when non-zero, ride on the Done chunk as
	// the turn's TokenUsage (like a real provider's final usage report), so
	// tests can exercise usage accumulation across turns.
	InputTokens  int
	OutputTokens int
}

// ScriptedProvider plays back a canned sequence of turns, one per
// CreateMessageStream call, enabling deterministic multi-turn agent-loop
// tests (turn 1: tool call, turn 2: completion, ...). When the script is
// exhausted, the last turn is replayed repeatedly.
type ScriptedProvider struct {
	mu       sync.Mutex
	turns    []TurnScript
	idx      int
	requests []*MessageRequest
}

// NewScriptedProvider creates a provider that replays the given turns.
func NewScriptedProvider(turns ...TurnScript) *ScriptedProvider {
	return &ScriptedProvider{turns: turns}
}

// Requests returns copies of every request received so far, in order.
func (p *ScriptedProvider) Requests() []*MessageRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*MessageRequest, len(p.requests))
	copy(out, p.requests)
	return out
}

// TurnCount returns how many turns have been served so far.
func (p *ScriptedProvider) TurnCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idx
}

func (p *ScriptedProvider) current() (TurnScript, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.idx
	p.idx++
	reqIdx := len(p.requests)
	p.requests = append(p.requests, nil)
	if idx >= len(p.turns) {
		idx = len(p.turns) - 1
	}
	return p.turns[idx], reqIdx
}

// CreateMessageStream plays back the next scripted turn as a stream.
// The ctx from the caller is bound to the returned channel closure via
// CreateMessageStream's contract (see below).
func (p *ScriptedProvider) CreateMessageStream(ctx context.Context, req *MessageRequest) (<-chan StreamChunk, error) {
	t, reqIdx := p.current()
	p.mu.Lock()
	if reqIdx < len(p.requests) {
		p.requests[reqIdx] = req
	}
	p.mu.Unlock()

	ch := make(chan StreamChunk, 32)
	go func() {
		defer close(ch)
		if t.Reasoning != "" {
			ch <- StreamChunk{ReasoningContent: t.Reasoning}
		}
		if t.Content != "" {
			ch <- StreamChunk{Content: t.Content}
		}
		for i := range t.ToolCalls {
			tc := t.ToolCalls[i]
			ch <- StreamChunk{ToolCall: &tc}
		}
		if t.Err != nil {
			ch <- StreamChunk{Error: t.Err}
			return
		}
		if t.Delay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(t.Delay):
			}
		}
		var usage TokenUsage
		if t.InputTokens > 0 || t.OutputTokens > 0 {
			usage = TokenUsage{
				InputTokens:  t.InputTokens,
				OutputTokens: t.OutputTokens,
				TotalTokens:  t.InputTokens + t.OutputTokens,
			}
		}
		ch <- StreamChunk{Done: true, Usage: usage}
	}()
	return ch, nil
}

// CreateMessage returns the current turn as a single response.
func (p *ScriptedProvider) CreateMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	t, reqIdx := p.current()
	p.mu.Lock()
	if reqIdx < len(p.requests) {
		p.requests[reqIdx] = req
	}
	p.mu.Unlock()
	if t.Err != nil {
		return nil, t.Err
	}
	return &MessageResponse{
		Content:          t.Content,
		ReasoningContent: t.Reasoning,
		ToolCalls:        t.ToolCalls,
	}, nil
}

func (p *ScriptedProvider) SupportsTools() bool     { return true }
func (p *ScriptedProvider) GetModel() string        { return "scripted" }
func (p *ScriptedProvider) GetProviderName() string { return "scripted" }
