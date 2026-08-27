package provider

import (
	"fmt"
	"log/slog"
)

// Thin logging shims over log/slog, replacing gline's internal/log (zerolog).
// Kept as package-private helpers so migrated files keep their original
// call sites untouched.

func logDebugf(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...))
}

func logInfof(format string, args ...any) {
	slog.Info(fmt.Sprintf(format, args...))
}

func logErrorf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
}

func logDebug(args ...any) {
	slog.Debug(fmt.Sprint(args...))
}
