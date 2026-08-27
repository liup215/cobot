package cobot

import (
	"github.com/liup215/cobot/loop"
	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
)

// Re-exported component types so callers can implement the agent's hooks
// (callbacks, guards, storage) importing only this root package. These are
// aliases — they are identical to the underlying package types.

// Mode selects the agent operating mode.
type Mode = loop.Mode

const (
	// ModePlan explores and plans without making changes (write tools hidden).
	ModePlan = loop.ModePlan
	// ModeAct executes tasks and modifies files.
	ModeAct = loop.ModeAct
)

// Callback receives streaming events for the whole run: content deltas,
// reasoning, tool lifecycle, compaction, retries, completion and errors.
// Embed CallbackAdapter to implement only what you need.
type Callback = provider.StreamCallback

// CallbackAdapter is the no-op base for partial Callback implementations.
type CallbackAdapter = provider.StreamCallbackAdapter

// ToolGuard is the approval gate consulted before every tool execution.
type ToolGuard = loop.ToolGuard

// GuardedCall describes the tool call a ToolGuard is asked to approve.
type GuardedCall = loop.GuardedCall

// ErrAbortRun is the sentinel a ToolGuard returns to abort the whole run.
var ErrAbortRun = loop.ErrAbortRun

// Store persists tasks, messages and tool calls.
type Store = loop.Store

// Provider is a streaming LLM backend.
type Provider = provider.Provider

// ToolRegistry is the tool set made available to the model.
type ToolRegistry = tools.Registry

// Tool is a single tool implementation.
type Tool = tools.Tool

// DefaultRetryDelays is the default connection-retry backoff schedule.
var DefaultRetryDelays = loop.DefaultRetryDelays

// DefaultAgentName is the agent identity used in the system prompt when
// Config.AgentName is empty.
const DefaultAgentName = prompts.DefaultAgentName
