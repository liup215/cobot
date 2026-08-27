// Package types contains the shared data structures used across agentcore:
// messages, tool calls, conversations and token estimation.
//
// The package has no dependencies outside the standard library.
package types

import (
	"encoding/json"
	"time"
)

// Role represents the role of a message sender.
type Role string

const (
	// RoleSystem represents a system message.
	RoleSystem Role = "system"
	// RoleUser represents a user message.
	RoleUser Role = "user"
	// RoleAssistant represents an assistant message.
	RoleAssistant Role = "assistant"
	// RoleTool represents a tool result message.
	RoleTool Role = "tool"
)

// ToolName defines the built-in tool names of the v1 tool set.
type ToolName string

const (
	// ToolReadFile reads the contents of a file.
	ToolReadFile ToolName = "read_file"
	// ToolWriteToFile writes content to a file (side effect).
	ToolWriteToFile ToolName = "write_to_file"
	// ToolReplaceInFile replaces content in a file via search/replace blocks (side effect).
	ToolReplaceInFile ToolName = "replace_in_file"
	// ToolExecuteCommand executes a CLI command (side effect).
	ToolExecuteCommand ToolName = "execute_command"
	// ToolSearchFiles searches a regex pattern across files.
	ToolSearchFiles ToolName = "search_files"
	// ToolListFiles lists files and directories.
	ToolListFiles ToolName = "list_files"
	// ToolListCodeDefinitionNames lists code definitions (functions, classes)
	// in a directory (read-only structure overview).
	ToolListCodeDefinitionNames ToolName = "list_code_definition_names"
	// ToolAttemptCompletion is the terminal tool that ends a run.
	ToolAttemptCompletion ToolName = "attempt_completion"
	// ToolAskFollowupQuestion asks the user a clarifying question (does NOT end the run).
	ToolAskFollowupQuestion ToolName = "ask_followup_question"
	// ToolPlanModeRespond is the terminal tool for plan mode turns.
	ToolPlanModeRespond ToolName = "plan_mode_respond"
)

// String returns the tool name as a plain string.
func (t ToolName) String() string { return string(t) }

// IsSpecialTool returns true if the tool requires special handling in the
// agent loop (completion / question / plan response tools).
func (t ToolName) IsSpecialTool() bool {
	switch t {
	case ToolAttemptCompletion, ToolAskFollowupQuestion, ToolPlanModeRespond:
		return true
	default:
		return false
	}
}

// Message represents a single message in a conversation.
type Message struct {
	// Role is the sender role.
	Role Role

	// Content is the message content.
	Content string

	// ReasoningContent stores model-provided internal reasoning/thinking, if any.
	// It is kept strictly separate from Content so consumers can render or
	// persist it independently.
	ReasoningContent string

	// ToolCalls contains tool calls issued by the assistant.
	ToolCalls []ToolCall

	// ToolCallID identifies which tool call this result is for (RoleTool only).
	ToolCallID string

	// AvailableTools optionally records the JSON list of tool names that were
	// available to the assistant when this message was produced. It is a
	// diagnostic field; consumers may ignore it.
	AvailableTools json.RawMessage

	// ToolChoice records the tool_choice setting sent with the request
	// (e.g. "auto", "required", "none").
	ToolChoice string

	// Timestamp when the message was created.
	Timestamp time.Time
}

// ToolCall represents a tool call requested by the LLM.
type ToolCall struct {
	// ID is the unique identifier for this tool call.
	ID string

	// Name is the name of the tool to call.
	Name string

	// Input is the JSON input for the tool.
	Input json.RawMessage
}

// Clone returns a deep copy of the tool call.
func (tc ToolCall) Clone() ToolCall {
	input := make(json.RawMessage, len(tc.Input))
	copy(input, tc.Input)
	return ToolCall{ID: tc.ID, Name: tc.Name, Input: input}
}
