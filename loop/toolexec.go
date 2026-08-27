package loop

import (
	"context"
	"fmt"

	"github.com/liup215/agentcore/tools"
	"github.com/liup215/agentcore/types"
)

// executeToolCalls runs the tool calls attached to the last assistant
// message. This is the main-loop execution path; pre-dispatched results
// (computed while the SSE stream was still active) are used when available.
// Every call passes through the ToolGuard (if configured) before execution.
func (a *BaseAgent) executeToolCalls(ctx context.Context, lastMsg types.Message, callback StreamCallback) {
	// Execute tools. Pre-dispatched results (computed while the stream
	// was still active) are used when available; otherwise tools run
	// synchronously here.
	for _, tc := range lastMsg.ToolCalls {
		if a.abort.Load() {
			break
		}

		// Check if tool is allowed in current mode
		if !a.toolRegistry.IsAllowed(string(a.mode), tc.Name) {
			errorMsg := fmt.Sprintf("Error: Tool '%s' is not allowed in %s mode.", tc.Name, a.mode)
			a.conversation.AddMessage(types.Message{
				Role:       types.RoleTool,
				ToolCallID: tc.ID,
				Content:    errorMsg,
			})
			callback.OnToolCallComplete(ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: string(tc.Input),
			}, errorMsg)
			continue
		}

		// Get the tool from registry
		tool, err := a.toolRegistry.Get(tc.Name)
		if err != nil {
			errorMsg := fmt.Sprintf("Error: Tool '%s' not found: %v", tc.Name, err)
			a.conversation.AddMessage(types.Message{
				Role:       types.RoleTool,
				ToolCallID: tc.ID,
				Content:    errorMsg,
			})
			continue
		}

		// ToolGuard approval gate (main-loop path). A denial feeds the
		// reason back to the model as a tool result; the loop continues.
		allow, denyReason, guardAbort := a.applyGuard(ctx, GuardedCall{Name: tc.Name, Input: string(tc.Input)})
		if guardAbort {
			a.abort.Store(true)
			return
		}
		if !allow {
			result := "[DENIED] " + denyReason
			a.conversation.AddMessage(types.Message{
				Role:       types.RoleTool,
				ToolCallID: tc.ID,
				Content:    result,
			})
			callback.OnToolCallComplete(ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: string(tc.Input),
			}, result)
			continue
		}

		// Only fire OnToolCallStart if this tool wasn't already
		// pre-dispatched during the stream (where the callback was
		// already triggered when the complete chunk arrived).
		a.preDispatchedIDsMu.Lock()
		wasPreDispatched := a.preDispatchedIDs[tc.ID]
		delete(a.preDispatchedIDs, tc.ID)
		a.preDispatchedIDsMu.Unlock()
		if !wasPreDispatched {
			callback.OnToolCallStart(ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: string(tc.Input),
			})
		}

		// Record tool call start in storage
		var callID int64
		if a.store != nil && a.taskID != "" {
			cid, err := a.store.StartToolCall(a.taskID, tc.Name, tc.Input)
			if err != nil {
				logWarnf("Failed to record tool call start: %v", err)
			} else {
				callID = cid
			}
		}

		// If this is the ask_followup_question tool and the callback supports AskFollowupQuestion,
		// inject the TUI/Callback handler so the tool doesn't read directly from stdin.
		if askTool, ok := tool.(*tools.AskFollowupQuestionTool); ok {
			askTool.SetHandler(func(question string, options []string) (string, error) {
				return callback.AskFollowupQuestion(question, options)
			})
		}

		var result string
		var execErr error

		// Use pre-dispatched result if available.
		pre, ok := a.takePreDispatchResult(tc.ID)
		if ok {
			result = pre.result
			execErr = pre.err
		} else {
			// Execute the tool synchronously as fallback.
			result, execErr = tool.Execute(ctx, tc.Input)
			if execErr != nil {
				result = fmt.Sprintf("Error: %v", execErr)
			}
		}

		// Add tool result to conversation
		a.conversation.AddMessage(types.Message{
			Role:       types.RoleTool,
			ToolCallID: tc.ID,
			Content:    result,
		})

		// Save tool result message and complete tool call record
		if a.store != nil && a.taskID != "" {
			if lastMsg := a.conversation.GetLastMessage(); lastMsg != nil {
				if dbErr := a.store.SaveMessage(a.taskID, *lastMsg); dbErr != nil {
					logWarnf("Failed to save tool result message: %v", dbErr)
				}
			}
			if callID > 0 {
				if execErr != nil {
					if dbErr := a.store.FailToolCall(callID, execErr); dbErr != nil {
						logWarnf("Failed to record tool call failure: %v", dbErr)
					}
				} else {
					if dbErr := a.store.CompleteToolCall(callID, result); dbErr != nil {
						logWarnf("Failed to record tool call completion: %v", dbErr)
					}
				}
			}
		}

		// Notify callback that tool is complete
		callback.OnToolCallComplete(ToolCall{
			ID:    tc.ID,
			Name:  tc.Name,
			Input: string(tc.Input),
		}, result)

		// Special tools that can terminate the conversation.
		// Only attempt_completion (task done) or plan_mode_respond
		// (plan mode turn finished) mark the conversation as complete.
		// ask_followup_question must NOT mark it complete — the agent
		// needs to continue the loop after receiving the user's answer.
		switch tc.Name {
		case types.ToolAttemptCompletion.String(),
			types.ToolPlanModeRespond.String():
			a.conversation.SetComplete()
		}
	}
}
