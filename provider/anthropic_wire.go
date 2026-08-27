package provider

import (
	"encoding/json"
	"strings"

	"github.com/liup215/cobot/types"
)

// This file holds the Anthropic Messages wire types and the mapping between
// agentcore's internal message shape and Anthropic's request/response
// format. The transport and streaming logic live in anthropic.go.

// anthropicCacheControl is a prompt-caching breakpoint. Only the ephemeral
// type exists today.
type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// anthropicBlock is one content block. The Messages API uses one block shape
// per type (text / thinking / tool_use / tool_result); a single struct with
// omitempty fields keeps the mapping code small while emitting only the
// fields relevant to each block type.
type anthropicBlock struct {
	Type string `json:"type"` // text | thinking | tool_use | tool_result

	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	// Signature authenticates a thinking block when it is echoed back in a
	// later turn. It is parsed from responses but never sent (see the note
	// in convertMessagesToAnthropic).
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Result    string `json:"content,omitempty"`

	// cache_control marks a prompt-caching breakpoint (system blocks only).
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicMessage is one message in the Anthropic conversation. Roles are
// only "user" and "assistant"; tool results ride in user messages.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicTool is a tool definition; agentcore's InputSchema maps directly
// onto Anthropic's input_schema.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice controls tool selection: auto / any / tool.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// anthropicRequest is the POST /v1/messages body.
type anthropicRequest struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	Temperature float64              `json:"temperature"`
	System      []anthropicBlock     `json:"system,omitempty"`
	Messages    []anthropicMessage   `json:"messages"`
	Tools       []anthropicTool      `json:"tools,omitempty"`
	ToolChoice  *anthropicToolChoice `json:"tool_choice,omitempty"`
	Stream      bool                 `json:"stream"`
}

// anthropicUsage carries token counts. message_start reports input tokens,
// message_delta reports the cumulative output tokens.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthropicAPIError is the error object of both the HTTP error envelope and
// mid-stream error events.
type anthropicAPIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// anthropicErrorResponse is the non-2xx body:
// {"type":"error","error":{"type":"overloaded_error","message":"..."}}.
type anthropicErrorResponse struct {
	Type  string             `json:"type"`
	Error *anthropicAPIError `json:"error"`
}

// anthropicResponse is the non-streaming Messages response.
type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      anthropicUsage   `json:"usage"`
}

// anthropicMessageStart is the message payload inside message_start events.
type anthropicMessageStart struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Role    string           `json:"role"`
	Model   string           `json:"model"`
	Content []anthropicBlock `json:"content"`
	Usage   anthropicUsage   `json:"usage"`
}

// anthropicStreamDelta is the delta payload of content_block_delta and
// message_delta events. Which fields are populated depends on the delta
// type (text_delta / thinking_delta / signature_delta / input_json_delta),
// and message_delta populates StopReason instead.
type anthropicStreamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// anthropicStreamEvent is the union of all SSE event payloads. Only the
// fields relevant to the event's type are populated.
type anthropicStreamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	Message      *anthropicMessageStart `json:"message,omitempty"`
	ContentBlock *anthropicBlock        `json:"content_block,omitempty"`
	Delta        *anthropicStreamDelta  `json:"delta,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
	Error        *anthropicAPIError     `json:"error,omitempty"`
}

// anthropicBlockState accumulates a streaming content block until its
// content_block_stop event arrives.
type anthropicBlockState struct {
	Type string // text | thinking | redacted_thinking | tool_use
	ID   string
	Name string

	// Input accumulates input_json_delta fragments for tool_use blocks.
	Input strings.Builder

	// InitialInput holds the input object from content_block_start, used
	// when a tool call carries no deltas (zero-argument tools).
	InitialInput json.RawMessage

	// Closed is set once content_block_stop arrived.
	Closed bool
}

// convertMessagesToAnthropic maps the internal conversation onto Anthropic
// format. It returns the system blocks (system-role history content, which
// must travel in the top-level system field) and the user/assistant message
// list.
//
// Mapping rules:
//   - user messages become a single text block;
//   - assistant text becomes a text block and each tool call becomes a
//     tool_use block whose input is the parsed JSON object;
//   - tool results become tool_result blocks grouped into a single user
//     message (Anthropic requires tool_result blocks to live in user
//     messages, and consecutive results share one message);
//   - empty user messages are dropped (Anthropic rejects empty content);
//   - ReasoningContent (thinking) is intentionally NOT round-tripped: the
//     Messages API requires thinking blocks to be echoed back together with
//     their cryptographic signature, which the internal Message type does
//     not carry, and a thinking block is only valid when thinking is enabled
//     for the request. Dropping it is the only safe behavior (v0.2 candidate:
//     persist the signature alongside the reasoning text).
func convertMessagesToAnthropic(msgs []types.Message) (system []anthropicBlock, messages []anthropicMessage) {
	var pendingResults []anthropicBlock

	// flushToolResults commits queued tool_result blocks as one user message.
	flushToolResults := func() {
		if len(pendingResults) == 0 {
			return
		}
		messages = append(messages, anthropicMessage{Role: "user", Content: pendingResults})
		pendingResults = nil
	}

	for i := range msgs {
		msg := msgs[i]
		switch msg.Role {
		case types.RoleSystem:
			if msg.Content != "" {
				system = append(system, anthropicBlock{Type: "text", Text: msg.Content})
			}

		case types.RoleUser:
			flushToolResults()
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			messages = append(messages, anthropicMessage{
				Role:    "user",
				Content: []anthropicBlock{{Type: "text", Text: msg.Content}},
			})

		case types.RoleAssistant:
			flushToolResults()
			blocks := make([]anthropicBlock, 0, 1+len(msg.ToolCalls))
			if msg.Content != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, anthropicBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: normalizeAnthropicToolInput(string(tc.Input)),
				})
			}
			if len(blocks) == 0 {
				// Anthropic requires non-empty assistant content; this can
				// happen after an empty stream or a reasoning-only turn.
				blocks = append(blocks, anthropicBlock{Type: "text", Text: "(no content)"})
			}
			messages = append(messages, anthropicMessage{Role: "assistant", Content: blocks})

		case types.RoleTool:
			content := msg.Content
			if strings.TrimSpace(content) == "" {
				// Anthropic rejects empty text content inside tool_result.
				content = "(empty tool result)"
			}
			pendingResults = append(pendingResults, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Result:    content,
			})
		}
	}
	flushToolResults()

	return system, messages
}

// normalizeAnthropicToolInput coerces an assembled tool-call input into the
// JSON object Anthropic requires: empty or non-object input becomes an empty
// object so replaying a historical assistant turn never 400s.
func normalizeAnthropicToolInput(raw string) json.RawMessage {
	s := strings.TrimSpace(raw)
	if s == "" || s[0] != '{' || !isValidJSON(s) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
