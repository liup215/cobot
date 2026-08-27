package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liup215/cobot/types"
)

// newAnthropicTestProvider returns an Anthropic provider pointed at the test server.
func newAnthropicTestProvider(t *testing.T, url string) *AnthropicProvider {
	t.Helper()
	return NewAnthropicProvider("test-key", "claude-sonnet-4-5", url)
}

// testStreamCtx bounds every stream test so a parsing bug cannot hang the run.
func testStreamCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// sseEvent formats one complete SSE event (event line + data line + blank line).
func sseEvent(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

// dataEvent formats a bare data line with no event line, proving the parser
// keys off the payload type when a proxy strips event lines.
func dataEvent(data string) string {
	return "data: " + data + "\n\n"
}

// newAnthropicSSEServer starts a server replying with a 200
// text/event-stream body made of the given raw events.
func newAnthropicSSEServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}))
}

// newAnthropicErrorServer starts a server replying with a non-200 status.
func newAnthropicErrorServer(t *testing.T, status int, retryAfter, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// drainStream consumes the whole stream and returns the chunks in order.
func drainStream(t *testing.T, ch <-chan StreamChunk) (content, reasoning string, chunks []StreamChunk) {
	t.Helper()
	for chunk := range ch {
		chunks = append(chunks, chunk)
		if chunk.Content != "" {
			content += chunk.Content
		}
		if chunk.ReasoningContent != "" {
			reasoning += chunk.ReasoningContent
		}
	}
	return content, reasoning, chunks
}

// messageStartEvent builds the message_start event with the given usage.
func messageStartEvent(inputTokens int) string {
	return sseEvent("message_start",
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant",`+
			`"model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":`+
			strconv.Itoa(inputTokens)+`,"output_tokens":1}}}`)
}

// messageDeltaEvent builds a message_delta carrying a stop reason.
func messageDeltaEvent(stopReason string) string {
	return sseEvent("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null},`+
			`"usage":{"output_tokens":8}}`)
}

// A plain text turn: message_start → content_block_start/delta/stop →
// message_delta → message_stop. Also proves ping events are ignored and
// that a missing event line still parses.
func TestAnthropicStreamPlainText(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(12),
		sseEvent("ping", `{"type":"ping"}`),
		dataEvent(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	content, reasoning, chunks := drainStream(t, ch)
	if content != "Hello world" {
		t.Errorf("content = %q, want %q", content, "Hello world")
	}
	if reasoning != "" {
		t.Errorf("reasoning = %q, want empty", reasoning)
	}

	var final *StreamChunk
	dones := 0
	for i := range chunks {
		if chunks[i].Error != nil {
			t.Fatalf("unexpected stream error: %v", chunks[i].Error)
		}
		if chunks[i].Done {
			dones++
			c := chunks[i]
			final = &c
		}
	}
	if dones != 1 {
		t.Fatalf("got %d Done chunks, want exactly 1", dones)
	}
	if final.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", final.FinishReason, "stop")
	}
	if final.Usage.InputTokens != 12 || final.Usage.OutputTokens != 8 || final.Usage.TotalTokens != 20 {
		t.Errorf("Usage = %+v, want in=12 out=8 total=20", final.Usage)
	}
}

// toolUseStream is a turn that streams one tool_use block in two
// input_json_delta fragments, preceded by a text block.
func toolUseStream() []string {
	return []string{
		messageStartEvent(20),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check."}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"read_file","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	}
}

// Tool_use arguments arrive as incremental JSON fragments keyed by block
// index and must be assembled into a complete, valid ToolCall at block stop,
// before the final Done chunk.
func TestAnthropicStreamToolUseIncremental(t *testing.T) {
	srv := newAnthropicSSEServer(t, toolUseStream()...)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "read a.txt"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	content, _, chunks := drainStream(t, ch)
	if content != "Let me check." {
		t.Errorf("content = %q, want %q", content, "Let me check.")
	}

	var partials []StreamChunk
	var complete *ToolCall
	doneIdx, completeIdx := -1, -1
	for i, c := range chunks {
		if c.ToolCall == nil {
			if c.Done {
				doneIdx = i
			}
			continue
		}
		if c.IsPartial {
			partials = append(partials, c)
		} else {
			completeIdx = i
			complete = c.ToolCall
		}
	}

	if len(partials) != 2 {
		t.Fatalf("got %d partial tool-call chunks, want 2 (one per input_json_delta)", len(partials))
	}
	last := partials[len(partials)-1].ToolCall
	if last.ID != "toolu_01" || last.Name != "read_file" {
		t.Errorf("partial tool call id/name = %q/%q", last.ID, last.Name)
	}
	if last.Input != `{"path":"a.txt"}` {
		t.Errorf("partial accumulated input = %q", last.Input)
	}

	if complete == nil {
		t.Fatalf("no complete tool call chunk received")
	}
	if complete.ID != "toolu_01" || complete.Name != "read_file" {
		t.Errorf("complete tool call id/name = %q/%q", complete.ID, complete.Name)
	}
	if complete.Input != `{"path":"a.txt"}` {
		t.Errorf("complete tool call input = %q", complete.Input)
	}
	if doneIdx < 0 {
		t.Fatalf("stream never signalled Done")
	}
	if completeIdx > doneIdx {
		t.Errorf("complete tool call (chunk %d) must arrive before Done (chunk %d)", completeIdx, doneIdx)
	}
}

// Multiple tool_use blocks are finalized in block order, each as its own
// complete ToolCall chunk.
func TestAnthropicStreamMultipleToolCalls(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(30),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"read_file","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b","name":"list_files","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"dir\":\".\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "inspect"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)

	var complete []ToolCall
	for _, c := range chunks {
		if c.Error != nil {
			t.Fatalf("unexpected stream error: %v", c.Error)
		}
		if c.ToolCall != nil && !c.IsPartial {
			complete = append(complete, *c.ToolCall)
		}
	}
	if len(complete) != 2 {
		t.Fatalf("got %d complete tool calls, want 2", len(complete))
	}
	if complete[0].ID != "toolu_a" || complete[0].Name != "read_file" || complete[0].Input != `{"path":"a.txt"}` {
		t.Errorf("first tool call = %+v", complete[0])
	}
	if complete[1].ID != "toolu_b" || complete[1].Name != "list_files" || complete[1].Input != `{"dir":"."}` {
		t.Errorf("second tool call = %+v", complete[1])
	}
}

// Thinking blocks stream through the dedicated ReasoningContent channel and
// never leak into Content; signature deltas are metadata and are ignored.
func TestAnthropicStreamThinking(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(25),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Step 1: "}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check the file."}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "think about it"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	content, reasoning, chunks := drainStream(t, ch)
	if reasoning != "Step 1: check the file." {
		t.Errorf("reasoning = %q, want %q", reasoning, "Step 1: check the file.")
	}
	if content != "Answer" {
		t.Errorf("content = %q, want %q", content, "Answer")
	}
	if strings.Contains(reasoning, "sig-abc") || strings.Contains(content, "sig-abc") {
		t.Errorf("signature leaked into text: reasoning=%q content=%q", reasoning, content)
	}
	for _, c := range chunks {
		if c.Error != nil {
			t.Fatalf("unexpected stream error: %v", c.Error)
		}
	}
}

// A tool_use block whose input never streams (zero-argument tool) still
// yields a complete ToolCall with an empty object input.
func TestAnthropicStreamZeroArgToolCall(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(10),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_00","name":"list_files","input":{}}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "list"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)

	var calls []ToolCall
	for _, c := range chunks {
		if c.ToolCall != nil && !c.IsPartial {
			calls = append(calls, *c.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("got %d complete tool calls, want 1", len(calls))
	}
	if calls[0].ID != "toolu_00" || calls[0].Name != "list_files" {
		t.Errorf("tool call id/name = %q/%q", calls[0].ID, calls[0].Name)
	}
	if calls[0].Input != "{}" {
		t.Errorf("input = %q, want %q", calls[0].Input, "{}")
	}
}

// An error event mid-stream must surface through the stream (chunk.Error)
// and terminate it — the correction loop owns the retry decision.
func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(5),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial answer"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	content, _, chunks := drainStream(t, ch)
	if content != "partial answer" {
		t.Errorf("content before error = %q, want %q", content, "partial answer")
	}

	var streamErr error
	for _, c := range chunks {
		if c.Error != nil {
			streamErr = c.Error
			if !c.Done {
				t.Error("error chunk must also signal Done")
			}
		} else if c.Done {
			t.Error("no Done chunk may follow a stream error")
		}
	}
	if streamErr == nil {
		t.Fatal("no stream error chunk received")
	}
	msg := streamErr.Error()
	if !strings.Contains(msg, "overloaded_error") || !strings.Contains(msg, "Overloaded") {
		t.Errorf("stream error text = %q, want type and message", msg)
	}
}

// stop_reason values map onto the OpenAI-style finish reasons used by the
// rest of agentcore.
func TestAnthropicStreamStopReasonMapping(t *testing.T) {
	cases := []struct {
		stopReason string
		want       string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
		{"max_tokens", "length"},
		{"pause_turn", "pause_turn"}, // unknown values pass through
		{"", "stop"},                 // absent stop reason = natural end
	}
	for _, c := range cases {
		srv := newAnthropicSSEServer(t,
			messageStartEvent(3),
			messageDeltaEvent(c.stopReason),
			sseEvent("message_stop", `{"type":"message_stop"}`),
		)
		p := newAnthropicTestProvider(t, srv.URL)
		ctx, cancel := testStreamCtx(t)

		ch, err := p.CreateMessageStream(ctx, &MessageRequest{
			Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
		})
		if err != nil {
			cancel()
			srv.Close()
			t.Fatalf("stop_reason %q: CreateMessageStream: %v", c.stopReason, err)
		}
		_, _, chunks := drainStream(t, ch)
		cancel()
		srv.Close()

		var finish string
		for _, chunk := range chunks {
			if chunk.Error != nil {
				t.Fatalf("stop_reason %q: unexpected error: %v", c.stopReason, chunk.Error)
			}
			if chunk.Done {
				finish = chunk.FinishReason
			}
		}
		if finish != c.want {
			t.Errorf("stop_reason %q: FinishReason = %q, want %q", c.stopReason, finish, c.want)
		}
	}
}

// Non-200 connections must fail synchronously with an *HTTPError so the
// agent loop can retry retryable statuses (429/5xx, incl. Anthropic's 529
// overloaded) honoring Retry-After.
func TestAnthropicStreamSyncHTTPError(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		retryAfter   string
		body         string
		wantRetry    bool
		wantRetryAft time.Duration
	}{
		{
			name:         "429 with retry-after",
			status:       http.StatusTooManyRequests,
			retryAfter:   "7",
			body:         `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantRetry:    true,
			wantRetryAft: 7 * time.Second,
		},
		{
			name:      "529 overloaded",
			status:    529,
			body:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantRetry: true,
		},
		{
			name:      "400 invalid request",
			status:    http.StatusBadRequest,
			body:      `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`,
			wantRetry: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newAnthropicErrorServer(t, c.status, c.retryAfter, c.body)
			defer srv.Close()

			p := newAnthropicTestProvider(t, srv.URL)
			ctx, cancel := testStreamCtx(t)
			defer cancel()

			ch, err := p.CreateMessageStream(ctx, &MessageRequest{
				Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
			})
			if err == nil {
				t.Fatalf("status %d: want error, got stream", c.status)
			}
			he, ok := AsHTTPError(err)
			if !ok {
				t.Fatalf("status %d: error is not *HTTPError: %v", c.status, err)
			}
			if he.Status != c.status {
				t.Errorf("status %d: got HTTPError.Status=%d", c.status, he.Status)
			}
			if he.Retryable() != c.wantRetry {
				t.Errorf("status %d: Retryable()=%v want %v", c.status, he.Retryable(), c.wantRetry)
			}
			if he.RetryAfter != c.wantRetryAft {
				t.Errorf("status %d: RetryAfter=%v want %v", c.status, he.RetryAfter, c.wantRetryAft)
			}
			if ch != nil {
				t.Errorf("status %d: channel should be nil on sync error", c.status)
			}
			if !strings.Contains(err.Error(), "Anthropic") {
				t.Errorf("error text %q should identify the provider", err.Error())
			}
		})
	}
}

// A server that ends the stream without message_stop (EOF) must still
// produce the final Done chunk with the stop reason and usage seen so far.
func TestAnthropicStreamEOFWithoutMessageStop(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(7),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"bye"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		messageDeltaEvent("end_turn"),
		// no message_stop: connection drops
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	content, _, chunks := drainStream(t, ch)
	if content != "bye" {
		t.Errorf("content = %q, want %q", content, "bye")
	}
	var final *StreamChunk
	for i := range chunks {
		if chunks[i].Done {
			c := chunks[i]
			final = &c
		}
	}
	if final == nil {
		t.Fatal("EOF stream never signalled Done")
	}
	if final.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", final.FinishReason, "stop")
	}
	if final.Usage.InputTokens != 7 {
		t.Errorf("Usage.InputTokens = %d, want 7", final.Usage.InputTokens)
	}
}

// A stream truncated in the middle of a tool_use JSON argument is recovered
// best-effort at EOF: the block is finalized with brace repair, or skipped
// when the input is beyond repair.
func TestAnthropicStreamTruncatedToolCall(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(4),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cut","name":"read_file","input":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\""}}`),
		// EOF: no content_block_stop, no message_delta, no message_stop
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)

	var call *ToolCall
	for i := range chunks {
		if chunks[i].ToolCall != nil && !chunks[i].IsPartial {
			call = chunks[i].ToolCall
		}
	}
	if call == nil {
		t.Fatal("truncated tool call was not recovered")
	}
	if call.ID != "toolu_cut" || call.Name != "read_file" {
		t.Errorf("tool call id/name = %q/%q", call.ID, call.Name)
	}
	if call.Input != `{"path":"a.txt"}` {
		t.Errorf("input = %q, want repaired %q", call.Input, `{"path":"a.txt"}`)
	}
}

// Unknown event types and unknown delta types must be skipped without
// poisoning the stream.
func TestAnthropicStreamUnknownEventsIgnored(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(6),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{}}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		sseEvent("unknown_event", `{"type":"unknown_event"}`),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	content, _, chunks := drainStream(t, ch)
	if content != "ok" {
		t.Errorf("content = %q, want %q", content, "ok")
	}
	for _, c := range chunks {
		if c.Error != nil {
			t.Fatalf("unexpected stream error: %v", c.Error)
		}
	}
}

// Malformed or irrelevant SSE framing must be skipped without killing the
// stream: comment lines, id/retry fields, an event with no data, unparseable
// JSON payloads, a content_block_start with no block, a delta with no delta
// object, and a content_block_stop for an index that never started.
func TestAnthropicStreamMalformedEventsIgnored(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(3),
		": keep-alive comment\n\n",
		"retry: 5000\n\nid: 42\n\n",
		sseEvent("ping", ""), // event line with empty data
		sseEvent("ping", "{not json"),
		dataEvent(`{"type":"content_block_start","index":0}`), // no content_block
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1}`), // no delta
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":99}`),  // unknown index
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"still alive"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
		// message_delta that also refreshes input tokens.
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":11,"output_tokens":5}}`),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	content, _, chunks := drainStream(t, ch)
	if content != "still alive" {
		t.Errorf("content = %q, want %q", content, "still alive")
	}
	var final *StreamChunk
	for i := range chunks {
		if chunks[i].Error != nil {
			t.Fatalf("unexpected stream error: %v", chunks[i].Error)
		}
		if chunks[i].Done {
			c := chunks[i]
			final = &c
		}
	}
	if final == nil {
		t.Fatal("stream never signalled Done")
	}
	if final.Usage.InputTokens != 11 || final.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want in=11 out=5 (message_delta usage must update both counters)", final.Usage)
	}
}

// A delta arriving for a block whose content_block_start was lost must be
// recovered so the text is not silently dropped.
func TestAnthropicStreamDeltaWithoutBlockStart(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(1),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"rescued"}}`),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	content, _, chunks := drainStream(t, ch)
	if content != "rescued" {
		t.Errorf("content = %q, want %q", content, "rescued")
	}
	for _, c := range chunks {
		if c.Error != nil {
			t.Fatalf("unexpected stream error: %v", c.Error)
		}
	}
}

// A tool_use block without id or name cannot be dispatched: it must be
// skipped (with a log) while the rest of the stream still completes.
func TestAnthropicStreamToolCallMissingIdentity(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(1),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":""}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)
	for i := range chunks {
		if chunks[i].ToolCall != nil && !chunks[i].IsPartial {
			t.Errorf("identity-less tool call was emitted: %+v", chunks[i].ToolCall)
		}
		if chunks[i].Error != nil {
			t.Fatalf("unexpected stream error: %v", chunks[i].Error)
		}
	}
	if len(chunks) == 0 || !chunks[len(chunks)-1].Done {
		t.Error("stream must still finish after skipping the malformed tool call")
	}
}

// Tool arguments that are invalid JSON and beyond brace repair are skipped
// rather than dispatched as a broken tool call.
func TestAnthropicStreamToolCallUnrepairableJSON(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(1),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_bad","name":"read_file"}}`),
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\": }"}}`),
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)
	for i := range chunks {
		if chunks[i].ToolCall != nil && !chunks[i].IsPartial {
			t.Errorf("unparseable tool call was emitted: %+v", chunks[i].ToolCall)
		}
	}
	var final *StreamChunk
	for i := range chunks {
		if chunks[i].Done {
			c := chunks[i]
			final = &c
		}
	}
	if final == nil || final.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %v, want tool_calls (stop reason must survive the skip)", final)
	}
}

// Zero-argument tools may ship their input object inside content_block_start
// and emit no deltas at all; the stored input must survive finalization.
func TestAnthropicStreamToolCallInputFromBlockStart(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(1),
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"list_dir","input":{"dir":"."}}}`),
		// no input_json_delta at all
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)
	var call *ToolCall
	for i := range chunks {
		if chunks[i].ToolCall != nil && !chunks[i].IsPartial {
			call = chunks[i].ToolCall
		}
	}
	if call == nil {
		t.Fatal("tool call from content_block_start input was not emitted")
	}
	if call.ID != "toolu_0" || call.Name != "list_dir" || call.Input != `{"dir":"."}` {
		t.Errorf("tool call = %+v, want toolu_0/list_dir/{\"dir\":\".\"}", call)
	}
}

// An error event with no error object still produces a stream error chunk
// (so the correction loop sees something) instead of ending silently.
func TestAnthropicStreamErrorEventWithoutPayload(t *testing.T) {
	srv := newAnthropicSSEServer(t,
		messageStartEvent(1),
		sseEvent("error", `{"type":"error"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)
	errs := 0
	for _, c := range chunks {
		if c.Error != nil {
			errs++
			if !c.Done {
				t.Error("stream error chunk must carry Done")
			}
		}
	}
	if errs != 1 {
		t.Fatalf("got %d error chunks, want 1", errs)
	}
}

// A connection dropped mid-body (unexpected EOF, not a clean EOF) surfaces
// as a stream error chunk rather than a silent partial answer.
func TestAnthropicStreamReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response is not hijackable")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		// Promise 500 bytes, deliver one event, then cut the connection:
		// the client sees an unexpected EOF instead of a clean one.
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 500\r\n\r\n")
		buf.WriteString(`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n")
		buf.Flush()
	}))
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	_, _, chunks := drainStream(t, ch)
	errs := 0
	for _, c := range chunks {
		if c.Error != nil {
			errs++
			if !c.Done {
				t.Error("read error chunk must carry Done")
			}
		}
	}
	if errs == 0 {
		t.Fatal("aborted stream produced no error chunk")
	}
}
