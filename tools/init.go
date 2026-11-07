package tools

// InitDefaultRegistry initializes a registry with the cobot built-in tool
// surface — gline-aligned names (2026-11 tool-surface alignment):
// read / ls / write / edit / grep / glob / run plus the three interaction
// tools. Every tool the system prompt tells the model about must be
// registered here — loop's prompt-guard test enforces this.
//
// list_code_definition_names stays implemented (search.go) but is NOT
// registered, mirroring gline's 2026-09-21 tool pruning; re-register it
// below if needed.
func InitDefaultRegistry() *Registry {
	registry := NewRegistry()

	// File operations - allowed in both modes (read-only in plan)
	registry.Register(&ToolInfo{
		Tool:                 NewReadFileTool(),
		Category:             CategoryFile,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	registry.Register(&ToolInfo{
		Tool:                 NewLsTool(),
		Category:             CategoryFile,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// File write operations - act mode only
	registry.Register(&ToolInfo{
		Tool:                 NewWriteFileTool(),
		Category:             CategoryFile,
		AllowedModes:         []string{"act"},
		RequiresConfirmation: true,
	})

	registry.Register(&ToolInfo{
		Tool:                 NewReplaceInFileTool(),
		Category:             CategoryFile,
		AllowedModes:         []string{"act"},
		RequiresConfirmation: true,
	})

	// Search operations - allowed in both modes
	registry.Register(&ToolInfo{
		Tool:                 NewGrepTool(),
		Category:             CategorySearch,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	registry.Register(&ToolInfo{
		Tool:                 NewGlobTool(),
		Category:             CategorySearch,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// Command execution - act mode only
	registry.Register(&ToolInfo{
		Tool:                 NewRunTool(),
		Category:             CategoryCommand,
		AllowedModes:         []string{"act"},
		RequiresConfirmation: true,
	})

	// User interaction - allowed in both modes
	registry.Register(&ToolInfo{
		Tool:                 NewAskFollowupQuestionTool(),
		Category:             CategoryInteraction,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// Plan mode response - plan mode only
	registry.Register(&ToolInfo{
		Tool:                 NewPlanModeRespondTool(),
		Category:             CategoryInteraction,
		AllowedModes:         []string{"plan"},
		RequiresConfirmation: false,
	})

	// Completion - allowed in both modes
	registry.Register(&ToolInfo{
		Tool:                 NewAttemptCompletionTool(),
		Category:             CategoryCompletion,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	return registry
}
