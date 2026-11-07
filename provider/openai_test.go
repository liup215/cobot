package provider

import (
	"testing"

	"github.com/liup215/cobot/types"
)

func TestNewOpenAIProvider(t *testing.T) {
	// Test with default values
	p := NewOpenAIProvider("test-key", "", "")
	if p.GetModel() != "gpt-4" {
		t.Errorf("Expected default model 'gpt-4', got '%s'", p.GetModel())
	}
	if p.GetBaseURL() != defaultOpenAIBaseURL {
		t.Errorf("Expected default URL '%s', got '%s'", defaultOpenAIBaseURL, p.GetBaseURL())
	}

	// Test with custom values
	p2 := NewOpenAIProvider("test-key", "gpt-3.5-turbo", "https://custom.api.com/v1")
	if p2.GetModel() != "gpt-3.5-turbo" {
		t.Errorf("Expected model 'gpt-3.5-turbo', got '%s'", p2.GetModel())
	}
	if p2.GetBaseURL() != "https://custom.api.com/v1" {
		t.Errorf("Expected custom URL, got '%s'", p2.GetBaseURL())
	}
}

func TestOpenAIProvider_GetProviderName(t *testing.T) {
	p := NewOpenAIProvider("test-key", "gpt-4", "")
	if p.GetProviderName() != "openai" {
		t.Errorf("Expected provider name 'openai', got '%s'", p.GetProviderName())
	}
}

func TestOpenAIProvider_SupportsTools(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		{"gpt-4", true},
		{"gpt-4-turbo", true},
		{"gpt-3.5-turbo", true},
		{"gpt-3.5-turbo-instruct", false},
		{"text-davinci-003", false},
		{"text-curie-001", false},
	}

	for _, tt := range tests {
		p := NewOpenAIProvider("test-key", tt.model, "")
		if p.SupportsTools() != tt.expected {
			t.Errorf("Model %s: expected SupportsTools=%v, got %v", tt.model, tt.expected, p.SupportsTools())
		}
	}
}

func TestOpenAIProvider_convertResponse(t *testing.T) {
	p := NewOpenAIProvider("test-key", "gpt-4", "")

	// Test normal response
	resp := &OpenAIResponse{
		ID:    "test-id",
		Model: "gpt-4",
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: OpenAIMessage{
					Role:    "assistant",
					Content: "Hello, world!",
				},
				FinishReason: "stop",
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	result := p.convertResponse(resp)

	if result.Content != "Hello, world!" {
		t.Errorf("Expected content 'Hello, world!', got '%s'", result.Content)
	}
	if result.FinishReason != "stop" {
		t.Errorf("Expected finish reason 'stop', got '%s'", result.FinishReason)
	}
	if result.Usage.InputTokens != 10 {
		t.Errorf("Expected input tokens 10, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 5 {
		t.Errorf("Expected output tokens 5, got %d", result.Usage.OutputTokens)
	}

	// Test response with tool calls
	respWithTools := &OpenAIResponse{
		ID:    "test-id-2",
		Model: "gpt-4",
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: OpenAIMessage{
					Role:    "assistant",
					Content: "I'll help you with that",
					ToolCalls: []OpenAIToolCall{
						{
							ID:   "call_123",
							Type: "function",
							Function: OpenAIFunction{
								Name:      "read",
								Arguments: `{"path": "test.txt"}`,
							},
						},
					},
				},
				FinishReason: "tool_calls",
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     20,
			CompletionTokens: 15,
			TotalTokens:      35,
		},
	}

	resultWithTools := p.convertResponse(respWithTools)

	if len(resultWithTools.ToolCalls) != 1 {
		t.Errorf("Expected 1 tool call, got %d", len(resultWithTools.ToolCalls))
	}

	if len(resultWithTools.ToolCalls) > 0 {
		tc := resultWithTools.ToolCalls[0]
		if tc.ID != "call_123" {
			t.Errorf("Expected tool call ID 'call_123', got '%s'", tc.ID)
		}
		if tc.Name != "read" {
			t.Errorf("Expected tool name 'read', got '%s'", tc.Name)
		}
		if tc.Input != `{"path": "test.txt"}` {
			t.Errorf("Expected input '{\"path\": \"test.txt\"}', got '%s'", tc.Input)
		}
	}

	// Test empty response
	emptyResp := &OpenAIResponse{
		Choices: []OpenAIChoice{},
		Usage: OpenAIUsage{
			PromptTokens:     5,
			CompletionTokens: 0,
			TotalTokens:      5,
		},
	}

	resultEmpty := p.convertResponse(emptyResp)
	if resultEmpty.Content != "" {
		t.Errorf("Expected empty content, got '%s'", resultEmpty.Content)
	}
	if resultEmpty.FinishReason != "error" {
		t.Errorf("Expected finish reason 'error' for empty response, got '%s'", resultEmpty.FinishReason)
	}
}

func TestOpenAIProvider_Registry(t *testing.T) {
	// Initialize registry
	InitDefaultRegistry()

	// Test that openai provider is registered
	if !IsProviderSupported("openai") {
		t.Error("Expected 'openai' provider to be registered")
	}

	// Test creating provider through registry
	config := ProviderConfig{
		APIKey:  "test-key",
		Model:   "gpt-4",
		BaseURL: "https://custom.api.com/v1",
	}

	provider, err := CreateDefault("openai", config)
	if err != nil {
		t.Fatalf("Failed to create openai provider: %v", err)
	}

	if provider.GetProviderName() != "openai" {
		t.Errorf("Expected provider name 'openai', got '%s'", provider.GetProviderName())
	}

	if provider.GetModel() != "gpt-4" {
		t.Errorf("Expected model 'gpt-4', got '%s'", provider.GetModel())
	}

	// Test GetProviderWithBaseURL
	provider2, err := GetProviderWithBaseURL("openai", "test-key", "gpt-3.5-turbo", "https://api.openai.com/v1")
	if err != nil {
		t.Fatalf("Failed to create provider with base URL: %v", err)
	}

	if provider2.GetModel() != "gpt-3.5-turbo" {
		t.Errorf("Expected model 'gpt-3.5-turbo', got '%s'", provider2.GetModel())
	}

	// Test GetProviderFromConfig
	config2 := ProviderConfig{
		APIKey:      "test-key",
		Model:       "gpt-4-turbo",
		BaseURL:     "https://openrouter.ai/api/v1",
		MaxTokens:   2048,
		Temperature: 0.7,
	}

	provider3, err := GetProviderFromConfig("openai", config2)
	if err != nil {
		t.Fatalf("Failed to create provider from config: %v", err)
	}

	if provider3.GetModel() != "gpt-4-turbo" {
		t.Errorf("Expected model 'gpt-4-turbo', got '%s'", provider3.GetModel())
	}
}

func TestOpenAIProvider_CreateMessageValidation(t *testing.T) {
	p := NewOpenAIProvider("", "gpt-4", "")

	// Test with empty API key
	req := &MessageRequest{
		Messages: []types.Message{
			{Role: types.RoleUser, Content: "Hello"},
		},
	}

	_, err := p.CreateMessage(nil, req)
	if err == nil {
		t.Error("Expected error when API key is empty")
	}
}
