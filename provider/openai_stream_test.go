package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liup215/agentcore/types"
)

// newStreamTestProvider returns an OpenAI provider pointed at the test server.
func newStreamTestProvider(t *testing.T, url string) *OpenAIProvider {
	t.Helper()
	return NewOpenAIProvider("test-key", "gpt-4", url)
}

func TestOpenAIStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newStreamTestProvider(t, srv.URL)
	ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	var content strings.Builder
	sawDone := false
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Error)
		}
		if chunk.Content != "" {
			content.WriteString(chunk.Content)
		}
		if chunk.Done {
			sawDone = true
		}
	}
	if content.String() != "Hello world" {
		t.Errorf("content = %q", content.String())
	}
	if !sawDone {
		t.Error("stream never signalled Done")
	}
}

// Non-2xx responses must surface as *HTTPError synchronously (before any
// streaming begins), so the agent loop can retry retryable statuses.
func TestOpenAIStreamSyncHTTPError(t *testing.T) {
	cases := []struct {
		status       int
		retryAfter   string
		wantRetry    bool
		wantRetryAft time.Duration
	}{
		{http.StatusTooManyRequests, "7", true, 7 * time.Second},
		{http.StatusServiceUnavailable, "", true, 0},
		{http.StatusBadRequest, "", false, 0},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit_error"}}`))
		}))
		p := newStreamTestProvider(t, srv.URL)

		ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
			Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
		})
		srv.Close()
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
		// Message text must include the API error detail (baseline text).
		if !strings.Contains(err.Error(), "slow down") {
			t.Errorf("status %d: error text %q missing API message", c.status, err.Error())
		}
	}
}
