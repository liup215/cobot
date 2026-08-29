// Package prompts builds the agent system prompt (role, sections, tool
// descriptions), ported from gline internal/prompts/system.go.
package prompts

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// DefaultAgentName is the agent identity used in the system prompt when the
// caller does not supply one.
const DefaultAgentName = "cobot"

// GetSystemPrompt returns the appropriate system prompt for the given mode.
// agentName is the agent identity announced in the role section; empty
// falls back to DefaultAgentName. If customRules is non-empty, it is
// appended after the tool section.
func GetSystemPrompt(mode string, tools []ToolDescription, customRules string, agentName string) string {
	if agentName == "" {
		agentName = DefaultAgentName
	}
	var prompt strings.Builder

	// Agent Role
	prompt.WriteString("You are " + agentName + ", a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices.")

	// ====
	prompt.WriteString("\n\n====\n\n")

	// TOOL USE
	prompt.WriteString(getToolUseSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// EDITING FILES
	prompt.WriteString(getEditingFilesSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// ACT VS PLAN
	prompt.WriteString(getActVsPlanSection(mode))

	// ====
	prompt.WriteString("\n\n====\n\n")

	// CAPABILITIES
	prompt.WriteString(getCapabilitiesSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// RULES
	prompt.WriteString(getRulesSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// SYSTEM INFORMATION
	prompt.WriteString(getSystemInfoSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// OBJECTIVE
	prompt.WriteString(getObjectiveSection())

	// ====
	prompt.WriteString("\n\n====\n\n")

	// AVAILABLE TOOLS
	toolSection := buildToolSection(tools)
	prompt.WriteString(toolSection)

	// Append custom rules if provided
	if customRules != "" {
		prompt.WriteString("\n\n# Custom Rules\n\n")
		prompt.WriteString(customRules)
	}

	return prompt.String()
}

// getToolUseSection returns the tool use section with formatting, guidelines, and examples.
func getToolUseSection() string {
	return `TOOL USE

You have access to tools. Use them step by step. After each tool use, wait for the result before proceeding.

# Format

Use native tool_calls when available. If native calls are not supported, use XML tags:

<tool_name>
<parameter>value</parameter>
</tool_name>

Example:
<read_file>
<path>src/main.go</path>
</read_file>`
}

// getEditingFilesSection returns the editing files section (concise).
func getEditingFilesSection() string {
	return `EDITING FILES

- write_to_file: create new files or overwrite existing ones completely.
- replace_in_file: make targeted edits using exact search/replace.

Usage styles:
  • Single block:  path + search + replace (one change)
  • Multi-block:   path + replacements array [{"search": "...", "replace": "..."}, ...]
    → Preferred when editing the same file multiple times.

Error handling:
  • If a search fails, the tool returns the NEAREST MATCH found with a similarity score.
  • Re-read the file and copy the EXACT text including indentation.
  • Ensure no hidden characters differ (tabs vs spaces).`
}

// getActVsPlanSection returns the Act vs Plan mode section.
func getActVsPlanSection(mode string) string {
	return `ACT MODE V.S. PLAN MODE

- ACT MODE: use tools to complete tasks. End with attempt_completion.
- PLAN MODE: use plan_mode_respond to present plans and gather feedback.`
}

// getCapabilitiesSection returns the capabilities section.
func getCapabilitiesSection() string {
	return `CAPABILITIES

- You have access to tools that let you execute CLI commands on the user's computer, list files, view source code definitions, regex search, read and edit files, and ask follow-up questions. These tools help you effectively accomplish a wide range of tasks, such as writing code, making edits or improvements to existing files, understanding the current state of a project, performing system operations, and much more.
- When you initially gives you a task, a recursive list of all filepaths in the current working directory ('` + getCWD() + `') will be included in environment_details. This provides an overview of the project's file structure, offering key insights into the project from directory/file names (how developers conceptualize and organize their code) and file extensions (the language used). This can also guide decision-making on which files to explore further. If you need to further explore directories such as outside the current working directory, you can use the list_files tool. If you pass 'true' for the recursive parameter, it will list files recursively. Otherwise, it will list files at the top level, which is better suited for generic directories where you don't necessarily need the nested structure, like the Desktop.
- You can use search_files to perform regex searches across files in a specified directory, outputting context-rich results that include surrounding lines. This is particularly useful for understanding code patterns, finding specific implementations, or identifying areas that need refactoring.
- You can use the list_code_definition_names tool to get an overview of source code definitions for all files at the top level of a specified directory. This can be particularly useful when you need to understand the broader context and relationships between certain parts of the code. You may need to call this tool multiple times to understand various parts of the codebase related to the task.
    - For example, when asked to make edits or improvements you might analyze the file structure in the initial environment_details to get an overview of the project, then use list_code_definition_names to get further insight using source code definitions for files located in relevant directories, then read_file to examine the contents of relevant files, analyze the code and suggest improvements or make necessary edits, then use the replace_in_file tool to implement changes. If you refactored code that could affect other parts of the codebase, you could use search_files to ensure you update other files as needed.
- You can use the execute_command tool to run commands on the user's computer whenever you feel it can help accomplish the user's task. When you need to execute a CLI command, you must provide a clear explanation of what the command does. Prefer to execute complex CLI commands over creating executable scripts, since they are more flexible and easier to run. Prefer non-interactive commands when possible: use flags to disable pagers (e.g., '--no-pager'), auto-confirm prompts (e.g., '-y' when safe), provide input via flags/arguments rather than stdin, suppress interactive behavior, etc. For commands that may fail, consider redirecting stderr to stdout (e.g., 'command 2>&1') so you can see error messages in the output. For long-running commands, the user may keep them running in the background and you will be kept updated on their status along the way. Each command you execute is run in a new terminal instance.`
}

// getRulesSection returns the rules section.
func getRulesSection() string {
	return `RULES

- Working directory: ` + getCWD() + ` (cannot cd elsewhere).
- Use correct path; no ~ or $HOME.
- For outside cwd: cd /dir && cmd.
- End with attempt_completion. Do NOT keep calling tools after finishing.
- NEVER call same tool with same params twice.`
}

// getObjectiveSection returns the objective section.
func getObjectiveSection() string {
	return `OBJECTIVE

Break task into steps, work sequentially, use tools one at a time.`
}

// getSystemInfoSection returns the system information section.
func getSystemInfoSection() string {
	goVersion := runtime.Version()
	osName := runtime.GOOS
	osArch := runtime.GOARCH
	cwd := getCWD()

	shell := "bash"
	if osName == "windows" {
		shell = "PowerShell"
	}

	homeDir, _ := os.UserHomeDir()

	return fmt.Sprintf(`SYSTEM INFORMATION

Operating System: %s (%s)
Default Shell: %s
Home Directory: %s
Current Working Directory: %s
Go Version: %s`, osName, osArch, shell, homeDir, cwd, goVersion)
}

// getCWD returns the current working directory.
func getCWD() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

// ToolDescription describes a tool for the system prompt
type ToolDescription struct {
	Name        string
	Description string
	InputSchema string
}

// buildToolSection builds the tool descriptions section with complete input schema.
// This follows the LtEdu Desktop Agent style for better tool understanding.
func buildToolSection(tools []ToolDescription) string {
	if len(tools) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("AVAILABLE TOOLS:\n\n")
	builder.WriteString("The following tools are available to you. Use these tools to accomplish the user's request:\n\n")

	for _, tool := range tools {
		builder.WriteString(fmt.Sprintf("## %s\n", tool.Name))
		builder.WriteString(fmt.Sprintf("%s\n", tool.Description))

		// Include full input schema if available (LtEdu style)
		if len(tool.InputSchema) > 0 && tool.InputSchema != "{}" {
			builder.WriteString(fmt.Sprintf("Input Schema:\n%s\n\n", tool.InputSchema))
		} else {
			builder.WriteString("\n")
		}
	}

	builder.WriteString("\n")
	builder.WriteString("---\n")
	builder.WriteString(`ALTERNATIVE TOOL USE FORMAT:
`)
	builder.WriteString(`If native tool calls are not supported, you can use XML-style tags: <tool_name>{"key":"value"}</tool_name>.\n`)
	builder.WriteString(`---\n`)

	return builder.String()
}
