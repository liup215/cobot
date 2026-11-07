package loop

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/liup215/cobot/provider"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// executeToolCalls runs the tool calls attached to the last assistant
// message. This is the main-loop execution path; pre-dispatched results
// (computed while the SSE stream was still active) are used when available.
// Every call passes through the ToolGuard (if configured) before execution.
//
// Every tool_call of the assistant message is answered by exactly one tool
// result message — including the calls that are skipped because the run is
// ending (canceled ctx, abort, guard-requested abort). A run may end here,
// but the conversation outlives it: an unanswered tool_call_id makes the next
// request invalid (OpenAI-compatible endpoints answer it with HTTP 400,
// "An assistant message with 'tool_calls' must be followed by tool messages").
func (a *BaseAgent) executeToolCalls(ctx context.Context, lastMsg types.Message, callback StreamCallback) {
	// Execute tools. Pre-dispatched results (computed while the stream
	// was still active) are used when available; otherwise tools run
	// synchronously here.
	for i := 0; i < len(lastMsg.ToolCalls); i++ {
		tc := lastMsg.ToolCalls[i]

		if a.abort.Load() || ctx.Err() != nil {
			// Aborted or canceled: stop executing tools. The run's context is
			// honored even for tools that ignore it (they receive it too).
			a.recordSkippedToolResults(lastMsg.ToolCalls[i:], callback, skippedToolResultCanceled)
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
			// The guard wants the run over: this call and everything after it
			// must still be answered before the run ends.
			a.abort.Store(true)
			a.recordSkippedToolResults(lastMsg.ToolCalls[i:], callback, skippedToolResultAborted)
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
		// allow_multiple rides along: callbacks implementing MultiAskCallback get
		// the flag (multi-select UIs); everyone else keeps the plain flow and the
		// flag is dropped.
		if askTool, ok := tool.(*tools.AskFollowupQuestionTool); ok {
			allowMultiple := false
			var askInput struct {
				AllowMultiple bool `json:"allow_multiple"`
			}
			if err := json.Unmarshal(tc.Input, &askInput); err == nil {
				allowMultiple = askInput.AllowMultiple
			}
			multi, supportsMulti := callback.(provider.MultiAskCallback)
			askTool.SetHandler(func(question string, options []string) (string, error) {
				if supportsMulti {
					return multi.AskFollowupQuestionMulti(question, options, allowMultiple)
				}
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
			// Executing synchronously now — surface the state change.
			callback.OnToolCallUpdate(ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: string(tc.Input),
			}, "running")
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

// skippedToolResultCanceled / skippedToolResultAborted are the tool results
// recorded for a tool call that never ran because the run ended around it
// (canceled context or an abort, respectively).
const (
	skippedToolResultCanceled = "[NOT EXECUTED] The run was canceled before this tool call was executed."
	skippedToolResultAborted  = "[NOT EXECUTED] The run was aborted before this tool call was executed."
)

// recordSkippedToolResults answers every tool call the loop will not execute
// with a placeholder tool result, keeping the transcript well-formed.
//
// An assistant message carrying tool_calls must be followed by one tool
// message per tool_call_id. Those calls were already announced to the host
// (OnToolCallStart fires when the call arrives in the stream), so each
// placeholder also completes the callback pair; without it a run that ends
// mid-batch leaves tool calls dangling forever in the UI.
func (a *BaseAgent) recordSkippedToolResults(calls []types.ToolCall, callback StreamCallback, reason string) {
	for _, tc := range calls {
		a.conversation.AddMessage(types.Message{
			Role:       types.RoleTool,
			ToolCallID: tc.ID,
			Content:    reason,
		})
		if callback != nil {
			callback.OnToolCallComplete(ToolCall{
				ID:    tc.ID,
				Name:  tc.Name,
				Input: string(tc.Input),
			}, reason)
		}
	}
}
