package loop

import (
	"context"
	"fmt"
	"strings"

	"github.com/liup215/cobot/prompts"
	"github.com/liup215/cobot/tools"
	"github.com/liup215/cobot/types"
)

// processStream handles the streaming response from the LLM. run receives the
// per-run accounting (turn usage); it is owned by the calling goroutine.
//
// The read is cancellable: when ctx is done the remaining stream is abandoned
// (drained in the background so the provider's pump goroutine can finish) and
// the interrupted turn is not recorded — the caller reports the wrapped
// context error.
func (a *BaseAgent) processStream(ctx context.Context, streamChan <-chan StreamChunk, callback StreamCallback, run *runStats) error {
	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []ToolCall

	// Usage rides on the final chunk of a stream as the cumulative total for
	// that turn. Only the last report is kept, so a provider that repeats a
	// cumulative report cannot double-count; it is folded into the run once,
	// after the stream ends.
	var usageSeen bool
	var usageIn, usageOut int

streamLoop:
	for {
		select {
		case <-ctx.Done():
			// The run's context is canceled: stop reading. The rest of the
			// stream is drained in the background so the provider's pump
			// goroutine — which may block sending into an unbuffered channel
			// — can finish and release the connection.
			go drainStream(streamChan)
			return ctx.Err()
		case chunk, ok := <-streamChan:
			if !ok {
				// Provider closed the stream (some providers close without a
				// Done chunk); treat it as the end of the turn.
				break streamLoop
			}

			if chunk.Error != nil {
				return chunk.Error
			}

			logDebugf("processStream chunk: content=%d reasoning=%d toolCall=%v done=%v",
				len(chunk.Content), len(chunk.ReasoningContent), chunk.ToolCall != nil, chunk.Done)

			// Accumulate real token usage from the API whenever available
			if chunk.Usage.TotalTokens > 0 {
				a.conversation.AddActualTokens(chunk.Usage.InputTokens, chunk.Usage.OutputTokens)
				usageSeen = true
				usageIn, usageOut = chunk.Usage.InputTokens, chunk.Usage.OutputTokens
			}

			if chunk.Done {
				break streamLoop
			}

			// Handle content (actual assistant reply text)
			if chunk.Content != "" {
				content.WriteString(chunk.Content)
				callback.OnContent(chunk.Content)
			}

			// Handle reasoning content (internal/model thinking).
			// Cline keeps this strictly separate from assistant text and exposes
			// it via a dedicated callback so the UI can choose to show it in a
			// collapsible panel or hide it completely.  We never mix reasoning
			// into OnContent.
			if chunk.ReasoningContent != "" {
				reasoning.WriteString(chunk.ReasoningContent)
				callback.OnReasoning(chunk.ReasoningContent)
			}

			// Handle tool call
			if chunk.ToolCall != nil {
				if chunk.IsPartial {
					// Partial tool calls from provider are already accumulated
					// Provider sends copies, so we don't need to track state here
					// Just ignore partials in processStream
				} else {
					// Complete tool call received
					tc := *chunk.ToolCall
					toolCalls = append(toolCalls, tc)

					// Notify UI that a tool call has been detected in the stream.
					callback.OnToolCallStart(tc)

					// Track in pending list (used by tests / diagnostics).
					a.pendingToolCallsMu.Lock()
					a.pendingToolCalls = append(a.pendingToolCalls, tc)
					a.pendingToolCallsMu.Unlock()

					// Mark this call ID as pre-dispatched so the main loop skips
					// calling OnToolCallStart a second time.
					a.preDispatchedIDsMu.Lock()
					if a.preDispatchedIDs == nil {
						a.preDispatchedIDs = make(map[string]bool)
					}
					a.preDispatchedIDs[tc.ID] = true
					a.preDispatchedIDsMu.Unlock()

					// Pre-dispatch: launch tool execution in the background so it
					// can overlap with the remaining SSE stream. The wait group
					// lets RunWithCallback await the goroutine on exit.
					a.preDispatchWG.Add(1)
					go func() {
						defer a.preDispatchWG.Done()
						a.preDispatchToolCall(ctx, tc)
					}()
				}
			}
		}
	}

	if usageSeen {
		run.addUsage(usageIn, usageOut)
	}

	// Build the final content strings after the stream is complete.
	fullContent := content.String()

	// Fallback: if no native tool_calls were received but the content contains
	// XML-style tool calls (<tool_name>...params...</tool_name>), parse them.
	if len(toolCalls) == 0 {
		var toolNames []string
		for _, t := range a.toolRegistry.GetAll() {
			toolNames = append(toolNames, t.Name())
		}
		parsedXML := tools.ParseXMLToolCalls(fullContent, toolNames)
		if len(parsedXML) > 0 {
			logInfof("Fallback: parsed %d XML tool calls from assistant content", len(parsedXML))
			for _, ptc := range parsedXML {
				toolCalls = append(toolCalls, ToolCall{
					ID:    ptc.ID,
					Name:  ptc.Name,
					Input: string(ptc.Input),
				})
			}
		}
	}

	// Convert accumulated tool calls to types.ToolCall
	var typesToolCalls []types.ToolCall
	for _, tc := range toolCalls {
		typesToolCalls = append(typesToolCalls, types.ToolCall{
			ID:    tc.ID,
			Name:  tc.Name,
			Input: []byte(tc.Input),
		})
	}

	// Add assistant message to conversation, including any accumulated reasoning content.
	// Tool calls are stored in the ToolCalls field; the TUI renders them via OnToolCallStart/Complete callbacks.
	a.conversation.AddMessage(types.Message{
		Role:             types.RoleAssistant,
		Content:          fullContent,
		ReasoningContent: reasoning.String(),
		ToolCalls:        typesToolCalls,
	})

	// Record what this turn delivered while it is fresh: run-scoped result
	// assembly reads this capture, not the (compaction-prone) transcript.
	run.recordAssistantTurn(fullContent, typesToolCalls)

	return nil
}

// drainStream consumes the remaining chunks of an abandoned stream until the
// provider closes it. Providers deliver into a channel and may block sending,
// so draining lets their pump goroutine finish and release the connection
// after a canceled run stopped reading.
func drainStream(streamChan <-chan StreamChunk) {
	for range streamChan {
	}
}

// preDispatchToolCall executes a tool in the background while the SSE stream
// is still ongoing and stores the result so the main loop can reuse it.
func (a *BaseAgent) preDispatchToolCall(ctx context.Context, tc ToolCall) {
	if a.abort.Load() || ctx.Err() != nil {
		// Aborted or canceled: do not start side work for a run that is over.
		return
	}

	// ToolGuard gate (pre-dispatch path). On denial the guard result is
	// recorded so the main loop reuses it instead of re-running the guard
	// decision and executing the tool.
	allow, denyReason, abort := a.applyGuard(ctx, GuardedCall{Name: tc.Name, Input: tc.Input})
	if abort {
		return
	}
	if !allow {
		a.recordDeniedPreDispatch(tc.ID, denyReason)
		return
	}

	// Validate before executing.
	if !a.toolRegistry.IsAllowed(string(a.mode), tc.Name) {
		a.recordPreDispatch(tc.ID, "", fmt.Errorf("tool %s is not allowed in %s mode", tc.Name, a.mode))
		return
	}

	tool, err := a.toolRegistry.Get(tc.Name)
	if err != nil {
		a.recordPreDispatch(tc.ID, "", fmt.Errorf("tool '%s' not found: %v", tc.Name, err))
		return
	}

	// Skip pre-dispatch for ask_followup_question because it needs the
	// callback handler injected by the main loop.
	if _, isAsk := tool.(*tools.AskFollowupQuestionTool); isAsk {
		return
	}

	// Skip pre-dispatch for tools with side effects (writes, DB updates,
	// command execution, browser automation) to avoid race conditions and
	// duplicate actions. Browser must not background-run due to resource cost.
	switch tc.Name {
	case "kb_ingest", "write_to_file", "replace_in_file", "execute_command", "memory_note", "browser_copy":
		return
	}

	res, execErr := tool.Execute(ctx, []byte(tc.Input))
	if execErr != nil {
		res = fmt.Sprintf("Error: %v", execErr)
	}
	a.recordPreDispatch(tc.ID, res, execErr)
}

// recordPreDispatch stores the outcome of a pre-dispatched tool call.
func (a *BaseAgent) recordPreDispatch(callID string, result string, execErr error) {
	a.preDispatchedResultsMu.Lock()
	defer a.preDispatchedResultsMu.Unlock()
	if a.preDispatchedResults == nil {
		a.preDispatchedResults = make(map[string]preDispatchedResult)
	}
	a.preDispatchedResults[callID] = preDispatchedResult{result: result, err: execErr}
}

// takePreDispatchResult retrieves and removes a pre-dispatched result for the
// given call ID. If no pre-dispatch was recorded it returns ok=false so the
// caller falls back to normal execution.
func (a *BaseAgent) takePreDispatchResult(callID string) (preDispatchedResult, bool) {
	a.preDispatchedResultsMu.Lock()
	defer a.preDispatchedResultsMu.Unlock()
	if a.preDispatchedResults == nil {
		return preDispatchedResult{}, false
	}
	r, ok := a.preDispatchedResults[callID]
	if ok {
		delete(a.preDispatchedResults, callID)
	}
	return r, ok
}

// convertTools converts internal tool definitions to provider format
func convertTools(toolsList []tools.Tool) []ToolDefinition {
	defs := make([]ToolDefinition, len(toolsList))
	for i, t := range toolsList {
		defs[i] = ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		}
	}
	return defs
}

// filterPlanModeTools filters out act-only tools for plan mode
func filterPlanModeTools(tools []prompts.ToolDescription) []prompts.ToolDescription {
	actOnlyTools := map[string]bool{
		"write_to_file":   true,
		"replace_in_file": true,
		"execute_command": true,
	}

	var filtered []prompts.ToolDescription
	for _, tool := range tools {
		if !actOnlyTools[tool.Name] {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}
