// Package provider provides LLM provider implementations
package provider

import (
	"context"
	"fmt"
	"time"
)

// MockProvider implements a mock provider for testing streaming and tool calls
// It simulates OpenAI-compatible SSE streaming responses

// MockScenario selects which canned conversation a MockProvider replays.
type MockScenario string

const (
	// MockScenarioLongText - Long text streaming response
	MockScenarioLongText MockScenario = "long_text"
	// MockScenarioToolCall - Single tool call with streaming
	MockScenarioToolCall MockScenario = "tool_call"
	// MockScenarioToolThenText - Tool call followed by text response
	MockScenarioToolThenText MockScenario = "tool_then_text"
	// MockScenarioMultiTool - Multiple tool calls
	MockScenarioMultiTool MockScenario = "multi_tool"
	// MockScenarioError - Error response
	MockScenarioError MockScenario = "error"
)

// MockProvider implements the Provider interface for testing
type MockProvider struct {
	scenario  MockScenario
	delay     time.Duration
	chunkSize int
}

// NewMockProvider creates a new mock provider for testing
// Parameters:
//   - scenario: The test scenario to run
//   - delay: Delay between chunks (default 50ms)
//   - chunkSize: Characters per chunk (default 20)
func NewMockProvider(scenario MockScenario, delay time.Duration, chunkSize int) *MockProvider {
	if delay == 0 {
		delay = 50 * time.Millisecond
	}
	if chunkSize == 0 {
		chunkSize = 20
	}
	return &MockProvider{
		scenario:  scenario,
		delay:     delay,
		chunkSize: chunkSize,
	}
}

// CreateMessage sends a message and returns a complete response (non-streaming)
func (p *MockProvider) CreateMessage(ctx context.Context, req *MessageRequest) (*MessageResponse, error) {
	// For mock, we just return a simple response
	return &MessageResponse{
		Content:      "This is a mock response. Use CreateMessageStream for streaming.",
		ToolCalls:    nil,
		FinishReason: "stop",
		Usage: TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}, nil
}

// CreateMessageStream sends a message and returns a stream of responses
// This simulates OpenAI's SSE streaming format
func (p *MockProvider) CreateMessageStream(ctx context.Context, req *MessageRequest) (<-chan StreamChunk, error) {
	chunkChan := make(chan StreamChunk)

	go func() {
		defer close(chunkChan)

		switch p.scenario {
		case MockScenarioLongText:
			p.streamLongText(ctx, chunkChan)
		case MockScenarioToolCall:
			p.streamToolCall(ctx, chunkChan)
		case MockScenarioToolThenText:
			p.streamToolThenText(ctx, chunkChan)
		case MockScenarioMultiTool:
			p.streamMultiTool(ctx, chunkChan)
		case MockScenarioError:
			p.streamError(ctx, chunkChan)
		default:
			p.streamLongText(ctx, chunkChan)
		}
	}()

	return chunkChan, nil
}

// streamLongText simulates a long text response with streaming
func (p *MockProvider) streamLongText(ctx context.Context, chunkChan chan<- StreamChunk) {
	longText := `I'll help you understand the architecture of this project. Let me break it down for you:

The project follows a clean architecture pattern with clear separation of concerns:

1. **Core Domain Layer**: This contains the business logic and entities. It's independent of any external frameworks or libraries. The domain models define the core business rules and invariants.

2. **Application Layer**: This layer orchestrates the use cases. It coordinates the domain objects to perform specific tasks. The application services are responsible for transaction management and coordinating multiple domain objects.

3. **Infrastructure Layer**: This provides implementations for external concerns like database access, file systems, and external APIs. It adapts the domain layer to specific technologies.

4. **Interface Layer**: This handles the user interface and external API endpoints. It translates user input into application commands and presents the results back to users.

The dependency flow follows the Dependency Inversion Principle - inner layers define interfaces that outer layers implement. This ensures that the core business logic remains pure and testable.

Key benefits of this architecture:
- **Testability**: Business logic can be tested without external dependencies
- **Flexibility**: Easy to swap implementations (e.g., change database)
- **Maintainability**: Clear boundaries make code easier to understand and modify
- **Scalability**: Each layer can be scaled independently

The project also uses several design patterns:
- Repository pattern for data access
- Factory pattern for object creation
- Strategy pattern for interchangeable algorithms
- Observer pattern for event handling

Would you like me to dive deeper into any specific aspect of the architecture?`

	// Split text into chunks
	chunks := splitIntoChunks(longText, p.chunkSize)

	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return
		}

		chunkChan <- StreamChunk{
			Content: chunk,
		}

		// Add delay between chunks to simulate network latency
		time.Sleep(p.delay)

		// Simulate occasional longer delays
		if i%10 == 0 {
			time.Sleep(p.delay * 2)
		}
	}

	// Send completion
	chunkChan <- StreamChunk{
		FinishReason: "stop",
		Done:         true,
		Usage: TokenUsage{
			InputTokens:  50,
			OutputTokens: len(longText) / 4, // Approximate token count
			TotalTokens:  50 + len(longText)/4,
		},
	}
}

// streamToolCall simulates a tool call with streaming parameter building
func (p *MockProvider) streamToolCall(ctx context.Context, chunkChan chan<- StreamChunk) {
	// First, send some introductory text
	intro := "I'll help you read that file. Let me fetch it for you."
	chunks := splitIntoChunks(intro, p.chunkSize)
	for _, chunk := range chunks {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{
			Content: chunk,
		}
		time.Sleep(p.delay)
	}

	// Simulate tool call building - OpenAI format sends tool calls incrementally
	toolID := "call_abc123"
	toolName := "read_file"

	// Step 1: Send tool call start (name only)
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:   toolID,
			Name: toolName,
		},
		IsPartial: true,
	}
	time.Sleep(p.delay * 2)

	// Step 2: Send partial arguments (path field opening)
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    toolID,
			Name:  toolName,
			Input: `{"path": "`,
		},
		IsPartial: true,
	}
	time.Sleep(p.delay)

	// Step 3: Send more of path
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    toolID,
			Name:  toolName,
			Input: `{"path": "/home/user/`,
		},
		IsPartial: true,
	}
	time.Sleep(p.delay)

	// Step 4: Complete path and close JSON
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    toolID,
			Name:  toolName,
			Input: `{"path": "/home/user/project/README.md"}`,
		},
		IsPartial: true,
	}
	time.Sleep(p.delay * 2)

	// Step 5: Send complete tool call
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    toolID,
			Name:  toolName,
			Input: `{"path": "/home/user/project/README.md"}`,
		},
		IsPartial: false,
	}

	// Send completion
	chunkChan <- StreamChunk{
		FinishReason: "tool_calls",
		Done:         true,
		Usage: TokenUsage{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	}
}

// streamToolThenText simulates a tool call followed by text response
func (p *MockProvider) streamToolThenText(ctx context.Context, chunkChan chan<- StreamChunk) {
	// First, send introductory text
	intro := "Let me check the configuration file for you."
	for _, chunk := range splitIntoChunks(intro, p.chunkSize) {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{Content: chunk}
		time.Sleep(p.delay)
	}

	// Send tool call
	toolID := "call_def456"
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    toolID,
			Name:  "read_file",
			Input: `{"path": "/etc/config.json"}`,
		},
		IsPartial: false,
	}

	// Simulate tool execution delay
	time.Sleep(p.delay * 5)

	// Send follow-up text explaining the result
	followUp := `Based on the configuration file, I can see several important settings:

1. The database connection is configured to use PostgreSQL with connection pooling enabled.
2. Cache settings show a TTL of 3600 seconds with Redis as the backend.
3. API rate limiting is set to 1000 requests per hour per API key.
4. Logging is configured at INFO level with structured JSON output.

The configuration looks well-optimized for production use. The connection pooling and caching should provide good performance under load. Would you like me to explain any specific configuration option in more detail?`

	for _, chunk := range splitIntoChunks(followUp, p.chunkSize) {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{Content: chunk}
		time.Sleep(p.delay)
	}

	chunkChan <- StreamChunk{
		FinishReason: "stop",
		Done:         true,
		Usage: TokenUsage{
			InputTokens:  150,
			OutputTokens: 200,
			TotalTokens:  350,
		},
	}
}

// streamMultiTool simulates multiple tool calls
func (p *MockProvider) streamMultiTool(ctx context.Context, chunkChan chan<- StreamChunk) {
	intro := "I'll analyze the project structure for you. Let me gather some information."
	for _, chunk := range splitIntoChunks(intro, p.chunkSize) {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{Content: chunk}
		time.Sleep(p.delay)
	}

	// First tool: list directory
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    "call_111",
			Name:  "list_directory",
			Input: `{"path": "/home/user/project"}`,
		},
		IsPartial: false,
	}
	time.Sleep(p.delay * 3)

	// Second tool: read specific file
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    "call_222",
			Name:  "read_file",
			Input: `{"path": "/home/user/project/package.json"}`,
		},
		IsPartial: false,
	}
	time.Sleep(p.delay * 3)

	// Third tool: search for patterns
	chunkChan <- StreamChunk{
		ToolCall: &ToolCall{
			ID:    "call_333",
			Name:  "search_files",
			Input: `{"path": "/home/user/project", "regex": "TODO|FIXME"}`,
		},
		IsPartial: false,
	}

	// Final summary
	summary := `I've analyzed the project structure. Here's what I found:

The project contains:
- Source code in the src/ directory
- Configuration files (package.json, tsconfig.json)
- Test files in the tests/ directory
- Documentation in the docs/ folder

I also found 3 TODO comments and 1 FIXME comment that might need attention. The package.json shows this is a Node.js project using TypeScript with several dependencies for testing and linting.

Would you like me to examine any specific file or directory in more detail?`

	for _, chunk := range splitIntoChunks(summary, p.chunkSize) {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{Content: chunk}
		time.Sleep(p.delay)
	}

	chunkChan <- StreamChunk{
		FinishReason: "stop",
		Done:         true,
		Usage: TokenUsage{
			InputTokens:  200,
			OutputTokens: 300,
			TotalTokens:  500,
		},
	}
}

// streamError simulates an error response
func (p *MockProvider) streamError(ctx context.Context, chunkChan chan<- StreamChunk) {
	// Send some text first
	for _, chunk := range splitIntoChunks("Let me try to process your request...", p.chunkSize) {
		if ctx.Err() != nil {
			return
		}
		chunkChan <- StreamChunk{Content: chunk}
		time.Sleep(p.delay)
	}

	// Then send an error
	chunkChan <- StreamChunk{
		Error: fmt.Errorf("mock error: rate limit exceeded. Please try again in 60 seconds."),
		Done:  true,
	}
}

// SupportsTools returns true if the provider supports tool calling
func (p *MockProvider) SupportsTools() bool {
	return true
}

// GetModel returns the current model name
func (p *MockProvider) GetModel() string {
	return "mock-model"
}

// GetProviderName returns the provider name
func (p *MockProvider) GetProviderName() string {
	return "mock"
}

// splitIntoChunks splits text into chunks of approximately chunkSize characters
// It tries to split at word boundaries for better readability
func splitIntoChunks(text string, chunkSize int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	start := 0

	for start < len(text) {
		end := start + chunkSize
		if end >= len(text) {
			chunks = append(chunks, text[start:])
			break
		}

		// Try to find a word boundary
		for end > start && end < len(text) && text[end] != ' ' && text[end] != '\n' {
			end--
		}

		// If no word boundary found, just use the chunk size
		if end == start {
			end = start + chunkSize
		}

		chunks = append(chunks, text[start:end])
		start = end

		// Skip whitespace at start of next chunk
		for start < len(text) && (text[start] == ' ' || text[start] == '\n') {
			start++
		}
	}

	return chunks
}

// RegisterMockProvider registers the mock provider in the registry
func RegisterMockProvider() {
	// This function can be called to register the mock provider
	// Implementation depends on how providers are registered in your system
}
