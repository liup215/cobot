// Package prompts builds the agent system prompt (role, sections, tool
// descriptions), ported from gline internal/prompts/system.go.
package prompts

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/liup215/cobot/types"
)

// DefaultAgentName is the agent identity used in the system prompt when the
// caller does not supply one.
const DefaultAgentName = "cobot"

// GetSystemPrompt returns the appropriate system prompt for the given mode.
// agentName is the agent identity announced in the role section; empty
// falls back to DefaultAgentName. If customRules is non-empty, it is
// appended before the skill section. If skills is non-empty, a SKILLS
// section is added telling the LLM about available skills and the
// use_skill tool.
func GetSystemPrompt(mode string, tools []ToolDescription, customRules string, skills []types.SkillMeta, agentName string) string {
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

	// Append SKILLS section if skills are available
	if len(skills) > 0 {
		prompt.WriteString("\n\n# SKILLS\n\n")
		prompt.WriteString(buildSkillsSection(skills))
	}

	return prompt.String()
}

// buildSkillsSection generates the skills section for the system prompt.
func buildSkillsSection(skills []types.SkillMeta) string {
	var b strings.Builder
	b.WriteString("The following skills provide specialized instructions for specific tasks. When a user's request matches a skill description, use the use_skill tool to load and activate the skill.\n\n")
	b.WriteString("Available skills:\n")
	for _, s := range skills {
		b.WriteString(fmt.Sprintf("  - \"%s\": %s\n", s.Name, s.Description))
	}
	b.WriteString("\nTo use a skill:\n")
	b.WriteString("1. Match the user's request to a skill based on its description\n")
	b.WriteString("2. Call use_skill with the skill_name parameter set to the exact skill name\n")
	b.WriteString("3. Follow the instructions returned by the tool")
	return b.String()
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

// GetToolDescriptions returns descriptions for all built-in tools
func GetToolDescriptions() []ToolDescription {
	return []ToolDescription{
		{
			Name:        "read_file",
			Description: "Read the contents of a file at the specified path. Use this when you need to examine the contents of an existing file.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"The path of the file to read"}},"required":["path"] }`,
		},
		{
			Name:        "write_to_file",
			Description: "Write content to a file at the specified path. If the file exists, it will be overwritten. Use this when creating new files or completely rewriting existing files.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"The path of the file to write"},"content":{"type":"string","description":"The content to write to the file"}} ,"required":["path","content"] }`,
		},
		{
			Name:        "replace_in_file",
			Description: "Replace specific content in a file using exact search/replace. Supports single blocks or an array of replacements for multiple edits. Use this for targeted modifications to existing files.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"The path of the file to modify"},"search":{"type":"string","description":"Exact content to find (single block style)"},"replace":{"type":"string","description":"Content to replace with (single block style)"},"replacements":{"type":"array","description":"Array of replacement blocks. Preferred for multiple edits.","items":{"type":"object","properties":{"search":{"type":"string","description":"Exact content to find"},"replace":{"type":"string","description":"Content to replace with"}},"required":["search","replace"]}}} ,"required":["path"] }`,
		},
		{
			Name:        "list_files",
			Description: "List files and directories at the specified path. Use this to explore the file system.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"The path of the directory to list"},"recursive":{"type":"boolean","description":"List recursively"}} ,"required":["path"] }`,
		},
		{
			Name:        "search_files",
			Description: "Search for a regex pattern in files within a directory. Returns context-rich results with file paths, line numbers, and surrounding context.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"Directory to search"},"regex":{"type":"string","description":"Pattern to search for"},"file_pattern":{"type":"string","description":"Glob pattern to filter files"}} ,"required":["path","regex"] }`,
		},
		{
			Name:        "list_code_definition_names",
			Description: "List definition names (functions, classes, methods, etc.) in code files within a directory. Provides insights into codebase structure.",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string","description":"Directory to analyze"},"file_pattern":{"type":"string","description":"Glob pattern to filter files"}} ,"required":["path"] }`,
		},
		{
			Name:        "execute_command",
			Description: "Execute a CLI command on the system. Use this when you need to perform system operations or run specific commands.",
			InputSchema: `{"type":"object","properties":{"command":{"type":"string","description":"The command to execute"},"requires_approval":{"type":"boolean","description":"Whether this command requires user approval","default":false},"timeout":{"type":"integer","description":"Timeout in seconds"}} ,"required":["command"] }`,
		},
		{
			Name:        "ask_followup_question",
			Description: "Ask the user a question to gather clarifying information or make a decision. Use this when you need more information to complete a task.",
			InputSchema: `{"type":"object","properties":{"question":{"type":"string","description":"The question to ask"},"options":{"type":"array","items":{"type":"string"},"description":"Options for the user to choose from"}} ,"required":["question"] }`,
		},
		{
			Name:        "attempt_completion",
			Description: "Indicate that you have completed the task and provide a summary of what was accomplished. Use this when you have finished all required work.",
			InputSchema: `{"type":"object","properties":{"result":{"type":"string","description":"Summary of what was accomplished"},"command":{"type":"string","description":"Command to showcase the result"}} ,"required":["result"] }`,
		},
		{
			Name:        "use_subagents",
			Description: "Run up to four focused in-process subagents in parallel. Each subagent gets its own prompt and returns a comprehensive research result. Use this for broad exploration when reading many files would consume the main agent's context window. You do not need to launch multiple subagents every time; using one subagent is valid when it avoids unnecessary context usage for light discovery work.",
			InputSchema: `{"type":"object","properties":{"prompt_1":{"type":"string","description":"First subagent prompt."},"prompt_2":{"type":"string","description":"Optional second subagent prompt."},"prompt_3":{"type":"string","description":"Optional third subagent prompt."},"prompt_4":{"type":"string","description":"Optional fourth subagent prompt."}} ,"required":["prompt_1"] }`,
		},
		{
			Name:        "web_fetch",
			Description: "Fetch the content of a web page and return it as Markdown. Use this to read documentation, articles, or any publicly accessible web page. The content is automatically cleaned (ads, navbars removed) and converted to Markdown. Only http/https URLs are allowed.",
			InputSchema: `{"type":"object","properties":{"url":{"type":"string","description":"The URL of the web page to fetch"}},"required":["url"] }`,
		},
		{
			Name:        "browser_copy",
			Description: "Copy content from a web page using a headless browser. Use this when the page requires JavaScript to render (SPA apps like React/Vue), or when content is loaded dynamically after page load. Falls back gracefully if the browser is unavailable. This tool is more resource-intensive than web_fetch; prefer web_fetch for static pages. Only http/https URLs are allowed.",
			InputSchema: `{"type":"object","properties":{"url":{"type":"string","description":"The URL of the web page to copy from"},"wait_for":{"type":"string","description":"Optional CSS selector to wait for before extracting content (e.g., '.article-body')"},"scroll_down":{"type":"boolean","description":"Whether to scroll down to trigger lazy-loading","default":false},"headless":{"type":"boolean","description":"Run browser in headless mode (default true). Set false to show the browser window.","default":true}},"required":["url"] }`,
		},
	}
}

// GetPlanModeToolDescriptions returns tool descriptions for plan mode
func GetPlanModeToolDescriptions() []ToolDescription {
	allTools := GetToolDescriptions()
	var planTools []ToolDescription

	actOnlyTools := map[string]bool{
		"write_to_file":   true,
		"replace_in_file": true,
		"execute_command": true,
	}

	for _, tool := range allTools {
		if !actOnlyTools[tool.Name] {
			planTools = append(planTools, tool)
		}
	}

	return planTools
}

// GetActModeToolDescriptions returns tool descriptions for act mode
func GetActModeToolDescriptions() []ToolDescription {
	return GetToolDescriptions()
}

// FormatToolUseExample returns an example of how to use a tool
func FormatToolUseExample(toolName string, input map[string]interface{}) string {
	return fmt.Sprintf("Using tool: %s with input: %v", toolName, input)
}
