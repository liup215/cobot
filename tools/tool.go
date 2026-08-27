// Package tools provides the tool system for agentcore.
// Tools are functions that the Agent can call to interact with the system.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is the interface for all tools
type Tool interface {
	// Name returns the tool name (must be unique)
	Name() string

	// Description returns a description of what the tool does
	Description() string

	// InputSchema returns the JSON schema for the tool's input parameters
	InputSchema() json.RawMessage

	// Execute runs the tool with the given input
	// Input is the JSON input parameters
	// Returns the result as a string (can be JSON)
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

// BaseTool provides common functionality for tools
type BaseTool struct {
	name        string
	description string
	inputSchema json.RawMessage
}

// Name returns the tool name
func (t *BaseTool) Name() string {
	return t.name
}

// Description returns the tool description
func (t *BaseTool) Description() string {
	return t.description
}

// InputSchema returns the tool's input schema
func (t *BaseTool) InputSchema() json.RawMessage {
	return t.inputSchema
}

// ToolResult represents the result of a tool execution
type ToolResult struct {
	// Success indicates if the tool executed successfully
	Success bool

	// Output is the tool output
	Output string

	// Error is the error message if Success is false
	Error string
}

// ToJSON converts the result to JSON
func (r *ToolResult) ToJSON() string {
	data, _ := json.Marshal(r)
	return string(data)
}

// ParseInput parses the JSON input into the target struct
func ParseInput(input json.RawMessage, target interface{}) error {
	if err := json.Unmarshal(input, target); err != nil {
		return fmt.Errorf("failed to parse input: %w", err)
	}
	return nil
}

// DisplayMode constants for ToolBehavior
const (
	// DisplayDefault shows a standard system message (e.g. "🔧 read_file: main.go")
	DisplayDefault = "default"
	// DisplayAssistant renders the tool output as an assistant message (full markdown)
	DisplayAssistant = "assistant"
	// DisplaySkip suppresses the message entirely (displayed elsewhere)
	DisplaySkip = "skip"
)

// ToolBehavior defines how a tool's start/complete events are displayed in the UI.
// This is metadata only — the TUI reads it to decide rendering; Tool itself has no UI dependency.
type ToolBehavior struct {
	// StartDisplayMode controls how the tool-start event is rendered.
	// Values: DisplayDefault, DisplayAssistant, DisplaySkip.
	StartDisplayMode string

	// CompleteDisplayMode controls how the tool-complete event is rendered.
	// Values: DisplayDefault, DisplayAssistant, DisplaySkip.
	CompleteDisplayMode string
}

// DefaultToolBehavior returns the default behavior (both modes = DisplayDefault).
func DefaultToolBehavior() ToolBehavior {
	return ToolBehavior{
		StartDisplayMode:    DisplayDefault,
		CompleteDisplayMode: DisplayDefault,
	}
}

// ToolCategory represents the category of a tool
type ToolCategory string

const (
	// CategoryFile for file operations
	CategoryFile ToolCategory = "file"
	// CategoryCode for code operations
	CategoryCode ToolCategory = "code"
	// CategoryCommand for command execution
	CategoryCommand ToolCategory = "command"
	// CategorySearch for search operations
	CategorySearch ToolCategory = "search"
	// CategoryInteraction for user interaction
	CategoryInteraction ToolCategory = "interaction"
	// CategoryCompletion for task completion
	CategoryCompletion ToolCategory = "completion"
	// CategoryNetwork for network operations
	CategoryNetwork ToolCategory = "network"
)

// ToolInfo contains metadata about a tool
type ToolInfo struct {
	// Tool is the tool implementation
	Tool Tool

	// Category is the tool category
	Category ToolCategory

	// AllowedModes lists which modes can use this tool
	AllowedModes []string

	// RequiresConfirmation indicates if user confirmation is needed
	RequiresConfirmation bool

	// Behavior controls how this tool's events are displayed in the UI.
	// Defaults to DefaultToolBehavior() when zero-valued.
	Behavior ToolBehavior
}

// IsAllowedInMode checks if the tool can be used in the given mode
func (i *ToolInfo) IsAllowedInMode(mode string) bool {
	for _, m := range i.AllowedModes {
		if m == mode || m == "*" {
			return true
		}
	}
	return false
}

// Common input schemas
var (
	// PathSchema is a common schema for file path parameters
	PathSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path of the file"
			}
		},
		"required": ["path"]
	}`)

	// PathAndContentSchema is a common schema for file operations with content
	PathAndContentSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path of the file"
			},
			"content": {
				"type": "string",
				"description": "The content to write"
			}
		},
		"required": ["path", "content"]
	}`)

	// CommandSchema is a common schema for command execution
	CommandSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The command to execute"
			},
			"requires_approval": {
				"type": "boolean",
				"description": "Whether this command requires user approval",
				"default": false
			}
		},
		"required": ["command"]
	}`)

	// SearchSchema is a common schema for search operations
	SearchSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The directory to search in"
			},
			"regex": {
				"type": "string",
				"description": "The regex pattern to search for"
			},
			"file_pattern": {
				"type": "string",
				"description": "Optional glob pattern to filter files"
			}
		},
		"required": ["path", "regex"]
	}`)

	// QuestionSchema is a common schema for asking questions
	QuestionSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"question": {
				"type": "string",
				"description": "The question to ask the user"
			},
			"options": {
				"type": "array",
				"items": {
					"type": "string"
				},
				"description": "Optional list of options for the user to choose from"
			}
		},
		"required": ["question"]
	}`)

	// CompletionSchema is a common schema for task completion
	CompletionSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"result": {
				"type": "string",
				"description": "A brief summary of what was accomplished"
			},
			"command": {
				"type": "string",
				"description": "Optional command to showcase the result"
			}
		},
		"required": ["result"]
	}`)
)
