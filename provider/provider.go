package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/liup215/agentcore/types"
)

// Provider is the interface for LLM providers
type Provider interface {
	// CreateMessage sends a message to the LLM and returns the response
	CreateMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error)

	// CreateMessageStream sends a message to the LLM and returns a stream of responses
	// This is used for real-time output in TUI mode
	CreateMessageStream(ctx context.Context, req *MessageRequest) (<-chan StreamChunk, error)

	// SupportsTools returns true if the provider supports tool/function calling
	SupportsTools() bool

	// GetModel returns the current model name
	GetModel() string

	// GetProviderName returns the provider name (e.g., "anthropic", "openai")
	GetProviderName() string
}

// StreamChunk represents a chunk of a streaming response
type StreamChunk struct {
	// Content is the text content delta (incremental)
	Content string

	// ReasoningContent is optional model-provided internal reasoning/thinking delivered in the stream
	ReasoningContent string

	// ToolCall contains a tool call if this chunk is a tool call
	ToolCall *ToolCall

	// IsPartial indicates if this is a partial/incomplete chunk
	// For tool calls, this means the tool call is still being streamed
	// For text, this is typically false as text is appended directly
	IsPartial bool

	// FinishReason indicates why the response finished (if this is the last chunk)
	FinishReason string

	// Usage contains token usage information (if available, usually in the last chunk)
	Usage TokenUsage

	// Error contains any error that occurred during streaming
	Error error

	// Done is true when the stream is complete
	Done bool
}

// ToolChoiceOption controls whether the model must use tools.
type ToolChoiceOption string

const (
	// ToolChoiceAuto lets the model decide whether to call tools.
	ToolChoiceAuto ToolChoiceOption = "auto"
	// ToolChoiceRequired forces the model to call at least one tool.
	ToolChoiceRequired ToolChoiceOption = "required"
	// ToolChoiceNone prevents the model from calling any tools.
	ToolChoiceNone ToolChoiceOption = "none"
)

// MessageRequest represents a request to the LLM
type MessageRequest struct {
	// Messages is the conversation history
	Messages []types.Message

	// Tools is the list of available tools
	Tools []ToolDefinition

	// SystemPrompt is the system prompt to use
	SystemPrompt string

	// MaxTokens is the maximum number of tokens to generate
	MaxTokens int

	// Temperature controls randomness (0.0 - 1.0)
	Temperature float64

	// ToolChoice forces the model to use a tool when set to "required".
	// The agent loop sets this automatically whenever work is still pending.
	ToolChoice ToolChoiceOption
}

// MessageResponse represents a response from the LLM
type MessageResponse struct {
	// Content is the text content of the response
	Content string

	// ReasoningContent carries model-provided internal reasoning/thinking (if any)
	ReasoningContent string

	// ToolCalls contains any tool calls requested by the LLM
	ToolCalls []ToolCall

	// Usage contains token usage information
	Usage TokenUsage

	// FinishReason indicates why the response finished
	FinishReason string
}

// ToolCall represents a tool call requested by the LLM
type ToolCall struct {
	// ID is the unique identifier for this tool call
	ID string

	// Name is the name of the tool to call
	Name string

	// Input is the JSON input for the tool
	Input string
}

// ToolDefinition defines a tool that can be called by the LLM
type ToolDefinition struct {
	// Name is the tool name
	Name string

	// Description describes what the tool does
	Description string

	// InputSchema is the JSON schema for the tool's input parameters
	InputSchema json.RawMessage
}

// TokenUsage contains information about token usage
type TokenUsage struct {
	// InputTokens is the number of tokens in the input
	InputTokens int

	// OutputTokens is the number of tokens in the output
	OutputTokens int

	// TotalTokens is the total number of tokens
	TotalTokens int
}

// ProviderConfig contains common configuration for providers
type ProviderConfig struct {
	// APIKey is the API key for the provider
	APIKey string

	// Model is the model to use
	Model string

	// BaseURL is the base URL for the API (optional, for custom endpoints)
	BaseURL string

	// MaxTokens is the maximum tokens to generate
	MaxTokens int

	// Temperature controls randomness
	Temperature float64

	// Timeout is the request timeout in seconds
	Timeout int
}

// DefaultProviderConfig returns a default provider configuration
func DefaultProviderConfig() ProviderConfig {
	return ProviderConfig{
		MaxTokens:   4096,
		Temperature: 0.0,
		Timeout:     120,
	}
}

// StreamCallback is the interface for receiving streaming updates
// This allows the Agent to notify the UI of content updates, tool calls, etc.
type StreamCallback interface {
	// OnContent is called when new content (the assistant reply text) is received.
	OnContent(delta string)

	// OnReasoning is called when the LLM emits a thinking / reasoning block.
	// UI implementations can choose to show this in a collapsible panel or hide it completely.
	OnReasoning(delta string)

	// OnStreamStart is called when a new streaming response begins
	// This allows the UI to prepare a new assistant message slot for streaming content.
	OnStreamStart()

	// OnToolCallStart is called when a tool call starts
	OnToolCallStart(toolCall ToolCall)

	// OnToolCallComplete is called when a tool call completes with its result
	OnToolCallComplete(toolCall ToolCall, result string)

	// OnToolCallUpdate is called with progress/status updates while a tool
	// is executing (e.g. "running" before a synchronous execution). It lets
	// UIs show live tool state without waiting for completion.
	OnToolCallUpdate(toolCall ToolCall, status string)

	// AskFollowupQuestion is invoked when the agent needs to prompt the user for clarification.
	// Implementations should present the question/options to the user and return their selected answer.
	AskFollowupQuestion(question string, options []string) (string, error)

	// OnError is called when an error occurs
	OnError(err error)

	// OnComplete is called when the conversation is complete
	OnComplete()

	// OnTaskCreated is called when a new task is created with its persistent ID.
	// This allows the UI to associate the conversation with a stored task.
	OnTaskCreated(taskID string)

	// OnCompactionStart is called before the agent trims or compacts the
	// conversation to fit the token budget. The reason describes the trigger
	// (e.g. "token budget 75% of 128000").
	OnCompactionStart(reason string)

	// OnCompactionEnd is called after compaction finished; the summary
	// describes the outcome (remaining token estimate).
	OnCompactionEnd(summary string)

	// OnRetry is called when the agent retries a failed stream connection.
	// attempt is the upcoming retry number (1-based), err is the failure
	// being retried, nextDelay is how long the loop will wait first.
	OnRetry(attempt int, err error, nextDelay time.Duration)
}

// StreamCallbackAdapter is a no-op adapter for non-streaming scenarios.
// It also provides a simple CLI fallback implementation for AskFollowupQuestion.
type StreamCallbackAdapter struct{}

func (a *StreamCallbackAdapter) OnContent(delta string)                                  {}
func (a *StreamCallbackAdapter) OnReasoning(delta string)                                {}
func (a *StreamCallbackAdapter) OnStreamStart()                                          {}
func (a *StreamCallbackAdapter) OnToolCallStart(toolCall ToolCall)                       {}
func (a *StreamCallbackAdapter) OnToolCallComplete(toolCall ToolCall, result string)     {}
func (a *StreamCallbackAdapter) OnToolCallUpdate(toolCall ToolCall, status string)       {}
func (a *StreamCallbackAdapter) OnCompactionStart(reason string)                         {}
func (a *StreamCallbackAdapter) OnCompactionEnd(summary string)                          {}
func (a *StreamCallbackAdapter) OnRetry(attempt int, err error, nextDelay time.Duration) {}
func (a *StreamCallbackAdapter) OnError(err error)                                       {}
func (a *StreamCallbackAdapter) OnComplete()                                             {}
func (a *StreamCallbackAdapter) OnTaskCreated(taskID string)                             {}

// AskFollowupQuestion provides a CLI fallback when no UI callback is available.
// It prints the question and options to stdout and reads from stdin.
func (a *StreamCallbackAdapter) AskFollowupQuestion(question string, options []string) (string, error) {
	// Print question
	fmt.Println()
	fmt.Println("❓", question)
	fmt.Println()

	// Print options if provided
	if len(options) > 0 {
		fmt.Println("Options:")
		for i, opt := range options {
			fmt.Printf("  %d. %s\n", i+1, opt)
		}
		fmt.Println()
	}

	// Read user input from stdin
	fmt.Print("Your answer: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	answer := strings.TrimSpace(line)

	// If the user provided a numeric option, map it to the option text
	if len(options) > 0 {
		if n, err := strconv.Atoi(answer); err == nil && n > 0 && n <= len(options) {
			return options[n-1], nil
		}
	}

	return answer, nil
}

// Ensure StreamCallbackAdapter implements StreamCallback
var _ StreamCallback = (*StreamCallbackAdapter)(nil)
