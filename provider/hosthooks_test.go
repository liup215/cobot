package provider

// hosthooks_test.go covers the ProviderConfig extension points hosts (the
// LTEDU desktop app) rely on: static + dynamic request headers, the content
// transform that turns the read tool's [image: path] markers into
// multimodal parts, the per-provider default output cap, stream usage
// reporting (stream_options.include_usage) and usage chunks that ride along
// with choices.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liup215/cobot/types"
)

func hostHookProviderConfig(url string) ProviderConfig {
	return ProviderConfig{
		APIKey:  "test-key",
		Model:   "gpt-4",
		BaseURL: url,
		Headers: map[string]string{
			"User-Agent": "ltedu-agent-desktop/1.0",
			"X-Static":   "from-config",
			"X-Conflict": "config-wins",
		},
		DynamicHeaders: func() map[string]string {
			return map[string]string{
				"X-Session":     "sess-1",
				"X-Conflict":    "dynamic-loses",
				"X-Empty-Skips": "",
			}
		},
		ContentTransform: func(content, role string) interface{} {
			if role == "tool" && strings.Contains(content, "[image: ") {
				// Mimic the desktop policy: tool-role images become a
				// placeholder instead of parts.
				return "[tool image placeholder]"
			}
			if strings.Contains(content, "[image: ") {
				return []interface{}{
					map[string]interface{}{"type": "text", "text": "text before"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}},
				}
			}
			return content
		},
		MaxTokens: 7777,
	}
}

// Static config headers must win over dynamic ones; empty dynamic values
// must be skipped; Accept must be set on streaming requests only.
func TestOpenAIStreamRequestHeadersAndHooks(t *testing.T) {
	var gotBody []byte
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIProviderFromConfig(hostHookProviderConfig(srv.URL))
	ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "look [image: /tmp/a.png]"},
			{Role: types.RoleTool, Content: "read [image: /tmp/b.png]"},
		},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	for range ch { // drain
	}

	if gotHeader.Get("User-Agent") != "ltedu-agent-desktop/1.0" {
		t.Errorf("User-Agent = %q", gotHeader.Get("User-Agent"))
	}
	if gotHeader.Get("X-Session") != "sess-1" {
		t.Errorf("X-Session = %q", gotHeader.Get("X-Session"))
	}
	if gotHeader.Get("X-Conflict") != "config-wins" {
		t.Errorf("X-Conflict = %q, want config-wins (static headers beat dynamic)", gotHeader.Get("X-Conflict"))
	}
	if gotHeader.Get("X-Empty-Skips") != "" {
		t.Errorf("empty dynamic value should be skipped, got %q", gotHeader.Get("X-Empty-Skips"))
	}
	if gotHeader.Get("Accept") != "text/event-stream" {
		t.Errorf("Accept = %q", gotHeader.Get("Accept"))
	}

	// Content transform: user-role marker became parts, tool-role marker
	// became the placeholder string.
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens     int `json:"max_tokens"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	if !strings.Contains(string(body.Messages[0].Content), `"type":"image_url"`) {
		t.Errorf("user content parts missing: %s", body.Messages[0].Content)
	}
	if string(body.Messages[1].Content) != `"[tool image placeholder]"` {
		t.Errorf("tool content = %s", body.Messages[1].Content)
	}
	if body.MaxTokens != 7777 {
		t.Errorf("max_tokens = %d, want provider default 7777", body.MaxTokens)
	}
	if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
		t.Errorf("stream_options.include_usage not set: %s", gotBody)
	}
}

// NoStreamUsage must omit stream_options for gateways that reject it.
func TestOpenAIStreamUsageOptOut(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := hostHookProviderConfig(srv.URL)
	cfg.NoStreamUsage = true
	p := NewOpenAIProviderFromConfig(cfg)
	ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	for range ch {
	}
	if strings.Contains(string(gotBody), "stream_options") {
		t.Errorf("stream_options present despite NoStreamUsage: %s", gotBody)
	}
}

// The non-streaming path must apply the same headers/transform/cap.
func TestOpenAIMessageRequestHooks(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	p := NewOpenAIProviderFromConfig(hostHookProviderConfig(srv.URL))
	resp, err := p.CreateMessage(context.Background(), &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if gotHeader.Get("X-Session") != "sess-1" || gotHeader.Get("X-Conflict") != "config-wins" {
		t.Errorf("headers not applied on non-stream path: %v", gotHeader)
	}
	if gotHeader.Get("Accept") != "" {
		t.Errorf("Accept must be stream-only, got %q", gotHeader.Get("Accept"))
	}
	if !strings.Contains(string(gotBody), `"max_tokens":7777`) {
		t.Errorf("max_tokens default missing: %s", gotBody)
	}
}

// Usage must surface even when the server attaches it to a contentful final
// chunk (DeepSeek style), not only to an empty-choices chunk.
func TestOpenAIStreamUsageWithChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3,\"total_tokens\":14}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIProviderFromConfig(hostHookProviderConfig(srv.URL))
	ch, err := p.CreateMessageStream(context.Background(), &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	var usage TokenUsage
	for chunk := range ch {
		if chunk.Usage.TotalTokens > 0 {
			usage = chunk.Usage
		}
	}
	if usage.TotalTokens != 14 || usage.InputTokens != 11 || usage.OutputTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}

// contentText must flatten array content and pass strings through.
func TestContentText(t *testing.T) {
	if got := contentText("plain"); got != "plain" {
		t.Errorf("string: %q", got)
	}
	if got := contentText(nil); got != "" {
		t.Errorf("nil: %q", got)
	}
	parts := []interface{}{
		map[string]interface{}{"type": "text", "text": "a"},
		map[string]interface{}{"type": "text", "text": "b"},
	}
	if got := contentText(parts); got != "a\nb\n" {
		t.Errorf("parts: %q", got)
	}
}

// The default registry must advertise its provider kinds and honor config
// hooks through Create.
func TestDefaultRegistryListAndConfigWiring(t *testing.T) {
	if DefaultRegistry.Count() == 0 {
		InitDefaultRegistry()
	}
	names := DefaultRegistry.List()
	want := map[string]bool{"openai": false, "anthropic": false}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for n, found := range want {
		if !found {
			t.Errorf("provider %q missing from registry list %v", n, names)
		}
	}

	p, err := DefaultRegistry.Create("openai", ProviderConfig{
		APIKey:  "k",
		Model:   "m",
		BaseURL: "https://gw.example/v1",
		Headers: map[string]string{"X-Extra": "1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	op, ok := p.(*OpenAIProvider)
	if !ok {
		t.Fatalf("type = %T", p)
	}
	if op.headers["X-Extra"] != "1" {
		t.Errorf("headers not wired through registry: %+v", op.headers)
	}
}
