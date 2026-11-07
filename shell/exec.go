// Package shell also provides the shared command executor used by the run
// tool AND by hosts that need to run commands without an LLM (builds,
// scripts, terminal panels). One executor, one contract:
//
//   - foreground only — Run blocks until the command exits or the timeout
//     elapses; there is no job control;
//   - per-call timeout (default 120s, capped at 600s) kills the WHOLE
//     process tree (taskkill /F /T on Windows, cmd.WaitDelay as backstop);
//   - stdout and stderr captured separately;
//   - each stream capped at 32KB keeping head (3/4) + tail (1/4) with an
//     omission marker, rune-boundary safe;
//   - non-zero exit codes are RESULTS, not errors — only spawn failures and
//     timeouts are exceptional, and even a timeout is reported in the result.
package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultTimeout is applied when Spec.Timeout is zero: long enough for a
	// build or test run, short enough that a hung command cannot stall a
	// whole agent turn.
	DefaultTimeout = 120 * time.Second

	// MaxTimeout caps per-call overrides.
	MaxTimeout = 600 * time.Second

	// MaxOutputBytes caps EACH of stdout and stderr in a result.
	MaxOutputBytes = 32 * 1024
)

// Spec describes one command to run.
type Spec struct {
	// Command is the full command line, interpreted by the resolved shell
	// (bash when available, otherwise cmd/sh — see Resolve).
	Command string
	// Cwd is the working directory; empty means the process's current
	// directory.
	Cwd string
	// Timeout overrides DefaultTimeout; values above MaxTimeout are clamped.
	Timeout time.Duration
}

// Result is the outcome of one command. ExitCode is -1 on timeout or spawn
// failure; TimedOut distinguishes the two.
type Result struct {
	Command    string `json:"command"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

// Run executes spec.Command under the resolved shell and returns its result.
// It never fails merely because the command exited non-zero.
func Run(ctx context.Context, spec Spec) Result {
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > MaxTimeout {
		timeout = MaxTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sh := Resolve()
	args := make([]string, 0, len(sh.Args)+1)
	args = append(args, sh.Args...)
	args = append(args, spec.Command)
	cmd := exec.CommandContext(ctx, sh.Path, args...)
	HideConsole(cmd)
	configureProcessKill(cmd)

	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = os.Environ()

	// stdout/stderr captured separately so the result can tell them apart.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	result := Result{
		Command:    spec.Command,
		Stdout:     truncateOutput(stdout.String()),
		Stderr:     truncateOutput(stderr.String()),
		DurationMs: time.Since(start).Milliseconds(),
		ExitCode:   0,
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result.TimedOut = true
		result.Stderr = joinNonEmpty(
			"Command timed out after "+strconv.Itoa(int(timeout/time.Second))+" seconds",
			result.Stderr)
		result.ExitCode = -1
	case runErr != nil:
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.Stderr = joinNonEmpty(runErr.Error(), result.Stderr)
			result.ExitCode = -1
		}
	}
	return result
}

// configureProcessKill makes a timed-out command actually stop: on Windows
// the shell does not forward a kill to its child, so the whole process tree
// is terminated instead; elsewhere the default context kill is enough.
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

// truncateOutput caps output at MaxOutputBytes, keeping the head (3/4) and
// the tail (1/4) with an omission marker in between — build errors live at
// the end, startup banners at the beginning. Cuts are moved to rune
// boundaries so multi-byte output (Chinese, emoji) is never split.
func truncateOutput(s string) string {
	const marker = "\n[... output truncated ...]\n"
	if len(s) <= MaxOutputBytes {
		return s
	}
	head := truncateUTF8Prefix(s, MaxOutputBytes*3/4)
	tail := truncateUTF8Suffix(s, MaxOutputBytes-len(head)-len(marker))
	return head + marker + tail
}

func truncateUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func truncateUTF8Suffix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// IsDestructiveCommand reports whether a command looks like it modifies or
// destroys state — hosts with an approval policy ask before running these.
// Deliberately conservative (a substring heuristic, not a parser): false
// negatives are acceptable because the model can also set requires_approval.
func IsDestructiveCommand(command string) bool {
	destructivePatterns := []string{
		"rm ", "rm-", "del ", "rmdir", "rd ",
		"mv ", "move ", "ren ",
		"cp -r", "xcopy /s",
		"format ", "diskpart",
		"git reset --hard",
		"git clean",
		"git push --force", "git push -f",
		"docker rm", "docker rmi", "docker system prune",
		"kubectl delete",
		"shutdown", "reboot",
		"mkfs", "> /dev/sd",
	}
	lower := strings.ToLower(command)
	for _, pattern := range destructivePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// IsNetworkCommand reports whether a command touches the network (kept for
// hosts that want a stricter policy than destructive-only approval).
func IsNetworkCommand(command string) bool {
	networkPatterns := []string{
		"curl", "wget", "http://", "https://",
		"git clone", "git fetch", "git pull", "git push",
		"npm install", "pip install", "go get", "go mod download",
		"docker pull", "docker push",
	}
	lower := strings.ToLower(command)
	for _, pattern := range networkPatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// ErrEmptyCommand is returned by hosts for a blank command line.
var ErrEmptyCommand = fmt.Errorf("command is required")
