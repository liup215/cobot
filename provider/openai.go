// Package provider provides LLM provider implementations
package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/liup215/cobot/types"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	chatCompletionsPath  = "/chat/completions"
)

// buildFullURL constructs the full API URL from base URL
// If baseURL already ends with /chat/completions, use it as-is
// Otherwise, append the chat completions path
func buildFullURL(baseURL string) string {
	if strings.HasSuffix(baseURL, chatCompletionsPath) {
		return baseURL
	}
	// Remove trailing slash if present to avoid double slashes
	baseURL = strings.TrimSuffix(baseURL, "/")
	return baseURL + chatCompletionsPath
}

// isValidJSON checks if a string is valid JSON
func isValidJSON(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// Try to parse as generic JSON
	var js interface{}
	if err := json.Unmarshal([]byte(s), &js); err != nil {
		return false
	}
	return true
}

// isCompleteJSONObject checks if the string looks like a complete JSON object
// This is a heuristic check for streaming JSON fragments
func isCompleteJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return false
	}
	// Check if it starts with { and ends with }
	if s[0] != '{' || s[len(s)-1] != '}' {
		return false
	}
	// Additional check: count braces
	openCount := 0
	closeCount := 0
	inString := false
	escaped := false
	for _, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if !inString {
			if r == '{' {
				openCount++
			} else if r == '}' {
				closeCount++
			}
		}
	}
	return openCount > 0 && openCount == closeCount
}

// OpenAIProvider implements the Provider interface for OpenAI-compatible APIs
// This provider can be used with:
// - OpenAI official API
// - OpenRouter
// - Local models (Ollama, LM Studio, etc.)
// - Any OpenAI-compatible endpoint
type OpenAIProvider struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client

	// headers are static extra headers applied to every request (config
	// headers, User-Agent overrides). Applied last so they win over
	// dynamicHeaders on key conflicts.
	headers map[string]string

	// dynamicHeaders is consulted per request for gateway values that
	// rotate with conversation state (e.g. x-opencode-session).
	dynamicHeaders func() map[string]string

	// contentTransform lets the host convert stored marker text (the read
	// tool's [image: path] markers) into multimodal content parts.
	// nil = message content is sent verbatim.
	contentTransform func(content, role string) interface{}

	// defaultMaxTokens is the output cap applied when a request carries
	// none (0 = 4096).
	defaultMaxTokens int

	// noStreamUsage opts out of stream_options.include_usage for gateways
	// that reject the field.
	noStreamUsage bool
}

// OpenAIMessage represents a message in OpenAI's format.
//
// Content is interface{} because requests may carry multimodal content
// parts (host content transform) while responses normally carry a string
// — decode with contentText when a plain string is needed.
type OpenAIMessage struct {
	Role             string           `json:"role"`
	Content          interface{}      `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
}

// OpenAIToolCall represents a tool call in OpenAI's format
type OpenAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

// OpenAIFunction represents a function call in OpenAI's format
type OpenAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAITool represents a tool definition in OpenAI's format
type OpenAITool struct {
	Type     string             `json:"type"`
	Function OpenAIToolFunction `json:"function"`
}

// OpenAIToolFunction represents the function definition
type OpenAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// OpenAIRequest represents the request body for OpenAI API
type OpenAIRequest struct {
	Model         string               `json:"model"`
	Messages      []OpenAIMessage      `json:"messages"`
	Tools         []OpenAITool         `json:"tools,omitempty"`
	ToolChoice    interface{}          `json:"tool_choice,omitempty"`
	Temperature   float64              `json:"temperature,omitempty"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
}

// OpenAIStreamOptions carries streaming request options. IncludeUsage asks
// the server to report token usage on a final chunk with empty choices —
// without it, strict OpenAI-compatible endpoints never report usage and the
// loop falls back to token estimation.
type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// OpenAIResponse represents the response from OpenAI API
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

// OpenAIStreamResponse represents a streaming response chunk from OpenAI API
type OpenAIStreamResponse struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   OpenAIUsage          `json:"usage,omitempty"`
}

// OpenAIStreamChoice represents a choice in the streaming response
type OpenAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        OpenAIStreamDelta `json:"delta"`
	FinishReason string            `json:"finish_reason"`
}

// OpenAIStreamDelta represents the delta in a streaming response
type OpenAIStreamDelta struct {
	Role             string                 `json:"role,omitempty"`
	Content          string                 `json:"content,omitempty"`
	ReasoningContent string                 `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}

// OpenAIStreamToolCall represents a tool call in a streaming response
type OpenAIStreamToolCall struct {
	Index    int                  `json:"index"`
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type,omitempty"`
	Function OpenAIStreamFunction `json:"function,omitempty"`
}

// OpenAIStreamFunction represents a function call in a streaming response
type OpenAIStreamFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// OpenAIChoice represents a choice in the response
type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// OpenAIUsage represents token usage
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAIError represents an error response
type OpenAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// NewOpenAIProvider creates a new OpenAI-compatible provider
// Parameters:
//   - apiKey: API key for authentication
//   - model: Model name (e.g., "gpt-4", "gpt-3.5-turbo")
//   - baseURL: Custom base URL (optional, defaults to OpenAI official API)
func NewOpenAIProvider(apiKey, model, baseURL string) *OpenAIProvider {
	if model == "" {
		model = "gpt-4"
	}

	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}

	return &OpenAIProvider{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		httpClient: &http.Client{
			// Leave global timeout at 0 for long-running streaming sessions.
			// Safety comes from Transport timeouts + per-read idle timeout
			// inside the SSE loop.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// NewOpenAIProviderFromConfig builds an OpenAI-compatible provider from the
// shared ProviderConfig, including host hooks (headers, dynamic headers,
// content transform) and defaults.
func NewOpenAIProviderFromConfig(config ProviderConfig) *OpenAIProvider {
	p := NewOpenAIProvider(config.APIKey, config.Model, config.BaseURL)
	p.headers = config.Headers
	p.dynamicHeaders = config.DynamicHeaders
	p.contentTransform = config.ContentTransform
	p.defaultMaxTokens = config.MaxTokens
	p.noStreamUsage = config.NoStreamUsage
	return p
}

// contentText renders a decoded OpenAI "content" value as plain text.
// Servers normally return a string; array-of-parts content (vision) is
// flattened to its text parts.
func contentText(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []interface{}:
		var b strings.Builder
		for _, part := range t {
			if m, ok := part.(map[string]interface{}); ok {
				if txt, ok := m["text"].(string); ok {
					b.WriteString(txt)
					b.WriteByte('\n')
				}
			}
		}
		return b.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}

// contentFor applies the host content transform (marker → parts) for a
// message; without a transform the stored string goes on the wire as-is.
func (p *OpenAIProvider) contentFor(content, role string) interface{} {
	if p.contentTransform == nil {
		return content
	}
	return p.contentTransform(content, role)
}

// applyHeaders sets the standard headers, then dynamic per-request headers,
// then static config headers (config wins on key conflicts).
func (p *OpenAIProvider) applyHeaders(req *http.Request, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if p.dynamicHeaders != nil {
		for k, v := range p.dynamicHeaders() {
			if k != "" && v != "" {
				req.Header.Set(k, v)
			}
		}
	}
	for k, v := range p.headers {
		if k != "" {
			req.Header.Set(k, v)
		}
	}
}

// outputCap resolves the request output cap: request value, then the
// provider default, then 4096.
func (p *OpenAIProvider) outputCap(reqCap int) int {
	if reqCap > 0 {
		return reqCap
	}
	if p.defaultMaxTokens > 0 {
		return p.defaultMaxTokens
	}
	return 4096
}

// buildMessages converts loop messages into OpenAI wire messages. The host
// content transform runs per message (markers → multimodal parts),
// reasoning_content rides only on assistant messages from the current turn
// (DeepSeek requirement), and the system prompt is prepended when present.
// Shared by the streaming and non-streaming request paths.
func (p *OpenAIProvider) buildMessages(req *MessageRequest) []OpenAIMessage {
	openaiMessages := make([]OpenAIMessage, 0, len(req.Messages))

	// Determine last user index so we only attach reasoning_content to assistant messages from the current turn.
	lastUserIndex := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == types.RoleUser {
			lastUserIndex = i
			break
		}
	}

	for i, msg := range req.Messages {
		role := string(msg.Role)
		switch msg.Role {
		case types.RoleSystem:
			openaiMessages = append(openaiMessages, OpenAIMessage{
				Role:    "system",
				Content: p.contentFor(msg.Content, role),
			})
		case types.RoleUser:
			openaiMessages = append(openaiMessages, OpenAIMessage{
				Role:    "user",
				Content: p.contentFor(msg.Content, role),
			})
		case types.RoleAssistant:
			// Include tool calls if present - required by OpenAI API
			openaiMsg := OpenAIMessage{
				Role:    "assistant",
				Content: p.contentFor(msg.Content, role),
			}
			// Attach reasoning_content only for assistant messages that belong to the current turn
			// i.e., assistant messages that come after the last user message in the history.
			if i > lastUserIndex && msg.ReasoningContent != "" {
				openaiMsg.ReasoningContent = msg.ReasoningContent
			}
			if len(msg.ToolCalls) > 0 {
				openaiMsg.ToolCalls = convertToolCallsToOpenAI(msg.ToolCalls)
			}
			openaiMessages = append(openaiMessages, openaiMsg)
		case types.RoleTool:
			// Tool results are sent as tool messages
			openaiMessages = append(openaiMessages, OpenAIMessage{
				Role:       "tool",
				Content:    p.contentFor(msg.Content, role),
				ToolCallID: msg.ToolCallID,
			})
		}
	}

	// Add system prompt if provided (verbatim: host transforms target
	// conversation markers, not the system prompt).
	if req.SystemPrompt != "" {
		systemMsg := OpenAIMessage{
			Role:    "system",
			Content: req.SystemPrompt,
		}
		openaiMessages = append([]OpenAIMessage{systemMsg}, openaiMessages...)
	}
	return openaiMessages
}

// buildTools converts loop tool definitions into OpenAI function tools.
func buildTools(req *MessageRequest) []OpenAITool {
	openaiTools := make([]OpenAITool, 0, len(req.Tools))
	for _, tool := range req.Tools {
		openaiTools = append(openaiTools, OpenAITool{
			Type: "function",
			Function: OpenAIToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}
	return openaiTools
}

// CreateMessage sends a message to the OpenAI-compatible API
func (p *OpenAIProvider) CreateMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	// Convert messages to OpenAI format (marker → parts transform and the
	// turn-scoped reasoning_content rule live in the shared helper).
	openaiMessages := p.buildMessages(req)
	openaiTools := buildTools(req)

	// Build request
	openaiReq := OpenAIRequest{
		Model:       p.model,
		Messages:    openaiMessages,
		Temperature: req.Temperature,
		MaxTokens:   p.outputCap(req.MaxTokens),
	}

	// Only add tools if there are any
	if len(openaiTools) > 0 {
		openaiReq.Tools = openaiTools
		// Force tool use whenever tools are provided and the agent loop signals work is pending.
		if req.ToolChoice == ToolChoiceRequired {
			openaiReq.ToolChoice = "required"
		}
	}

	// Set defaults
	if openaiReq.MaxTokens == 0 {
		openaiReq.MaxTokens = 4096
	}
	if openaiReq.Temperature == 0 {
		openaiReq.Temperature = 0.0
	}

	// Serialize request
	jsonBody, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build full URL by appending the chat completions path
	fullURL := buildFullURL(p.baseURL)
	logInfof("CreateMessage: Sending request to %s (tools=%d, tool_choice=%v)", fullURL, len(openaiReq.Tools), openaiReq.ToolChoice)
	logDebugf("CreateMessage request body: %s", string(jsonBody))

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	p.applyHeaders(httpReq, false)

	// Send request
	logDebugf("CreateMessage: Sending HTTP request...")
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	logDebugf("CreateMessage: Received response status: %d", resp.StatusCode)

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		var openaiErr OpenAIError
		if err := json.Unmarshal(body, &openaiErr); err == nil && openaiErr.Error.Message != "" {
			return nil, fmt.Errorf("OpenAI API error: %s (type: %s, code: %s)",
				openaiErr.Error.Message, openaiErr.Error.Type, openaiErr.Error.Code)
		}
		return nil, fmt.Errorf("OpenAI API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var openaiResp OpenAIResponse
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	// Convert to agent response
	return p.convertResponse(&openaiResp), nil
}

// convertResponse converts OpenAI response to agent response
func (p *OpenAIProvider) convertResponse(resp *OpenAIResponse) *MessageResponse {
	if len(resp.Choices) == 0 {
		return &MessageResponse{
			Content:      "",
			FinishReason: "error",
			Usage: TokenUsage{
				InputTokens:  resp.Usage.PromptTokens,
				OutputTokens: resp.Usage.CompletionTokens,
				TotalTokens:  resp.Usage.TotalTokens,
			},
		}
	}

	choice := resp.Choices[0]
	result := &MessageResponse{
		Content:          contentText(choice.Message.Content),
		ReasoningContent: choice.Message.ReasoningContent,
		FinishReason:     choice.FinishReason,
		Usage: TokenUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		},
	}

	// Convert tool calls
	if len(choice.Message.ToolCalls) > 0 {
		for _, tc := range choice.Message.ToolCalls {
			toolCall := ToolCall{
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: tc.Function.Arguments,
			}
			result.ToolCalls = append(result.ToolCalls, toolCall)
		}
	}

	return result
}

// SupportsTools returns true if the provider supports tool calling
func (p *OpenAIProvider) SupportsTools() bool {
	// Most modern OpenAI models and compatible models support tools
	// Models that don't support tools: gpt-3.5-turbo-instruct, text-* models
	nonToolModels := []string{
		"gpt-3.5-turbo-instruct",
		"text-davinci",
		"text-curie",
		"text-babbage",
		"text-ada",
	}

	for _, prefix := range nonToolModels {
		if len(p.model) >= len(prefix) && p.model[:len(prefix)] == prefix {
			return false
		}
	}
	return true
}

// GetModel returns the current model name
func (p *OpenAIProvider) GetModel() string {
	return p.model
}

// GetProviderName returns the provider name
func (p *OpenAIProvider) GetProviderName() string {
	return "openai"
}

// SetHTTPClient sets a custom HTTP client (useful for testing)
func (p *OpenAIProvider) SetHTTPClient(client *http.Client) {
	p.httpClient = client
}

// GetBaseURL returns the current base URL
func (p *OpenAIProvider) GetBaseURL() string {
	return p.baseURL
}

// CreateMessageStream sends a message to the OpenAI-compatible API and returns a stream of responses
func (p *OpenAIProvider) CreateMessageStream(ctx context.Context, req *MessageRequest) (<-chan StreamChunk, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	// Convert messages + tools (shared helpers; host content transform and
	// reasoning rules included).
	openaiMessages := p.buildMessages(req)
	openaiTools := buildTools(req)

	// Build request
	openaiReq := OpenAIRequest{
		Model:       p.model,
		Messages:    openaiMessages,
		Temperature: req.Temperature,
		MaxTokens:   p.outputCap(req.MaxTokens),
		Stream:      true,
	}
	// Ask for real token usage on a final empty-choices chunk; opt-out for
	// gateways that reject the stream_options field.
	if !p.noStreamUsage {
		openaiReq.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	// Only add tools if there are any
	if len(openaiTools) > 0 {
		openaiReq.Tools = openaiTools
		// Force tool use whenever tools are provided and the agent loop signals work is pending.
		if req.ToolChoice == ToolChoiceRequired {
			openaiReq.ToolChoice = "required"
		}
	}

	// Set defaults
	if openaiReq.MaxTokens == 0 {
		openaiReq.MaxTokens = 4096
	}
	if openaiReq.Temperature == 0 {
		openaiReq.Temperature = 0.0
	}

	// Serialize request
	jsonBody, err := json.Marshal(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build full URL by appending the chat completions path
	fullURL := buildFullURL(p.baseURL)
	logInfof("CreateMessageStream: Sending request to %s (tools=%d, tool_choice=%v)", fullURL, len(openaiReq.Tools), openaiReq.ToolChoice)
	logDebugf("CreateMessageStream request system prompt length=%d body=%d", len(contentText(openaiReq.Messages[0].Content)), len(jsonBody))

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	p.applyHeaders(httpReq, true)

	// Send request
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Network-level failure: retryable by the agent loop retry policy.
		return nil, &HTTPError{Status: 0, Message: fmt.Sprintf("failed to send request: %v", err), Err: err}
	}

	// Check status code (synchronous so the caller can retry)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var openaiErr OpenAIError
		msg := fmt.Sprintf("OpenAI API returned status %d: %s", resp.StatusCode, string(body))
		if err := json.Unmarshal(body, &openaiErr); err == nil && openaiErr.Error.Message != "" {
			msg = fmt.Sprintf("OpenAI API error: %s (type: %s, code: %s)",
				openaiErr.Error.Message, openaiErr.Error.Type, openaiErr.Error.Code)
		}
		return nil, &HTTPError{
			Status:     resp.StatusCode,
			Message:    msg,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	// From here on the connection is established: stream SSE in a goroutine.

	chunkChan := make(chan StreamChunk)

	go func() {
		defer close(chunkChan)
		defer resp.Body.Close()

		// Parse SSE stream
		reader := bufio.NewReader(resp.Body)
		toolCalls := make(map[int]*ToolCall)

		logDebugf("Starting SSE stream parsing from %s", fullURL)

		const sseIdleTimeout = 120 * time.Second
		idleTimer := time.NewTimer(sseIdleTimeout)
		defer idleTimer.Stop()

		for {
			select {
			case <-idleTimer.C:
				chunkChan <- StreamChunk{
					Error: fmt.Errorf("stream idle timeout after %v of inactivity", sseIdleTimeout),
					Done:  true,
				}
				return
			default:
			}

			// Set a per-read deadline so ReadString cannot block forever.
			// This is a best-effort; resp.Body is an http.bodyReadCloser which
			// wraps a net.Conn that supports SetReadDeadline.
			if rc, ok := resp.Body.(interface{ SetReadDeadline(t time.Time) error }); ok {
				_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
			}

			line, err := reader.ReadString('\n')
			if err == nil {
				idleTimer.Reset(sseIdleTimeout)
			} else {
				if err != io.EOF {
					logDebugf("Error reading stream: %v", err)
					chunkChan <- StreamChunk{Error: fmt.Errorf("error reading stream: %w", err), Done: true}
				} else {
					logDebug("Stream ended with EOF")
				}
				break
			}

			line = strings.TrimSpace(line)
			logDebugf("SSE line received: %s", line)

			if line == "" {
				continue
			}

			// Handle SSE format: lines starting with "data: "
			if !strings.HasPrefix(line, "data: ") {
				logDebugf("Skipping non-data line: %s", line)
				continue
			}

			// Extract JSON data
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				logDebug("Received [DONE] signal")
				chunkChan <- StreamChunk{Done: true}
				break
			}

			// Parse stream response
			var streamResp OpenAIStreamResponse
			if err := json.Unmarshal([]byte(data), &streamResp); err != nil {
				logDebugf("Failed to parse SSE data: %s, error: %v", data, err)
				continue
			}

			logDebugf("Parsed stream response: ID=%s, Choices=%d", streamResp.ID, len(streamResp.Choices))

			// Usage rides on a final chunk — strict OpenAI gives it empty
			// choices (via stream_options.include_usage), other servers attach
			// it to the last contentful chunk. Emit whenever present; the loop
			// treats the latest report as the turn's usage.
			if streamResp.Usage.TotalTokens > 0 {
				chunkChan <- StreamChunk{
					Usage: TokenUsage{
						InputTokens:  streamResp.Usage.PromptTokens,
						OutputTokens: streamResp.Usage.CompletionTokens,
						TotalTokens:  streamResp.Usage.TotalTokens,
					},
				}
			}

			if len(streamResp.Choices) == 0 {
				continue
			}

			choice := streamResp.Choices[0]
			delta := choice.Delta

			logDebugf("SSE delta: content=%d reasoning=%d toolCalls=%d finish=%s",
				len(delta.Content), len(delta.ReasoningContent), len(delta.ToolCalls), choice.FinishReason)

			// Handle content
			if delta.Content != "" {
				chunkChan <- StreamChunk{
					Content: delta.Content,
				}
			}

			// Handle reasoning content (models like DeepSeek / Claude-style reasoning)
			if delta.ReasoningContent != "" {
				chunkChan <- StreamChunk{
					ReasoningContent: delta.ReasoningContent,
				}
			}

			// Handle tool calls - OpenAI streams tool calls incrementally
			// Each chunk may contain partial updates to the tool call
			for _, tc := range delta.ToolCalls {
				if tc.Index >= 0 {
					isNewToolCall := false
					if toolCalls[tc.Index] == nil {
						toolCalls[tc.Index] = &ToolCall{
							ID:   tc.ID,
							Name: tc.Function.Name,
						}
						isNewToolCall = tc.ID != "" || tc.Function.Name != ""
						logDebugf("New tool call started: index=%d, id=%s, name=%s", tc.Index, tc.ID, tc.Function.Name)
					} else {
						// Update tool call info if we receive it in later chunks
						if tc.ID != "" {
							toolCalls[tc.Index].ID = tc.ID
						}
						if tc.Function.Name != "" {
							toolCalls[tc.Index].Name = tc.Function.Name
							logDebugf("Tool call name updated: index=%d, name=%s", tc.Index, tc.Function.Name)
						}
					}

					// Accumulate arguments
					if tc.Function.Arguments != "" {
						toolCalls[tc.Index].Input += tc.Function.Arguments
						logDebugf("Tool call arguments accumulated: index=%d, args=%s", tc.Index, toolCalls[tc.Index].Input)
					}

					// Send partial update for real-time UI display
					// This allows the TUI to show "⏺ Running: tool_name" with partial args
					// Only send partial updates if we have a name (required for display)
					// IMPORTANT: Send a COPY of the tool call, not the pointer
					// This prevents double accumulation in processStream
					if isNewToolCall || tc.Function.Arguments != "" {
						toolCallCopy := *toolCalls[tc.Index]
						chunkChan <- StreamChunk{
							ToolCall:  &toolCallCopy,
							IsPartial: true,
						}
					}
				}
			}

			// Check finish reason
			if choice.FinishReason != "" {
				logDebugf("Stream finished with reason: %s", choice.FinishReason)

				// Send final complete tool calls - only if they have valid JSON arguments
				// NOTE: must iterate over map keys, not 0..len, because indices may be sparse.
				for i := range toolCalls {
					if tc := toolCalls[i]; tc != nil && tc.ID != "" && tc.Name != "" {
						// Validate that the accumulated arguments are valid JSON
						if tc.Input == "" {
							// Empty input is valid (will be treated as empty object)
							tc.Input = "{}"
							logDebugf("Tool call has empty input, using empty object: index=%d", i)
						} else if !isValidJSON(tc.Input) {
							// Try to fix incomplete JSON by adding closing braces
							fixedInput := tc.Input
							for !isCompleteJSONObject(fixedInput) && len(fixedInput) < len(tc.Input)+10 {
								fixedInput += "}"
							}

							if isValidJSON(fixedInput) {
								logDebugf("Fixed incomplete JSON for tool call: index=%d, original=%s, fixed=%s", i, tc.Input, fixedInput)
								tc.Input = fixedInput
							} else {
								// Log error and skip this tool call
								logErrorf("Tool call has invalid JSON arguments, skipping: index=%d, id=%s, name=%s, input=%s",
									i, tc.ID, tc.Name, tc.Input)
								continue
							}
						}

						logDebugf("Sending complete tool call: index=%d, id=%s, name=%s", i, tc.ID, tc.Name)
						chunkChan <- StreamChunk{
							ToolCall:  tc,
							IsPartial: false, // Mark as complete
						}
					}
				}

				// Send completion with usage info
				chunkChan <- StreamChunk{
					FinishReason: choice.FinishReason,
					Done:         true,
				}
				break
			}
		}
	}()

	return chunkChan, nil
}

// convertToolCallsToOpenAI converts internal tool calls to OpenAI format
func convertToolCallsToOpenAI(toolCalls []types.ToolCall) []OpenAIToolCall {
	result := make([]OpenAIToolCall, 0, len(toolCalls))
	for _, tc := range toolCalls {
		result = append(result, OpenAIToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: OpenAIFunction{
				Name:      tc.Name,
				Arguments: string(tc.Input),
			},
		})
	}
	return result
}
