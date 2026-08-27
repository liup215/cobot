package tools

// InitDefaultRegistry initializes a registry with the agentcore v1 built-in
// tool set (the file/search/code/command tools plus the three special
// interaction tools). Every tool the system prompt tells the model about
// must be registered here — loop's prompt-guard test enforces this.
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
		Tool:                 NewListFilesTool(),
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
		Tool:                 NewSearchFilesTool(),
		Category:             CategorySearch,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// Code structure inspection - allowed in both modes (read-only)
	// list_code_definition_names: referenced by the system prompt CAPABILITIES
	// section; read-only so it is also safe for stream pre-dispatch.
	registry.Register(&ToolInfo{
		Tool:                 NewListCodeDefinitionNamesTool(),
		Category:             CategoryCode,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
	})

	// Command execution - act mode only
	registry.Register(&ToolInfo{
		Tool:                 NewExecuteCommandTool(),
		Category:             CategoryCommand,
		AllowedModes:         []string{"act"},
		RequiresConfirmation: true,
	})

	// User interaction - allowed in both modes
	// ask_followup_question: skip both start & complete system messages;
	// the askQuestionMsg handler displays the question with styled options instead.
	registry.Register(&ToolInfo{
		Tool:                 NewAskFollowupQuestionTool(),
		Category:             CategoryInteraction,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
		Behavior: ToolBehavior{
			StartDisplayMode:    DisplaySkip,
			CompleteDisplayMode: DisplaySkip,
		},
	})

	// Plan mode response - plan mode only
	// plan_mode_respond: skip start; render the completed result as a full assistant message (markdown)
	registry.Register(&ToolInfo{
		Tool:                 NewPlanModeRespondTool(),
		Category:             CategoryInteraction,
		AllowedModes:         []string{"plan"},
		RequiresConfirmation: false,
		Behavior: ToolBehavior{
			StartDisplayMode:    DisplaySkip,
			CompleteDisplayMode: DisplayAssistant,
		},
	})

	// Completion - allowed in both modes
	// attempt_completion: show result as assistant message (markdown); skip duplicate complete message
	registry.Register(&ToolInfo{
		Tool:                 NewAttemptCompletionTool(),
		Category:             CategoryCompletion,
		AllowedModes:         []string{"plan", "act"},
		RequiresConfirmation: false,
		Behavior: ToolBehavior{
			StartDisplayMode:    DisplayAssistant,
			CompleteDisplayMode: DisplaySkip,
		},
	})

	return registry
}

// GetDefaultTools returns a list of all v1 built-in tools
func GetDefaultTools() []Tool {
	return []Tool{
		NewReadFileTool(),
		NewWriteFileTool(),
		NewReplaceInFileTool(),
		NewListFilesTool(),
		NewSearchFilesTool(),
		NewListCodeDefinitionNamesTool(),
		NewExecuteCommandTool(),
		NewAskFollowupQuestionTool(),
		NewPlanModeRespondTool(),
		NewAttemptCompletionTool(),
	}
}

// GetToolsForMode returns tools available for a specific mode
func GetToolsForMode(mode string) []Tool {
	allTools := GetDefaultTools()
	var filtered []Tool

	for _, tool := range allTools {
		// Check if tool is allowed in this mode
		// This is a simplified check - in production, use the registry
		switch tool.Name() {
		case "write_to_file", "replace_in_file", "execute_command":
			if mode == "act" {
				filtered = append(filtered, tool)
			}
		case "plan_mode_respond":
			if mode == "plan" {
				filtered = append(filtered, tool)
			}
		default:
			filtered = append(filtered, tool)
		}
	}

	return filtered
}

// IsToolAllowed checks if a tool is allowed in a specific mode
func IsToolAllowed(toolName string, mode string) bool {
	switch toolName {
	case "write_to_file", "replace_in_file", "execute_command":
		return mode == "act"
	case "plan_mode_respond":
		return mode == "plan"
	default:
		return true
	}
}
