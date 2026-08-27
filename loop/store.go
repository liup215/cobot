package loop

import (
	"encoding/json"

	"github.com/liup215/agentcore/types"
)

// Store is the optional persistence sink for a task run. It is the
// minimal write-path surface of gline's storage.Store, trimmed to what
// the agent loop itself needs; query/history methods are out of scope
// for the headless SDK (hosts implement their own reporting).
//
// All methods are best-effort: the loop logs (via slog) and continues on
// store errors — persistence failures never abort a run. A nil Store is
// valid and disables persistence entirely.
type Store interface {
	// CreateTask creates a new task record and returns its UUID.
	CreateTask(title, prompt, mode, provider, model, workingDir string) (taskID string, err error)

	// CompleteTask marks the task as completed.
	CompleteTask(taskID string) error

	// FailTask marks the task as failed with an optional error message.
	FailTask(taskID string, errMsg string) error

	// SaveMessage persists a single message to a task.
	SaveMessage(taskID string, msg types.Message) error

	// StartToolCall records the beginning of a tool call.
	StartToolCall(taskID, toolName string, input json.RawMessage) (callID int64, err error)

	// CompleteToolCall records successful completion of a tool call.
	CompleteToolCall(callID int64, output string) error

	// FailToolCall records a failed tool call.
	FailToolCall(callID int64, err error) error
}
