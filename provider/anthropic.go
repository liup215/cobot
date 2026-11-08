package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultAnthropicBaseURL is the official Anthropic API endpoint. A
	// proxy or gateway can be used instead by setting BaseURL.
	defaultAnthropicBaseURL = "https://api.anthropic.com"

	// anthropicMessagesPath is the Messages API endpoint appended to the
	// base URL.
	anthropicMessagesPath = "/v1/messages"

	// defaultAnthropicVersion is the API version sent in the
	// anthropic-version header.
	defaultAnthropicVersion = "2023-06-01"

	// defaultAnthropicModel is used when no model is configured.
	defaultAnthropicModel = "claude-sonnet-4-5"

	// defaultAnthropicMaxTokens is used when MaxTokens is not set. Anthropic
	// requires an explicit max_tokens on every request.
	defaultAnthropicMaxTokens = 4096

	// anthropicAPIKeyEnv is consulted when no explicit API key is set.
	anthropicAPIKeyEnv = "ANTHROPIC_API_KEY"
)

// buildAnthropicURL resolves the /v1/messages endpoint from a base URL.
// Accepted forms:
//
//	https://host              → https://host/v1/messages
//	https://host/v1           → https://host/v1/messages
//	https://host/v1/messages  → used as-is (full endpoint override)
func buildAnthropicURL(baseURL string) string {
	if strings.HasSuffix(baseURL, anthropicMessagesPath) {
		return baseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/messages"
	}
	return baseURL + anthropicMessagesPath
}

// AnthropicProvider implements the Provider interface for the native
// Anthropic Messages API (Claude models).
//
// Differences from the OpenAI-compatible provider:
//   - Authentication uses x-api-key plus anthropic-version (no Bearer
//     token), and the API key falls back to the ANTHROPIC_API_KEY env var.
//   - The system prompt travels in the top-level system field carrying an
//     ephemeral cache_control marker (prompt caching) instead of a
//     system-role message.
//   - Tool definitions use input_schema and tool results are returned as
//     tool_result content blocks grouped into user messages.
//   - Thinking blocks are delivered through the ReasoningContent channel,
//     matching the reasoning_content semantics of the OpenAI provider.
//
// Streaming: the connection is established synchronously (request
// marshaling, HTTP round trip, status check) so transport failures surface
// as an *HTTPError the agent loop can retry; the SSE stream is then parsed
// in a background goroutine. Mid-stream error events flow through the
// stream (chunk.Error) into the correction loop and are never retried.
type AnthropicProvider struct {
	apiKey     string
	model      string
	baseURL    string
	version    string
	httpClient *http.Client

	// headers / dynamicHeaders mirror the OpenAI provider: static config
	// headers applied last (they win), consulted-per-request values for
	// gateway quirks like session ids.
	headers        map[string]string
	dynamicHeaders func() map[string]string
}

// NewAnthropicProvider creates a new Anthropic Messages provider.
// Parameters:
//   - apiKey: API key for authentication; when empty the ANTHROPIC_API_KEY
//     environment variable is used
//   - model: Model name (e.g. "claude-sonnet-4-5")
//   - baseURL: Custom base URL (optional, defaults to the Anthropic API)
func NewAnthropicProvider(apiKey, model, baseURL string) *AnthropicProvider {
	if apiKey == "" {
		apiKey = os.Getenv(anthropicAPIKeyEnv)
	}
	if model == "" {
		model = defaultAnthropicModel
	}
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}

	return &AnthropicProvider{
		apiKey:  apiKey,
		model:   model,
		baseURL: baseURL,
		version: defaultAnthropicVersion,
		httpClient: &http.Client{
			// Leave the global timeout at 0 for long-running streaming
			// sessions; safety comes from transport timeouts plus the
			// per-read idle timeout inside the SSE loop.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
	}
}

// GetModel returns the current model name.
func (p *AnthropicProvider) GetModel() string { return p.model }

// NewAnthropicProviderFromConfig builds an Anthropic provider from the shared
// ProviderConfig (headers and dynamic headers honored; ContentTransform is
// OpenAI-wire-specific and not applied here).
func NewAnthropicProviderFromConfig(config ProviderConfig) *AnthropicProvider {
	p := NewAnthropicProvider(config.APIKey, config.Model, config.BaseURL)
	p.headers = config.Headers
	p.dynamicHeaders = config.DynamicHeaders
	return p
}

// GetProviderName returns the provider name.
func (p *AnthropicProvider) GetProviderName() string { return "anthropic" }

// SupportsTools returns true: all Claude 3+ models support native tool use.
func (p *AnthropicProvider) SupportsTools() bool { return true }

// GetBaseURL returns the current base URL.
func (p *AnthropicProvider) GetBaseURL() string { return p.baseURL }

// GetAPIVersion returns the anthropic-version header value.
func (p *AnthropicProvider) GetAPIVersion() string { return p.version }

// SetHTTPClient sets a custom HTTP client (useful for testing).
func (p *AnthropicProvider) SetHTTPClient(client *http.Client) { p.httpClient = client }

// newAnthropicRequest converts the internal request into Anthropic wire
// format. stream selects streaming mode.
func (p *AnthropicProvider) newAnthropicRequest(req *MessageRequest, stream bool) *anthropicRequest {
	anthReq := &anthropicRequest{
		Model:       p.model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if anthReq.MaxTokens <= 0 {
		anthReq.MaxTokens = defaultAnthropicMaxTokens
	}

	// The explicit system prompt comes first, followed by any system-role
	// messages recorded in the conversation history (Anthropic rejects
	// system-role messages inside `messages`).
	system, messages := convertMessagesToAnthropic(req.Messages)
	if req.SystemPrompt != "" {
		system = append([]anthropicBlock{{Type: "text", Text: req.SystemPrompt}}, system...)
	}
	// Anthropic caches the longest prefix ending at a cache_control marker,
	// so the ephemeral marker goes on the LAST system block: the whole
	// system segment is cached as one prefix.
	if len(system) > 0 {
		system[len(system)-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
	}
	anthReq.System = system
	anthReq.Messages = messages

	if len(req.Tools) > 0 {
		tools := make([]anthropicTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.InputSchema
			if len(schema) == 0 {
				// input_schema is mandatory; fall back to an object with no
				// properties so a tool without a schema stays callable.
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, anthropicTool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			})
		}
		anthReq.Tools = tools
		if choice := mapAnthropicToolChoice(req.ToolChoice); choice != nil {
			anthReq.ToolChoice = choice
		}
	}

	return anthReq
}

// post sends the request body. Only the HTTP round trip happens here; the
// status check stays with the caller so each entry point can build the
// right error shape.
func (p *AnthropicProvider) post(ctx context.Context, body []byte) (*http.Response, error) {
	fullURL := buildAnthropicURL(p.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", p.version)
	// Per-request dynamic headers first, then static config headers
	// (config wins on key conflicts) — same order as the OpenAI provider.
	if p.dynamicHeaders != nil {
		for k, v := range p.dynamicHeaders() {
			if k != "" && v != "" {
				httpReq.Header.Set(k, v)
			}
		}
	}
	for k, v := range p.headers {
		if k != "" {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		// Network-level failure: retryable by the agent loop retry policy.
		return nil, &HTTPError{Status: 0, Message: fmt.Sprintf("failed to send request: %v", err), Err: err}
	}
	return resp, nil
}

// checkAnthropicStatus converts a non-200 connection into an *HTTPError,
// parsing the Anthropic error envelope
// ({"type":"error","error":{"type","message"}}) and the Retry-After header
// (rate limiting / overloaded_error / 529).
func checkAnthropicStatus(resp *http.Response, body []byte) *HTTPError {
	msg := fmt.Sprintf("Anthropic API returned status %d: %s", resp.StatusCode, string(body))
	var errResp anthropicErrorResponse
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error != nil && errResp.Error.Message != "" {
		msg = fmt.Sprintf("Anthropic API error: %s (type: %s)", errResp.Error.Message, errResp.Error.Type)
	}
	return &HTTPError{
		Status:     resp.StatusCode,
		Message:    msg,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// CreateMessage sends a message to the Anthropic Messages API and returns
// the complete (non-streaming) response.
func (p *AnthropicProvider) CreateMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	body, err := json.Marshal(p.newAnthropicRequest(req, false))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	logInfof("CreateMessage: Sending request to %s (tools=%d)", buildAnthropicURL(p.baseURL), len(req.Tools))
	logDebugf("CreateMessage request body: %s", string(body))

	resp, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, checkAnthropicStatus(resp, raw)
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(raw, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return convertAnthropicResponse(&apiResp), nil
}

// CreateMessageStream sends a message to the Anthropic Messages API and
// returns a stream of chunks.
func (p *AnthropicProvider) CreateMessageStream(ctx context.Context, req *MessageRequest) (<-chan StreamChunk, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("API key is required")
	}

	body, err := json.Marshal(p.newAnthropicRequest(req, true))
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	logInfof("CreateMessageStream: Sending request to %s (tools=%d, tool_choice=%v)", buildAnthropicURL(p.baseURL), len(req.Tools), req.ToolChoice)
	logDebugf("CreateMessageStream request body: %s", string(body))

	resp, err := p.post(ctx, body)
	if err != nil {
		return nil, err
	}

	// Check status code synchronously so the caller can retry.
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, checkAnthropicStatus(resp, raw)
	}

	// From here on the connection is established: stream SSE in a goroutine.
	chunkChan := make(chan StreamChunk)
	go func() {
		defer close(chunkChan)
		defer resp.Body.Close()
		p.pumpStream(resp, chunkChan)
	}()
	return chunkChan, nil
}

// pumpStream parses the Anthropic SSE stream into StreamChunks.
//
// Event order (Anthropic streaming protocol):
//
//	message_start       → captures input-token usage
//	content_block_start → opens a text / thinking / tool_use block
//	content_block_delta → text_delta | thinking_delta | input_json_delta
//	content_block_stop  → closes the block; tool_use input is parsed here
//	message_delta       → carries the stop_reason and cumulative usage
//	message_stop        → final chunk (FinishReason + Usage + Done)
//	ping                → ignored
//	error               → stream error chunk (correction loop, no retry)
//
// Events are dispatched by the `type` field of the data payload (always
// present), falling back to the SSE `event:` line, so proxies that strip
// event lines still parse correctly.
func (p *AnthropicProvider) pumpStream(resp *http.Response, chunkChan chan<- StreamChunk) {
	const sseIdleTimeout = 120 * time.Second
	idleTimer := time.NewTimer(sseIdleTimeout)
	defer idleTimer.Stop()

	reader := bufio.NewReader(resp.Body)

	var (
		eventName string
		dataLines []string
		// blocks holds partially assembled content blocks keyed by index.
		blocks = make(map[int]*anthropicBlockState)
		// usage accumulators fed by message_start / message_delta.
		usageIn, usageOut int
		stopReason        string
		terminated        bool // stream completed or errored: stop reading
	)

	// finish emits the final chunk, mirroring the OpenAI provider's single
	// final chunk that carries FinishReason + Usage + Done.
	finish := func() {
		chunkChan <- StreamChunk{
			FinishReason: mapAnthropicStopReason(stopReason),
			Usage: TokenUsage{
				InputTokens:  usageIn,
				OutputTokens: usageOut,
				TotalTokens:  usageIn + usageOut,
			},
			Done: true,
		}
		terminated = true
	}

	// finalizeToolCall validates the incrementally assembled input and emits
	// the complete ToolCall for a tool_use block.
	finalizeToolCall := func(st *anthropicBlockState) {
		if st.ID == "" || st.Name == "" {
			logErrorf("Anthropic tool call missing id/name, skipping: id=%q name=%q", st.ID, st.Name)
			return
		}
		input := strings.TrimSpace(st.Input.String())
		if input == "" {
			// Zero-argument tools may arrive with the input object inside
			// content_block_start and no deltas at all.
			if init := strings.TrimSpace(string(st.InitialInput)); init != "" && init != "{}" {
				input = init
			} else {
				input = "{}"
			}
		}
		if !isValidJSON(input) {
			// Same brace-repair treatment as the OpenAI provider.
			fixed := input
			for !isCompleteJSONObject(fixed) && len(fixed) < len(input)+10 {
				fixed += "}"
			}
			if !isValidJSON(fixed) {
				logErrorf("Anthropic tool call has invalid JSON arguments, skipping: id=%s name=%s input=%s", st.ID, st.Name, input)
				return
			}
			logDebugf("Fixed incomplete JSON for Anthropic tool call: id=%s, original=%s, fixed=%s", st.ID, input, fixed)
			input = fixed
		}
		chunkChan <- StreamChunk{ToolCall: &ToolCall{ID: st.ID, Name: st.Name, Input: input}}
	}

	// closeBlock finalizes the block at the given index.
	closeBlock := func(index int) {
		st := blocks[index]
		if st == nil {
			return
		}
		st.Closed = true
		if st.Type == "tool_use" {
			finalizeToolCall(st)
		}
		// text/thinking blocks need no finalization: their deltas were
		// already streamed out as they arrived.
	}

	// handleEvent processes one decoded SSE event.
	handleEvent := func(evType string, ev *anthropicStreamEvent) {
		switch evType {
		case "message_start":
			if ev.Message != nil {
				usageIn = ev.Message.Usage.InputTokens
				usageOut = ev.Message.Usage.OutputTokens
			}

		case "content_block_start":
			if ev.ContentBlock == nil {
				return
			}
			blocks[ev.Index] = &anthropicBlockState{
				Type:         ev.ContentBlock.Type,
				ID:           ev.ContentBlock.ID,
				Name:         ev.ContentBlock.Name,
				InitialInput: ev.ContentBlock.Input,
			}

		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil {
				// Deltas for a block we never saw started: recover so text
				// is not lost.
				st = &anthropicBlockState{}
				blocks[ev.Index] = st
			}
			if ev.Delta == nil {
				return
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					chunkChan <- StreamChunk{Content: ev.Delta.Text}
				}
			case "thinking_delta":
				// Thinking flows through the dedicated reasoning channel,
				// never mixed into Content (gline convention).
				if ev.Delta.Thinking != "" {
					chunkChan <- StreamChunk{ReasoningContent: ev.Delta.Thinking}
				}
			case "signature_delta":
				// The signature authenticates a thinking block when echoed
				// back in later turns. agentcore does not round-trip thinking
				// (see convertMessagesToAnthropic), so it is ignored here.
			case "input_json_delta":
				st.Input.WriteString(ev.Delta.PartialJSON)
				// Partial update for real-time UI display. A copy, like the
				// OpenAI provider, so processStream never double-accumulates.
				if st.ID != "" || st.Name != "" {
					tc := ToolCall{ID: st.ID, Name: st.Name, Input: st.Input.String()}
					chunkChan <- StreamChunk{ToolCall: &tc, IsPartial: true}
				}
			default:
				logDebugf("Ignoring unsupported content_block_delta type %q", ev.Delta.Type)
			}

		case "content_block_stop":
			closeBlock(ev.Index)

		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				if ev.Usage.OutputTokens > 0 {
					usageOut = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 {
					usageIn = ev.Usage.InputTokens
				}
			}

		case "message_stop":
			finish()

		case "ping":
			// Keep-alive; nothing to do.

		case "error":
			// Mid-stream error: surfaced through the stream so the agent
			// loop's correction path handles it (never retried here).
			if ev.Error != nil && ev.Error.Message != "" {
				chunkChan <- StreamChunk{
					Error: fmt.Errorf("Anthropic stream error: %s (type: %s)", ev.Error.Message, ev.Error.Type),
					Done:  true,
				}
			} else {
				chunkChan <- StreamChunk{
					Error: fmt.Errorf("Anthropic stream error event"),
					Done:  true,
				}
			}
			terminated = true

		default:
			logDebugf("Ignoring unknown Anthropic SSE event type %q", evType)
		}
	}

	// dispatch decodes and handles one complete SSE event.
	dispatch := func() {
		if len(dataLines) == 0 && eventName == "" {
			return
		}
		data := strings.Join(dataLines, "\n")
		eventType := eventName
		eventName = ""
		dataLines = nil

		if strings.TrimSpace(data) == "" {
			return
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			logDebugf("Failed to parse Anthropic SSE data: %s, error: %v", data, err)
			return
		}
		// The payload's type field wins over the SSE event line.
		if ev.Type != "" {
			eventType = ev.Type
		}
		handleEvent(eventType, &ev)
	}

	// handleLine consumes one raw SSE line (already newline-trimmed).
	handleLine := func(line string) {
		switch {
		case line == "", strings.HasPrefix(line, ":"):
			// Blank separators and SSE comments.
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		default:
			// Other SSE fields (id:, retry:) are irrelevant here.
		}
	}

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
		if rc, ok := resp.Body.(interface{ SetReadDeadline(t time.Time) error }); ok {
			_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		}

		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			logDebugf("Error reading stream: %v", err)
			chunkChan <- StreamChunk{Error: fmt.Errorf("error reading stream: %w", err), Done: true}
			return
		}

		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			idleTimer.Reset(sseIdleTimeout)
		}
		handleLine(trimmed)
		if trimmed == "" {
			// A blank line terminates the current event.
			dispatch()
		}
		if terminated {
			return
		}
		if err == io.EOF {
			break
		}
	}

	// Flush a trailing event that lacked its terminating blank line, then
	// best-effort finalize tool_use blocks the server never closed.
	dispatch()
	finalizeUnclosedToolBlocks(blocks, finalizeToolCall)
	if !terminated {
		finish()
	}
}

// finalizeUnclosedToolBlocks finalizes tool_use blocks still open when the
// stream ended (truncated-stream best-effort recovery), in index order.
func finalizeUnclosedToolBlocks(blocks map[int]*anthropicBlockState, finalize func(*anthropicBlockState)) {
	indexes := make([]int, 0, len(blocks))
	for idx := range blocks {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)
	for _, idx := range indexes {
		if st := blocks[idx]; st != nil && !st.Closed && st.Type == "tool_use" {
			finalize(st)
		}
	}
}

// convertAnthropicResponse maps a non-streaming Messages response onto the
// internal response shape.
func convertAnthropicResponse(r *anthropicResponse) *MessageResponse {
	out := &MessageResponse{
		FinishReason: mapAnthropicStopReason(r.StopReason),
		Usage: TokenUsage{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			TotalTokens:  r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	}

	var content bytes.Buffer
	for i := range r.Content {
		block := &r.Content[i]
		switch block.Type {
		case "text":
			content.WriteString(block.Text)
		case "thinking":
			if block.Thinking != "" {
				if out.ReasoningContent != "" {
					out.ReasoningContent += "\n"
				}
				out.ReasoningContent += block.Thinking
			}
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:    block.ID,
				Name:  block.Name,
				Input: string(normalizeAnthropicToolInput(string(block.Input))),
			})
		}
	}
	out.Content = content.String()
	return out
}

// mapAnthropicStopReason maps Anthropic stop_reason values onto the
// OpenAI-style finish reasons used across agentcore:
//
//	end_turn      → stop        (turn finished naturally)
//	stop_sequence → stop        (a stop sequence matched)
//	tool_use      → tool_calls  (the loop must execute tools)
//	max_tokens    → length      (output truncated)
//
// Unknown values pass through unchanged; an absent stop_reason (a message
// that completed without one) is treated as a natural end.
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "", "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return reason
	}
}

// mapAnthropicToolChoice maps the shared ToolChoice onto Anthropic's
// tool_choice object. Anthropic has no "none" option, so none/unknown
// choices omit the field entirely.
func mapAnthropicToolChoice(choice ToolChoiceOption) *anthropicToolChoice {
	switch choice {
	case ToolChoiceAuto:
		return &anthropicToolChoice{Type: "auto"}
	case ToolChoiceRequired:
		// "any" makes the model use some tool (Anthropic's equivalent of
		// OpenAI's "required").
		return &anthropicToolChoice{Type: "any"}
	default:
		return nil
	}
}
