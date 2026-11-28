package tools

// InitDefaultRegistry initializes a registry with the cobot built-in tool
// surface — gline-aligned names (2026-11 tool-surface alignment):
// read / ls / write / edit / grep / glob / run plus the three interaction
// tools, plus the web triple (web_search / fetch_content / get_search_content).
// Every tool the system prompt tells the model about must be registered here —
// loop's prompt-guard test enforces this.
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

	// Web access - allowed in both modes. web_search reads the network and
	// stores its answer; get_search_content reads that store back. Neither
	// writes to the filesystem or executes anything, so plan mode keeps both:
	// "what does the current spec say" is a planning question.
	registry.Register(&ToolInfo{
		Tool:                 NewWebSearchTool(),
		Category:             CategoryNetwork,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	registry.Register(&ToolInfo{
		Tool:                 NewGetSearchContentTool(),
		Category:             CategoryNetwork,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// fetch_content reads ONE page in full: HTML → markdown, PDF/office → text.
	// It takes a URL from the MODEL, so it carries the private-address gate
	// (websearch/ssrf.go) that web_search does not need — see that file for why.
	registry.Register(&ToolInfo{
		Tool:                 NewWebFetchTool(),
		Category:             CategoryNetwork,
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
