package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Command execution is always foreground: the tool blocks until the command
// exits or the timeout elapses, then reports stdout, stderr and the exit code.
// There is no shell job control here — a command that backgrounds itself (e.g.
// with "&") may keep running after the tool returns, so long-running servers
// should be started by the host instead of through this tool.

const (
	// DefaultCommandTimeoutSeconds is the timeout applied when the input does
	// not set one. Long enough for a build or a test run, short enough that a
	// hung command cannot stall a whole turn.
	DefaultCommandTimeoutSeconds = 120

	// MaxCommandOutputBytes caps each of stdout and stderr in the tool result.
	// The head (3/4) and the tail (1/4) of the stream are kept, because build
	// errors live at the end and startup banners at the beginning.
	MaxCommandOutputBytes = 32 * 1024
)

// ExecuteCommandTool executes a CLI command
type ExecuteCommandTool struct {
	BaseTool
}

// ExecuteCommandInput represents the input for execute_command tool
type ExecuteCommandInput struct {
	Command          string `json:"command"`
	RequiresApproval bool   `json:"requires_approval"`
	Cwd              string `json:"cwd,omitempty"`
	Timeout          int    `json:"timeout,omitempty"`
}

// ExecuteCommandOutput represents the output of execute_command tool
type ExecuteCommandOutput struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Duration int64  `json:"duration_ms"`
}

// NewExecuteCommandTool creates a new execute_command tool
func NewExecuteCommandTool() *ExecuteCommandTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The command to execute"
			},
			"requires_approval": {
				"type": "boolean",
				"description": "Whether this command requires user approval",
				"default": false
			},
			"cwd": {
				"type": "string",
				"description": "Working directory for the command (optional)"
			},
			"timeout": {
				"type": "integer",
				"description": "Timeout in seconds (default: 120)",
				"default": 120
			}
		},
		"required": ["command"]
	}`)

	return &ExecuteCommandTool{
		BaseTool: BaseTool{
			name: "execute_command",
			description: fmt.Sprintf(
				"Execute a CLI command in the foreground and wait for it to finish "+
					"(default timeout %ds, configurable per call). Stdout and stderr are reported "+
					"separately and each is capped at %dKB. Use this for system operations, builds and "+
					"tests; do not use it to start long-running servers. Commands that modify the "+
					"system require approval by default.",
				DefaultCommandTimeoutSeconds, MaxCommandOutputBytes/1024),
			inputSchema: schema,
		},
	}
}

// shellCommand returns the shell invocation used to run command on the given
// operating system: "cmd /C" on Windows and "sh -c" everywhere else. Pinned by
// TestShellCommandSelection so a platform change is a deliberate decision.
func shellCommand(goos, command string) (name string, args []string) {
	if goos == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

// Execute runs the command
func (t *ExecuteCommandTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ExecuteCommandInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	// Set default timeout
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeoutSeconds
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// Prepare command through the pinned shell selection.
	name, args := shellCommand(runtime.GOOS, req.Command)
	cmd := exec.CommandContext(ctx, name, args...)
	configureProcessKill(cmd)

	// Set working directory if specified
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = "."
	}

	// Set environment
	cmd.Env = os.Environ()

	// Capture stdout and stderr separately so the result can tell them apart.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start).Milliseconds()

	result := ExecuteCommandOutput{
		Stdout:   truncateCommandOutput(stdout.String()),
		Stderr:   truncateCommandOutput(stderr.String()),
		Duration: duration,
	}

	// Get exit code
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	// Handle errors
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result.Stderr = joinNonEmpty(fmt.Sprintf("Command timed out after %d seconds", timeout), result.Stderr)
		result.ExitCode = -1
	case runErr != nil:
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Stderr = joinNonEmpty(runErr.Error(), result.Stderr)
			result.ExitCode = -1
		}
	}

	// Format output
	var outputBuilder strings.Builder
	outputBuilder.WriteString(fmt.Sprintf("Command: %s\n", req.Command))
	outputBuilder.WriteString(fmt.Sprintf("Exit code: %d\n", result.ExitCode))
	outputBuilder.WriteString(fmt.Sprintf("Duration: %dms\n", result.Duration))
	outputBuilder.WriteString("\n")

	if result.Stdout != "" {
		outputBuilder.WriteString(fmt.Sprintf("Output:\n%s\n", result.Stdout))
	}

	if result.Stderr != "" {
		outputBuilder.WriteString(fmt.Sprintf("\nErrors:\n%s\n", result.Stderr))
	}

	return outputBuilder.String(), nil
}

// configureProcessKill makes a timed-out command actually stop: on Windows the
// shell (cmd.exe) does not forward a kill to its child, so the whole process
// tree is terminated instead; elsewhere the default context kill is enough.
// WaitDelay additionally bounds the wait in case a grandchild inherited the
// output pipes.
func configureProcessKill(cmd *exec.Cmd) {
	if runtime.GOOS == "windows" {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return os.ErrProcessDone
			}
			return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		}
	}
	cmd.WaitDelay = 3 * time.Second
}

// joinNonEmpty joins parts with "; ", skipping empty ones.
func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return strings.Join(kept, "; ")
}

// truncateCommandOutput caps output at MaxCommandOutputBytes, keeping the head
// and the tail of the stream with an omission marker in between.
func truncateCommandOutput(s string) string {
	const marker = "\n[... output truncated ...]\n"
	if len(s) <= MaxCommandOutputBytes {
		return s
	}
	head := MaxCommandOutputBytes * 3 / 4
	tail := MaxCommandOutputBytes - head - len(marker)
	if tail < 0 {
		tail = 0
	}
	return s[:head] + marker + s[len(s)-tail:]
}

// IsDestructiveCommand checks if a command might be destructive
func IsDestructiveCommand(command string) bool {
	destructivePatterns := []string{
		"rm ", "del ", "rmdir ", "rd ",
		"mv ", "move ", "ren ",
		"cp -r", "xcopy /s",
		"git reset --hard",
		"git clean -f",
		"docker rm", "docker rmi",
		"kubectl delete",
	}

	lowerCmd := strings.ToLower(command)
	for _, pattern := range destructivePatterns {
		if strings.Contains(lowerCmd, pattern) {
			return true
		}
	}
	return false
}

// IsNetworkCommand checks if a command involves network operations
func IsNetworkCommand(command string) bool {
	networkPatterns := []string{
		"curl", "wget", "http", "https://",
		"git clone", "git fetch", "git pull",
		"npm install", "pip install", "go get",
		"docker pull", "docker push",
	}

	lowerCmd := strings.ToLower(command)
	for _, pattern := range networkPatterns {
		if strings.Contains(lowerCmd, pattern) {
			return true
		}
	}
	return false
}
