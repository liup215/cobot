package shell

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRunEcho(t *testing.T) {
	res := Run(context.Background(), Spec{Command: "echo hello-shell"})
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0 (stderr: %q)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hello-shell") {
		t.Errorf("stdout = %q, want it to contain hello-shell", res.Stdout)
	}
	if res.TimedOut {
		t.Error("unexpected timeout")
	}
	if res.DurationMs < 0 {
		t.Errorf("duration = %d", res.DurationMs)
	}
}

func TestRunNonZeroExitIsResultNotError(t *testing.T) {
	res := Run(context.Background(), Spec{Command: "definitely_not_a_real_command_xyz123"})
	if res.ExitCode == 0 {
		t.Errorf("exit = 0, want non-zero (stdout %q stderr %q)", res.Stdout, res.Stderr)
	}
	if res.TimedOut {
		t.Error("must not be reported as a timeout")
	}
}

func TestRunTimeoutKillsAndReports(t *testing.T) {
	var cmd string
	if Resolve().IsBash || runtime.GOOS != "windows" {
		cmd = "sleep 30"
	} else {
		cmd = "ping -n 30 127.0.0.1 >nul"
	}
	start := time.Now()
	res := Run(context.Background(), Spec{Command: cmd, Timeout: time.Second})
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("want TimedOut, got exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1 on timeout", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "timed out") {
		t.Errorf("stderr = %q, want a timeout note", res.Stderr)
	}
	if elapsed > 20*time.Second {
		t.Errorf("took %s — process tree was not killed", elapsed)
	}
}

func TestRunCwd(t *testing.T) {
	dir := t.TempDir()
	// The resolver may pick bash even on Windows — ask the RESOLVED shell,
	// not the OS: pwd works under bash/sh, cd prints cwd under cmd.
	var cmd string
	if Resolve().IsBash {
		cmd = "pwd"
	} else {
		cmd = "cd"
	}
	res := Run(context.Background(), Spec{Command: cmd, Cwd: dir})
	want := dir
	if i := strings.LastIndexAny(want, `/\`); i >= 0 {
		want = want[i+1:]
	}
	if !strings.Contains(res.Stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", res.Stdout, want)
	}
}

func TestTruncateOutputHeadAndTail(t *testing.T) {
	block := strings.Repeat("0123456789", 4096) // 40KB
	out := truncateOutput(block)
	if len(out) > MaxOutputBytes {
		t.Errorf("len = %d, want <= %d", len(out), MaxOutputBytes)
	}
	if !strings.Contains(out, "[... output truncated ...]") {
		t.Error("missing truncation marker")
	}
	if !strings.HasPrefix(out, "0123456789") {
		t.Error("head lost")
	}
	if !strings.HasSuffix(out, "0123456789") {
		t.Error("tail lost")
	}
}

func TestTruncateOutputIsRuneSafe(t *testing.T) {
	block := strings.Repeat("中文字符测试", 4000)
	out := truncateOutput(block)
	if !utf8.ValidString(out) {
		t.Error("truncated output is not valid UTF-8")
	}
	if strings.Contains(out, "�") {
		t.Error("truncation produced replacement runes")
	}
}

func TestIsDestructiveCommand(t *testing.T) {
	destructive := []string{
		"rm -rf build/", "del notes.txt", "git reset --hard HEAD~3",
		"docker system prune -f", "shutdown /s /t 0",
	}
	innocuous := []string{
		"go test ./...", "echo hello", "dir /w", "git status", "npm run build",
	}
	for _, c := range destructive {
		if !IsDestructiveCommand(c) {
			t.Errorf("%q should be destructive", c)
		}
	}
	for _, c := range innocuous {
		if IsDestructiveCommand(c) {
			t.Errorf("%q should NOT be destructive", c)
		}
	}
}
