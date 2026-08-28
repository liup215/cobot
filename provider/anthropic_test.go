package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/liup215/cobot/types"
)

// capturedRequest records what the test server received.
type capturedRequest struct {
	path   string
	header http.Header
	body   []byte
}

// newAnthropicCaptureServer starts a server that records the request and
// replies with the given raw SSE events.
func newAnthropicCaptureServer(t *testing.T, sseEvents ...string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.header = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		captured.body = body
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range sseEvents {
			_, _ = io.WriteString(w, e)
		}
	}))
	return srv, captured
}

// bodyMap decodes a JSON request body into a generic map so the test asserts
// the actual wire format rather than a mirror of the encoding structs.
func bodyMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, raw)
	}
	return m
}

func mapAt(t *testing.T, v any, label string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want object, got %T (%v)", label, v, v)
	}
	return m
}

func sliceAt(t *testing.T, v any, label string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want array, got %T (%v)", label, v, v)
	}
	return s
}

func stringAt(t *testing.T, m map[string]any, key, label string) string {
	t.Helper()
	s, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s[%s]: want string, got %T (%v)", label, key, m[key], m[key])
	}
	return s
}

func floatAt(t *testing.T, m map[string]any, key, label string) float64 {
	t.Helper()
	f, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s[%s]: want number, got %T (%v)", label, key, m[key], m[key])
	}
	return f
}

func boolAt(t *testing.T, m map[string]any, key, label string) bool {
	t.Helper()
	b, ok := m[key].(bool)
	if !ok {
		t.Fatalf("%s[%s]: want bool, got %T (%v)", label, key, m[key], m[key])
	}
	return b
}

func TestNewAnthropicProviderDefaults(t *testing.T) {
	p := NewAnthropicProvider("", "", "")
	if p.GetModel() != defaultAnthropicModel {
		t.Errorf("default model = %q, want %q", p.GetModel(), defaultAnthropicModel)
	}
	if p.GetBaseURL() != defaultAnthropicBaseURL {
		t.Errorf("default base URL = %q, want %q", p.GetBaseURL(), defaultAnthropicBaseURL)
	}
	if p.GetAPIVersion() != defaultAnthropicVersion {
		t.Errorf("API version = %q, want %q", p.GetAPIVersion(), defaultAnthropicVersion)
	}
	if !p.SupportsTools() {
		t.Error("SupportsTools() = false, want true")
	}
	if p.GetProviderName() != "anthropic" {
		t.Errorf("provider name = %q, want %q", p.GetProviderName(), "anthropic")
	}

	// Explicit values win; an empty key falls back to the env var.
	t.Setenv("ANTHROPIC_API_KEY", "env-key")
	if got := NewAnthropicProvider("", "claude-3-5-haiku", "").apiKey; got != "env-key" {
		t.Errorf("env fallback key = %q, want %q", got, "env-key")
	}
	if got := NewAnthropicProvider("explicit", "claude-3-5-haiku", "").apiKey; got != "explicit" {
		t.Errorf("explicit key = %q, want %q", got, "explicit")
	}
}

// A missing key (explicit and from the environment) must fail fast, before
// any HTTP request is made.
func TestAnthropicProvider_MissingKey(t *testing.T) {
	t.Setenv(anthropicAPIKeyEnv, "")
	p := NewAnthropicProvider("", "claude-sonnet-4-5", "")
	if p.apiKey != "" {
		t.Fatalf("apiKey = %q, want empty", p.apiKey)
	}
	req := &MessageRequest{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}}
	if _, err := p.CreateMessageStream(context.Background(), req); err == nil {
		t.Error("CreateMessageStream: want error for missing API key")
	}
	if _, err := p.CreateMessage(context.Background(), req); err == nil {
		t.Error("CreateMessage: want error for missing API key")
	}
}

func TestBuildAnthropicURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://api.anthropic.com", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/", "https://api.anthropic.com/v1/messages"},
		{"https://proxy.example.com", "https://proxy.example.com/v1/messages"},
		{"https://proxy.example.com/v1", "https://proxy.example.com/v1/messages"},
		{"https://proxy.example.com/v1/", "https://proxy.example.com/v1/messages"},
		{"https://proxy.example.com/v1/messages", "https://proxy.example.com/v1/messages"},
		{defaultAnthropicBaseURL, defaultAnthropicBaseURL + "/v1/messages"},
	}
	for _, c := range cases {
		if got := buildAnthropicURL(c.in); got != c.want {
			t.Errorf("buildAnthropicURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The request body must be a valid Messages API payload: x-api-key +
// anthropic-version headers, top-level system with an ephemeral cache
// marker, input_schema tool definitions, tool_use input as a JSON object
// and consecutive tool results grouped into one user message.
func TestAnthropicProvider_RequestShape(t *testing.T) {
	srv, captured := newAnthropicCaptureServer(t,
		messageStartEvent(1),
		messageDeltaEvent("tool_use"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	req := &MessageRequest{
		SystemPrompt: "You are a helpful assistant.",
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "What is the weather?"},
			{Role: types.RoleAssistant, Content: "Let me check.",
				ToolCalls: []types.ToolCall{
					{ID: "toolu_1", Name: "get_weather", Input: []byte(`{"city":"Paris"}`)},
					{ID: "toolu_2", Name: "get_humidity", Input: []byte(`{}`)},
				}},
			{Role: types.RoleTool, ToolCallID: "toolu_1", Content: "15 degrees"},
			{Role: types.RoleTool, ToolCallID: "toolu_2", Content: "humidity 40%"},
		},
		Tools: []ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the weather for a city",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		}},
		MaxTokens:   1024,
		Temperature: 0.2,
		ToolChoice:  ToolChoiceAuto,
	}

	ctx, cancel := testStreamCtx(t)
	defer cancel()
	if _, err := p.CreateMessageStream(ctx, req); err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	// Connection metadata.
	if captured.path != "/v1/messages" {
		t.Errorf("path = %q, want %q", captured.path, "/v1/messages")
	}
	if got := captured.header.Get("x-api-key"); got != "test-key" {
		t.Errorf("x-api-key = %q, want %q", got, "test-key")
	}
	if got := captured.header.Get("anthropic-version"); got != defaultAnthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", got, defaultAnthropicVersion)
	}
	if got := captured.header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	body := bodyMap(t, captured.body)

	// Scalar request fields.
	if got := stringAt(t, body, "model", "body"); got != "claude-sonnet-4-5" {
		t.Errorf("model = %q", got)
	}
	if got := floatAt(t, body, "max_tokens", "body"); got != 1024 {
		t.Errorf("max_tokens = %v, want 1024", got)
	}
	if got := floatAt(t, body, "temperature", "body"); got != 0.2 {
		t.Errorf("temperature = %v, want 0.2", got)
	}
	if !boolAt(t, body, "stream", "body") {
		t.Error("stream = false, want true")
	}

	// system: one text block carrying the ephemeral cache marker.
	system := sliceAt(t, body["system"], "system")
	if len(system) != 1 {
		t.Fatalf("system has %d blocks, want 1", len(system))
	}
	sysBlock := mapAt(t, system[0], "system[0]")
	if got := stringAt(t, sysBlock, "type", "system[0]"); got != "text" {
		t.Errorf("system[0].type = %q, want text", got)
	}
	if got := stringAt(t, sysBlock, "text", "system[0]"); got != req.SystemPrompt {
		t.Errorf("system[0].text = %q", got)
	}
	cache := mapAt(t, sysBlock["cache_control"], "system[0].cache_control")
	if got := stringAt(t, cache, "type", "system[0].cache_control"); got != "ephemeral" {
		t.Errorf("cache_control.type = %q, want ephemeral", got)
	}

	// tools: input_schema instead of OpenAI's function parameters.
	tools := sliceAt(t, body["tools"], "tools")
	if len(tools) != 1 {
		t.Fatalf("tools has %d entries, want 1", len(tools))
	}
	tool := mapAt(t, tools[0], "tools[0]")
	if got := stringAt(t, tool, "name", "tools[0]"); got != "get_weather" {
		t.Errorf("tools[0].name = %q", got)
	}
	if got := stringAt(t, tool, "description", "tools[0]"); got != "Get the weather for a city" {
		t.Errorf("tools[0].description = %q", got)
	}
	schema := mapAt(t, tool["input_schema"], "tools[0].input_schema")
	if got := stringAt(t, schema, "type", "tools[0].input_schema"); got != "object" {
		t.Errorf("input_schema.type = %q, want object", got)
	}
	if _, ok := schema["properties"]; !ok {
		t.Error("input_schema.properties missing")
	}
	toolChoice := mapAt(t, body["tool_choice"], "body.tool_choice")
	if got := stringAt(t, toolChoice, "type", "tool_choice"); got != "auto" {
		t.Errorf("tool_choice.type = %q, want auto", got)
	}

	// messages: user → assistant(text + tool_use×2) → user(tool_result×2).
	messages := sliceAt(t, body["messages"], "body.messages")
	if len(messages) != 3 {
		t.Fatalf("messages has %d entries, want 3", len(messages))
	}
	first := mapAt(t, messages[0], "messages[0]")
	if got := stringAt(t, first, "role", "messages[0]"); got != "user" {
		t.Errorf("messages[0].role = %q, want user", got)
	}
	firstBlocks := sliceAt(t, first["content"], "messages[0].content")
	if len(firstBlocks) != 1 {
		t.Fatalf("messages[0] has %d blocks, want 1", len(firstBlocks))
	}
	firstText := mapAt(t, firstBlocks[0], "messages[0].content[0]")
	if got := stringAt(t, firstText, "type", "messages[0].content[0]"); got != "text" {
		t.Errorf("messages[0].content[0].type = %q, want text", got)
	}
	if got := stringAt(t, firstText, "text", "messages[0].content[0]"); got != "What is the weather?" {
		t.Errorf("messages[0].content[0].text = %q", got)
	}

	second := mapAt(t, messages[1], "messages[1]")
	if got := stringAt(t, second, "role", "messages[1]"); got != "assistant" {
		t.Errorf("messages[1].role = %q, want assistant", got)
	}
	secondBlocks := sliceAt(t, second["content"], "messages[1].content")
	if len(secondBlocks) != 3 {
		t.Fatalf("messages[1] has %d blocks, want 3 (text + 2 tool_use)", len(secondBlocks))
	}
	textBlock := mapAt(t, secondBlocks[0], "messages[1].content[0]")
	if got := stringAt(t, textBlock, "type", "messages[1].content[0]"); got != "text" {
		t.Errorf("messages[1].content[0].type = %q, want text", got)
	}
	if got := stringAt(t, textBlock, "text", "messages[1].content[0]"); got != "Let me check." {
		t.Errorf("messages[1].content[0].text = %q", got)
	}

	toolUse := mapAt(t, secondBlocks[1], "messages[1].content[1]")
	if got := stringAt(t, toolUse, "type", "messages[1].content[1]"); got != "tool_use" {
		t.Errorf("messages[1].content[1].type = %q, want tool_use", got)
	}
	if got := stringAt(t, toolUse, "id", "messages[1].content[1]"); got != "toolu_1" {
		t.Errorf("messages[1].content[1].id = %q, want toolu_1", got)
	}
	if got := stringAt(t, toolUse, "name", "messages[1].content[1]"); got != "get_weather" {
		t.Errorf("messages[1].content[1].name = %q, want get_weather", got)
	}
	input := mapAt(t, toolUse["input"], "messages[1].content[1].input")
	if got := stringAt(t, input, "city", "tool input"); got != "Paris" {
		t.Errorf("tool input.city = %q, want Paris (input must be an object, not a string)", got)
	}
	zeroUse := mapAt(t, secondBlocks[2], "messages[1].content[2]")
	zeroInput := mapAt(t, zeroUse["input"], "messages[1].content[2].input")
	if len(zeroInput) != 0 {
		t.Errorf("toolu_2 input = %v, want empty object", zeroInput)
	}

	third := mapAt(t, messages[2], "messages[2]")
	if got := stringAt(t, third, "role", "messages[2]"); got != "user" {
		t.Errorf("messages[2].role = %q, want user (tool_result rides in a user message)", got)
	}
	resultBlocks := sliceAt(t, third["content"], "messages[2].content")
	if len(resultBlocks) != 2 {
		t.Fatalf("messages[2] has %d blocks, want 2 (tool results grouped)", len(resultBlocks))
	}
	for i, wantID := range []string{"toolu_1", "toolu_2"} {
		block := mapAt(t, resultBlocks[i], "messages[2].content[i]")
		if got := stringAt(t, block, "type", "tool_result"); got != "tool_result" {
			t.Errorf("messages[2].content[%d].type = %q, want tool_result", i, got)
		}
		if got := stringAt(t, block, "tool_use_id", "tool_result"); got != wantID {
			t.Errorf("messages[2].content[%d].tool_use_id = %q, want %q", i, got, wantID)
		}
		if got := stringAt(t, block, "content", "tool_result"); got == "" {
			t.Errorf("messages[2].content[%d].content is empty", i)
		}
	}
}

// System-role history content must ride in the top-level system field with
// the ephemeral marker on the LAST block, and a request without any system
// content must omit the field entirely.
func TestAnthropicProvider_SystemCacheControl(t *testing.T) {
	srv, captured := newAnthropicCaptureServer(t,
		messageStartEvent(1),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	if _, err := p.CreateMessageStream(ctx, &MessageRequest{
		SystemPrompt: "Base instructions.",
		Messages: []types.Message{
			{Role: types.RoleSystem, Content: "[Context compacted: older messages were summarized.]"},
			{Role: types.RoleUser, Content: "hi"},
		},
	}); err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	body := bodyMap(t, captured.body)
	system := sliceAt(t, body["system"], "system")
	if len(system) != 2 {
		t.Fatalf("system has %d blocks, want 2 (explicit prompt + history system message)", len(system))
	}
	first := mapAt(t, system[0], "system[0]")
	if got := stringAt(t, first, "text", "system[0]"); got != "Base instructions." {
		t.Errorf("system[0].text = %q", got)
	}
	if _, ok := first["cache_control"]; ok {
		t.Error("only the last system block may carry cache_control")
	}
	lastBlock := mapAt(t, system[1], "system[1]")
	if got := stringAt(t, lastBlock, "text", "system[1]"); !strings.Contains(got, "[Context compacted") {
		t.Errorf("system[1].text = %q", got)
	}
	lastCache := mapAt(t, lastBlock["cache_control"], "system[1].cache_control")
	if got := stringAt(t, lastCache, "type", "system[1].cache_control"); got != "ephemeral" {
		t.Errorf("cache_control.type = %q, want ephemeral", got)
	}

	// No system prompt at all → the field is omitted.
	srv2, captured2 := newAnthropicCaptureServer(t,
		messageStartEvent(1),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv2.Close()
	p2 := newAnthropicTestProvider(t, srv2.URL)
	ctx2, cancel2 := testStreamCtx(t)
	defer cancel2()
	if _, err := p2.CreateMessageStream(ctx2, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}
	body2 := bodyMap(t, captured2.body)
	if _, ok := body2["system"]; ok {
		t.Error("system field must be omitted when there is no system prompt")
	}
	if _, ok := body2["tools"]; ok {
		t.Error("tools field must be omitted when no tools are supplied")
	}
	if _, ok := body2["tool_choice"]; ok {
		t.Error("tool_choice field must be omitted when no tools are supplied")
	}
}

// A tool declared without input_schema still gets a valid (empty object)
// schema, because the Messages API requires the field; an explicit schema is
// forwarded untouched, and tool_choice=required maps to Anthropic's "any".
func TestAnthropicProvider_DefaultToolSchema(t *testing.T) {
	srv, captured := newAnthropicCaptureServer(t,
		messageStartEvent(1),
		messageDeltaEvent("end_turn"),
		sseEvent("message_stop", `{"type":"message_stop"}`),
	)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	if _, err := p.CreateMessageStream(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
		Tools: []ToolDefinition{
			{Name: "no_schema", Description: "schema missing"},
			{Name: "with_schema", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: ToolChoiceRequired,
	}); err != nil {
		t.Fatalf("CreateMessageStream: %v", err)
	}

	tools := sliceAt(t, bodyMap(t, captured.body)["tools"], "tools")
	if len(tools) != 2 {
		t.Fatalf("tools has %d entries, want 2", len(tools))
	}
	fallback := mapAt(t, mapAt(t, tools[0], "tools[0]")["input_schema"], "tools[0].input_schema")
	if got := stringAt(t, fallback, "type", "tools[0].input_schema"); got != "object" {
		t.Errorf("default input_schema.type = %q, want object", got)
	}
	if _, ok := fallback["properties"]; !ok {
		t.Error("default input_schema must carry an (empty) properties object")
	}
	explicit := mapAt(t, mapAt(t, tools[1], "tools[1]")["input_schema"], "tools[1].input_schema")
	if len(explicit) != 1 {
		t.Errorf("explicit input_schema keys = %v, want only type", explicit)
	}

	choice := mapAt(t, bodyMap(t, captured.body)["tool_choice"], "tool_choice")
	if got := stringAt(t, choice, "type", "tool_choice"); got != "any" {
		t.Errorf("tool_choice.type = %q, want any (required)", got)
	}
}

// SetHTTPClient must replace the transport used for requests.
func TestAnthropicProvider_SetHTTPClient(t *testing.T) {
	p := NewAnthropicProvider("k", "m", "")
	custom := &http.Client{Timeout: 5 * time.Second}
	p.SetHTTPClient(custom)
	if p.httpClient != custom {
		t.Error("SetHTTPClient did not replace the HTTP client")
	}
}

// Connection-level failures (nothing listening) surface as retryable
// *HTTPError with Status 0 on both entry points, so the agent loop's retry
// policy can treat them like any other transient network fault.
func TestAnthropicProvider_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens on the port anymore

	p := NewAnthropicProvider("k", "claude-sonnet-4-5", url)
	req := &MessageRequest{Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}}}

	ctx, cancel := testStreamCtx(t)
	defer cancel()

	ch, err := p.CreateMessageStream(ctx, req)
	if err == nil {
		t.Fatal("CreateMessageStream: want network error")
	}
	if ch != nil {
		t.Error("CreateMessageStream: channel must be nil on network error")
	}
	he, ok := AsHTTPError(err)
	if !ok || he.Status != 0 || !he.Retryable() {
		t.Errorf("CreateMessageStream error = %v, want *HTTPError{Status:0, Retryable:true}", err)
	}

	if _, err := p.CreateMessage(ctx, req); err == nil {
		t.Fatal("CreateMessage: want network error")
	} else if he, ok := AsHTTPError(err); !ok || he.Status != 0 || !he.Retryable() {
		t.Errorf("CreateMessage error = %v, want *HTTPError{Status:0, Retryable:true}", err)
	}
}

// A 200 body that is not JSON must produce a parse error, not a panic or an
// empty response.
func TestAnthropicCreateMessageInvalidBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `<html>not json</html>`)
	}))
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	if _, err := p.CreateMessage(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	}); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want a parse error", err)
	}
}

// Multiple thinking blocks in one response are joined with a newline in the
// single ReasoningContent field.
func TestAnthropicCreateMessageMultipleThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_t","type":"message","role":"assistant","model":"claude-sonnet-4-5",
			"content":[
				{"type":"thinking","thinking":"first","signature":"s1"},
				{"type":"thinking","thinking":"second","signature":"s2"}
			],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":5,"output_tokens":6}
		}`)
	}))
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	resp, err := p.CreateMessage(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	if resp.ReasoningContent != "first\nsecond" {
		t.Errorf("ReasoningContent = %q, want %q", resp.ReasoningContent, "first\nsecond")
	}
	if resp.Content != "" {
		t.Errorf("Content = %q, want empty", resp.Content)
	}
}

// The non-streaming path maps content blocks back: text → Content,
// thinking → ReasoningContent, tool_use → ToolCalls with object input.
func TestAnthropicCreateMessage(t *testing.T) {
	var captured *capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = &capturedRequest{path: r.URL.Path, header: r.Header.Clone()}
		captured.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_9","type":"message","role":"assistant","model":"claude-sonnet-4-5",
			"content":[
				{"type":"thinking","thinking":"Check the file first.","signature":"sig-1"},
				{"type":"text","text":"Reading"},
				{"type":"tool_use","id":"toolu_9","name":"read_file","input":{"path":"a.txt"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":33,"output_tokens":21}
		}`)
	}))
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	resp, err := p.CreateMessage(ctx, &MessageRequest{
		Messages:  []types.Message{{Role: types.RoleUser, Content: "hi"}},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}

	if resp.Content != "Reading" {
		t.Errorf("Content = %q, want %q", resp.Content, "Reading")
	}
	if resp.ReasoningContent != "Check the file first." {
		t.Errorf("ReasoningContent = %q, want thinking text", resp.ReasoningContent)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Name != "read_file" {
		t.Errorf("tool call id/name = %q/%q", tc.ID, tc.Name)
	}
	if tc.Input != `{"path":"a.txt"}` {
		t.Errorf("tool call input = %q", tc.Input)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 33 || resp.Usage.OutputTokens != 21 || resp.Usage.TotalTokens != 54 {
		t.Errorf("Usage = %+v, want in=33 out=21 total=54", resp.Usage)
	}

	// Non-streaming requests must not set stream, and still hit /v1/messages.
	if captured == nil {
		t.Fatal("server never received a request")
	}
	if captured.path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", captured.path)
	}
	body := bodyMap(t, captured.body)
	if boolAt(t, body, "stream", "body") {
		t.Error("stream = true on the non-streaming path")
	}
	if got := floatAt(t, body, "max_tokens", "body"); got != 512 {
		t.Errorf("max_tokens = %v, want 512", got)
	}
}

// Non-200 on the non-streaming path also surfaces as an *HTTPError.
func TestAnthropicCreateMessageSyncError(t *testing.T) {
	srv := newAnthropicErrorServer(t, http.StatusTooManyRequests, "3",
		`{"type":"error","error":{"type":"rate_limit_error","message":"Number of requests too high"}}`)
	defer srv.Close()

	p := newAnthropicTestProvider(t, srv.URL)
	ctx, cancel := testStreamCtx(t)
	defer cancel()
	_, err := p.CreateMessage(ctx, &MessageRequest{
		Messages: []types.Message{{Role: types.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error, got response")
	}
	he, ok := AsHTTPError(err)
	if !ok {
		t.Fatalf("error is not *HTTPError: %v", err)
	}
	if he.Status != http.StatusTooManyRequests || !he.Retryable() {
		t.Errorf("HTTPError = %+v, want 429 retryable", he)
	}
	if he.RetryAfter != 3*time.Second {
		t.Errorf("RetryAfter = %v, want 3s", he.RetryAfter)
	}
	if !strings.Contains(err.Error(), "Number of requests too high") {
		t.Errorf("error text %q should carry the API message", err.Error())
	}
}

func TestAnthropicProvider_Registry(t *testing.T) {
	InitDefaultRegistry()
	if !IsProviderSupported("anthropic") {
		t.Fatal("expected 'anthropic' provider to be registered")
	}

	config := ProviderConfig{
		APIKey:  "test-key",
		Model:   "claude-sonnet-4-5",
		BaseURL: "https://proxy.example.com/v1",
	}
	prov, err := CreateDefault("anthropic", config)
	if err != nil {
		t.Fatalf("CreateDefault(anthropic): %v", err)
	}
	if prov.GetProviderName() != "anthropic" {
		t.Errorf("provider name = %q, want anthropic", prov.GetProviderName())
	}
	if prov.GetModel() != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5", prov.GetModel())
	}
	ap, ok := prov.(*AnthropicProvider)
	if !ok {
		t.Fatalf("registry returned %T, want *AnthropicProvider", prov)
	}
	if ap.GetBaseURL() != "https://proxy.example.com/v1" {
		t.Errorf("base URL = %q", ap.GetBaseURL())
	}
	if !ap.SupportsTools() {
		t.Error("registry-created provider must support tools")
	}
}

func TestNormalizeAnthropicToolInput(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "{}"},
		{"   ", "{}"},
		{`{"path":"a.txt"}`, `{"path":"a.txt"}`},
		{` {"path":"a.txt"} `, `{"path":"a.txt"}`},
		{`{"path":`, "{}"}, // truncated
		{`[1,2]`, "{}"},    // not an object
		{`"text"`, "{}"},   // not an object
		{`not json`, "{}"}, // not JSON
		{`{"n":1,"nested":{"a":true}}`, `{"n":1,"nested":{"a":true}}`},
	}
	for _, c := range cases {
		if got := string(normalizeAnthropicToolInput(c.in)); got != c.want {
			t.Errorf("normalizeAnthropicToolInput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Mapping edge cases that do not need a server: empty user messages are
// dropped, empty assistant turns get a placeholder, tool results are queued
// into one user message, and system history rides in the system blocks.
func TestConvertMessagesToAnthropicEdgeCases(t *testing.T) {
	system, messages := convertMessagesToAnthropic([]types.Message{
		{Role: types.RoleSystem, Content: "history system note"},
		{Role: types.RoleUser, Content: "   "},   // dropped: empty content
		{Role: types.RoleAssistant, Content: ""}, // empty assistant turn
		{Role: types.RoleAssistant, Content: "with tools",
			ToolCalls: []types.ToolCall{{ID: "toolu_1", Name: "read_file", Input: []byte(`{"path":"a.txt"}`)}}},
		{Role: types.RoleTool, ToolCallID: "toolu_1", Content: "contents"},
		{Role: types.RoleTool, ToolCallID: "toolu_1", Content: ""}, // empty result
	})

	if len(system) != 1 || system[0].Type != "text" || system[0].Text != "history system note" {
		t.Errorf("system blocks = %+v, want the history system message", system)
	}

	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3 (assistant placeholder + assistant with tools + tool results)", len(messages))
	}
	placeholder := messages[0]
	if placeholder.Role != "assistant" {
		t.Fatalf("messages[0].role = %q, want assistant", placeholder.Role)
	}
	if len(placeholder.Content) != 1 || placeholder.Content[0].Type != "text" || placeholder.Content[0].Text != "(no content)" {
		t.Errorf("assistant placeholder = %+v, want a single '(no content)' text block", placeholder.Content)
	}

	withTools := messages[1]
	if len(withTools.Content) != 2 || withTools.Content[1].Type != "tool_use" {
		t.Fatalf("messages[1] content = %+v, want text + tool_use", withTools.Content)
	}
	got := withTools.Content[1]
	if got.ID != "toolu_1" || got.Name != "read_file" || string(got.Input) != `{"path":"a.txt"}` {
		t.Errorf("tool_use block = %+v", got)
	}

	results := messages[2]
	if results.Role != "user" {
		t.Fatalf("tool result message role = %q, want user", results.Role)
	}
	if len(results.Content) != 2 {
		t.Fatalf("got %d tool_result blocks, want 2", len(results.Content))
	}
	for i, block := range results.Content {
		if block.Type != "tool_result" || block.ToolUseID != "toolu_1" {
			t.Errorf("tool_result[%d] = %+v", i, block)
		}
	}
	if results.Content[0].Result != "contents" {
		t.Errorf("tool_result[0].content = %q, want %q", results.Content[0].Result, "contents")
	}
	if results.Content[1].Result != "(empty tool result)" {
		t.Errorf("tool_result[1].content = %q, want empty-result placeholder", results.Content[1].Result)
	}
}

func TestMapAnthropicToolChoice(t *testing.T) {
	if got := mapAnthropicToolChoice(ToolChoiceAuto); got == nil || got.Type != "auto" {
		t.Errorf("auto → %+v, want {auto}", got)
	}
	if got := mapAnthropicToolChoice(ToolChoiceRequired); got == nil || got.Type != "any" {
		t.Errorf("required → %+v, want {any}", got)
	}
	if got := mapAnthropicToolChoice(ToolChoiceNone); got != nil {
		t.Errorf("none → %+v, want nil (Anthropic has no none option)", got)
	}
	if got := mapAnthropicToolChoice(""); got != nil {
		t.Errorf("empty → %+v, want nil", got)
	}
}
