// Package loop implements the agent execution loop: it drives a Provider,
// executes tools through a Registry, persists via Store, and emits events
// through StreamCallback. Extracted and refactored from gline
// internal/agent/agent.go into three cooperating files: agent.go (main
// loop), stream.go (stream processing and pre-dispatch), toolexec.go
// (tool execution).
package loop

import (
	"fmt"
	"log/slog"
)

func logDebugf(format string, args ...any) { slog.Debug(fmt.Sprintf(format, args...)) }
func logInfof(format string, args ...any)  { slog.Info(fmt.Sprintf(format, args...)) }
func logWarnf(format string, args ...any)  { slog.Warn(fmt.Sprintf(format, args...)) }
func logErrorf(format string, args ...any) { slog.Error(fmt.Sprintf(format, args...)) }
