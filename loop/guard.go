package loop

import (
	"context"
	"errors"
)

// GuardedCall is the tool-call view passed to a ToolGuard. Input is the
// raw JSON arguments as a string, regardless of the internal ToolCall
// representation (provider.ToolCall carries a string; types.ToolCall a
// json.RawMessage) so a single guard implementation serves both the
// main-loop and the pre-dispatch execution paths.
type GuardedCall struct {
	Name  string
	Input string
}

// ToolGuard is the approval gate for tool calls (PreToolUse hook).
//
// A ToolGuard is consulted before every tool execution, on both execution
// paths (the main-loop path and the pre-dispatch path). Because a guarded
// call may be inspected twice, a ToolGuard MUST be side-effect free and
// idempotent.
//
// Decision semantics:
//   - allow=true            -> the call proceeds.
//   - allow=false           -> the call is denied; the model receives a tool
//     result of "[DENIED] <reason>" and the loop continues, so the model
//     can adapt (e.g. ask a different way or use another tool).
//   - err wrapping ErrAbortRun -> the entire run aborts immediately.
//   - any other non-nil err -> treated as a denial with the error text as
//     the reason (fail-closed).
type ToolGuard func(ctx context.Context, call GuardedCall) (allow bool, reason string, err error)

// ErrAbortRun is the sentinel error a ToolGuard may return to abort the
// entire run (not just deny one call). Wrap it with fmt.Errorf("%w", ...)
// if you need to add context.
var ErrAbortRun = errors.New("toolguard: abort run")

// applyGuard runs the configured ToolGuard (if any) and normalizes the
// outcome. allow=false carries the denial reason; abort=true means the
// guard returned ErrAbortRun and the caller must stop the run.
func (a *BaseAgent) applyGuard(ctx context.Context, call GuardedCall) (allow bool, reason string, abort bool) {
	if a.guard == nil {
		return true, "", false
	}
	gAllow, gReason, gErr := a.guard(ctx, call)
	if errors.Is(gErr, ErrAbortRun) {
		return false, "", true
	}
	if gErr != nil {
		// Fail closed: an unexpected guard error denies the call.
		return false, gErr.Error(), false
	}
	if gAllow {
		return true, "", false
	}
	return false, gReason, false
}

// recordDeniedPreDispatch stores a guard denial as the pre-dispatched
// result for callID so the main loop observes the denial without having
// to run the guard a second time before it can know the outcome.
func (a *BaseAgent) recordDeniedPreDispatch(callID, reason string) {
	a.preDispatchedResultsMu.Lock()
	defer a.preDispatchedResultsMu.Unlock()
	if a.preDispatchedResults == nil {
		a.preDispatchedResults = make(map[string]preDispatchedResult)
	}
	a.preDispatchedResults[callID] = preDispatchedResult{result: "[DENIED] " + reason, denied: true}
}
