package tools

// command.go — the `run` tool: gline's refactored command tool aligned into
// cobot (2026-09→11), keeping cobot's execution hardening.
//
// What came from gline: the shell is RESOLVED at construction (bash
// preferred wherever it exists — incl. Git Bash on Windows, WSL stubs
// skipped) and the resolved syntax is baked into the description, because
// only the description tells the model whether to write POSIX `&&` or cmd
// `&`. What cobot keeps: execution delegates to the shared shell.Run
// executor — process-tree kill on timeout, separate stdout/stderr, 32KB
// head+tail truncation on rune boundaries, exit codes as results.
//
// Approval: hosts may inject an ApprovalHandler; destructive commands
// (shell.IsDestructiveCommand) or requires_approval ask it first and fail
// closed. A nil handler means no gate (gline's default: no confirmation).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/liup215/cobot/shell"
)

// ApprovalHandler asks the host's user to approve a command. It returns
// whether the command may run; an error (modal failed, run aborted) is
// treated as deny. ctx is the tool's run context so aborting the run
// unblocks a waiting approval prompt.
type ApprovalHandler func(ctx context.Context, command string) (bool, error)

// RunTool executes a shell command in the foreground.
type RunTool struct {
	BaseTool
	approve ApprovalHandler
}

// ExecuteCommandInput is the input for the run tool.
type ExecuteCommandInput struct {
	Command          string `json:"command"`
	Cwd              string `json:"cwd,omitempty"`
	Timeout          int    `json:"timeout,omitempty"`
	RequiresApproval bool   `json:"requires_approval,omitempty"`
}

// ExecuteCommandOutput is the structured result of a run (the formatted
// tool string is built from it).
type ExecuteCommandOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration int64  `json:"duration_ms"`
	TimedOut bool   `json:"timed_out"`
}

// NewRunTool creates the run tool with no approval gate.
func NewRunTool() *RunTool {
	return NewRunToolWithApproval(nil)
}

// NewRunToolWithApproval creates the run tool with an optional approval
// gate for destructive commands.
func NewRunToolWithApproval(approve ApprovalHandler) *RunTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The shell command to execute"
			},
			"cwd": {
				"type": "string",
				"description": "Working directory for the command (optional)"
			},
			"timeout": {
				"type": "integer",
				"description": "Timeout in SECONDS (default 120, max 600). Write 300 for five minutes, not 300000.",
				"default": 120
			},
			"requires_approval": {
				"type": "boolean",
				"description": "Set true for commands that modify the system — the user is asked before execution (destructive commands are always asked regardless)"
			}
		},
		"required": ["command"]
	}`)

	sh := shell.Resolve()
	var syntax string
	switch {
	case sh.IsBash:
		syntax = "Commands run under bash: POSIX syntax works (&& chains, $() substitution, $VAR variables, test -f)."
	case sh.Name() == "cmd":
		syntax = "This machine has no bash; commands run through cmd.exe. Write cmd syntax: & or a newline instead of &&, %VAR% instead of $VAR, if exist instead of test -f. Native tools (git, go, npm) work as usual."
	default:
		syntax = "Commands run under sh: POSIX syntax works (&& chains, $() substitution, $VAR variables)."
	}

	return &RunTool{
		BaseTool: BaseTool{
			name: "run",
			description: fmt.Sprintf(
				"Execute a shell command in the foreground and wait for it to finish. %s "+
					"Use it for builds, tests, git and other system operations. Do NOT use it to read files (use read) "+
					"or search code (use grep/glob). Default timeout %ds (max 600s, set per call, in SECONDS — "+
					"write 300 for five minutes, not 300000); a command that outlives its timeout is killed "+
					"(whole process tree) and reports the partial output with a timeout note. Stdout and stderr are "+
					"reported separately, each capped at %dKB (head + tail kept). A non-zero exit code is part of "+
					"the result, not an error. cwd runs the command in that directory. Do NOT use it to start "+
					"long-running servers. Destructive commands (rm/del, git reset --hard, docker rm, shutdown…) "+
					"and commands with requires_approval ask the user for approval before running.",
				syntax, int(shell.DefaultTimeout/time.Second), shell.MaxOutputBytes/1024),
			inputSchema: schema,
		},
		approve: approve,
	}
}

// SetApprovalHandler injects (or replaces) the approval gate.
func (t *RunTool) SetApprovalHandler(h ApprovalHandler) { t.approve = h }

// Execute runs the command (after approval when required) and formats the
// result.
func (t *RunTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ExecuteCommandInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.Command) == "" {
		return "", fmt.Errorf("command is required")
	}

	// Approval gate: model-declared (requires_approval) or heuristic
	// (destructive). Any failure to obtain an answer denies — fail closed.
	if req.RequiresApproval || shell.IsDestructiveCommand(req.Command) {
		if t.approve != nil {
			allowed, err := t.approve(ctx, req.Command)
			if err != nil {
				return "Command NOT executed: approval request failed (" + err.Error() + ").", nil
			}
			if !allowed {
				return "Command NOT executed: approval denied by the user.", nil
			}
		}
	}

	spec := shell.Spec{Command: req.Command, Cwd: req.Cwd}
	if req.Timeout > 0 {
		spec.Timeout = time.Duration(req.Timeout) * time.Second
	}
	res := shell.Run(ctx, spec)

	var b strings.Builder
	fmt.Fprintf(&b, "Command: %s\n", res.Command)
	fmt.Fprintf(&b, "Exit code: %d\n", res.ExitCode)
	fmt.Fprintf(&b, "Duration: %dms\n", res.DurationMs)
	if res.TimedOut {
		b.WriteString("Status: TIMED OUT — the process tree was killed\n")
	}
	if res.Stdout != "" {
		b.WriteString("\nOutput:\n")
		b.WriteString(res.Stdout)
		b.WriteString("\n")
	}
	if res.Stderr != "" {
		b.WriteString("\nErrors:\n")
		b.WriteString(res.Stderr)
		b.WriteString("\n")
	}
	return b.String(), nil
}
