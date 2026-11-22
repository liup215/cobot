package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/liup215/cobot/types"
)

// TestCreateMessageStream_TrailingUsageChunk pins the OpenAI/vLLM stream
// layout: content deltas, a finish chunk, then a SEPARATE chunk with empty
// choices that carries the usage, then [DONE]. The provider must forward that
// usage to the consumer — the agent loop only ever sees real token counts
// through it (context display, per-turn usage facts, billing).
func TestCreateMessageStream_TrailingUsageChunk(t *testing.T) {
	const body = `data: {"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"1","choices":[],"usage":{"prompt_tokens":32,"completion_tokens":29,"total_tokens":61}}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	p := NewOpenAIProvider("k", "m", srv.URL)
	ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	var usage *TokenUsage
	var content string
	done := 0
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("stream error: %v", chunk.Error)
		}
		if chunk.Content != "" {
			content += chunk.Content
		}
		if chunk.Usage.TotalTokens > 0 {
			u := chunk.Usage
			usage = &u
		}
		if chunk.Done {
			done++
			if chunk.FinishReason == "" {
				t.Errorf("done chunk lost the finish reason")
			}
		}
	}
	if content != "hi" {
		t.Errorf("content = %q, want %q", content, "hi")
	}
	if usage == nil {
		t.Fatal("no usage chunk emitted — the trailing usage-only chunk was dropped")
	}
	if usage.InputTokens != 32 || usage.OutputTokens != 29 || usage.TotalTokens != 61 {
		t.Errorf("usage = %+v, want in=32 out=29 total=61", *usage)
	}
	if done != 1 {
		t.Errorf("done chunks = %d, want exactly 1", done)
	}
}
