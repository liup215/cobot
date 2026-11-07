package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liup215/cobot/shell"
)

// ─── run ──────────────────────────────────────────────────────────────────

// TestRunTool_DefaultTimeout pins the 120s default documented on the tool
// description.
func TestRunTool_DefaultTimeout(t *testing.T) {
	if shell.DefaultTimeout != 120*time.Second {
		t.Fatalf("shell.DefaultTimeout = %s, want 120s", shell.DefaultTimeout)
	}
	if !strings.Contains(NewRunTool().Description(), "Default timeout 120s") {
		t.Error("the tool description must document the default timeout")
	}
}

// TestExecuteCommandTool_SeparatesStreams pins the stdout/stderr split and the
// result framing (command, exit code, duration, Output/Errors sections).
func TestExecuteCommandTool_SeparatesStreams(t *testing.T) {
	var cmd string
	if shell.Resolve().IsBash || runtime.GOOS != "windows" {
		cmd = `echo out-line; echo err-line 1>&2`
	} else {
		cmd = `echo out-line & echo err-line 1>&2`
	}

	out, err := runCommand(t, map[string]any{"command": cmd})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "out-line") {
		t.Errorf("stdout content missing: %s", out)
	}
	if !strings.Contains(out, "err-line") {
		t.Errorf("stderr content missing: %s", out)
	}
	if !strings.Contains(out, "Exit code: 0") {
		t.Errorf("exit code not reported: %s", out)
	}
	if !strings.Contains(out, "Duration:") {
		t.Errorf("duration not reported: %s", out)
	}
}

// TestExecuteCommandTool_NonZeroExit pins the failure path: a failing command
// is reported with its exit code, not turned into a tool error.
func TestExecuteCommandTool_NonZeroExit(t *testing.T) {
	// `exit 7` works under bash/sh (-c) AND cmd (/C) — the resolved shell
	// executes it either way.
	cmd := `exit 7`

	out, err := runCommand(t, map[string]any{"command": cmd})
	if err != nil {
		t.Fatalf("tool should not fail on a non-zero exit: %v", err)
	}
	if !strings.Contains(out, "Exit code: 7") {
		t.Errorf("exit code 7 not reported: %s", out)
	}
}

// TestExecuteCommandTool_Timeout pins the timeout semantics: the command is
// killed, the result reports a timeout and no tool error.
func TestExecuteCommandTool_Timeout(t *testing.T) {
	var cmd string
	if shell.Resolve().IsBash || runtime.GOOS != "windows" {
		cmd = `sleep 30`
	} else {
		cmd = `ping -n 30 127.0.0.1 >nul`
	}

	out, err := runCommand(t, map[string]any{"command": cmd, "timeout": 1})
	if err != nil {
		t.Fatalf("a timeout is a reported result, not an error: %v", err)
	}
	if !strings.Contains(out, "timed out after 1 seconds") {
		t.Errorf("timeout not reported: %s", out)
	}
	if !strings.Contains(out, "Exit code: -1") {
		t.Errorf("timed-out command should report exit code -1: %s", out)
	}
}

// TestExecuteCommandTool_Cwd pins the cwd option.
func TestExecuteCommandTool_Cwd(t *testing.T) {
	var cmd string
	switch runtime.GOOS {
	case "windows":
		cmd = `cd`
	default:
		cmd = `pwd`
	}

	out, err := runCommand(t, map[string]any{"command": cmd, "cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "Exit code: 0") == false {
		t.Errorf("command failed: %s", out)
	}
}

// TestExecuteCommandTool_RequiresCommand guards the empty-command input.
func TestExecuteCommandTool_RequiresCommand(t *testing.T) {
	tool := NewRunTool()
	input, _ := json.Marshal(map[string]any{"command": ""})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Error("expected an error for an empty command")
	}
}

// runCommand executes run with the given input and returns the tool result.
func runCommand(t *testing.T, in map[string]any) (string, error) {
	t.Helper()
	input, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return NewRunTool().Execute(context.Background(), input)
}

// ─── write / edit ─────────────────────────────────────────

// TestWriteFileAtomicPreservesMode pins the atomic-write helper: the previous
// file mode survives an overwrite and a failed write leaves the content alone.
func TestWriteFileAtomicPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("original"), 0640); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := writeFileAtomic(path, []byte("updated")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "updated" {
		t.Errorf("content = %q, want updated", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
		t.Errorf("mode = %v, want the original 0640", info.Mode().Perm())
	}

	// No leftover temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// TestWriteFileAtomicFailureLeavesOriginal pins the rollback semantics: a write
// that cannot complete must not damage the target.
func TestWriteFileAtomicFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("original"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Force the rename to fail by targeting a directory path.
	if err := writeFileAtomic(dir, []byte("new")); err == nil {
		t.Fatal("expected an error writing over a directory")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original" {
		t.Errorf("original content damaged: %q (%v)", data, err)
	}
}

// TestReplaceInFileIsAllOrNothing pins the multi-block rollback semantics: one
// unmatched block leaves the file untouched.
func TestReplaceInFileIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	input, _ := json.Marshal(map[string]any{
		"path": path,
		"replacements": []map[string]string{
			{"search": "gamma", "replace": "GAMMA"},
			{"search": "does-not-exist", "replace": "x"},
		},
	})
	if _, err := NewReplaceInFileTool().Execute(context.Background(), input); err == nil {
		t.Fatal("expected an error for the unmatched block")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Errorf("file modified although a block failed: %q (%v)", data, err)
	}
}

// ─── ls ──────────────────────────────────────────────────────────────

// TestListFilesEntryCap pins the entry cap and its truncation notice.
func TestListFilesEntryCap(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%02d.txt", i)), nil, 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	var b strings.Builder
	if err := listFilesUpTo(dir, &b, 3); err != nil {
		t.Fatalf("listFilesUpTo: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "[Listing truncated: 3 of 6 entries shown.") {
		t.Errorf("cap notice missing: %s", out)
	}
	if strings.Contains(out, "file-03") {
		t.Error("entries beyond the cap must not be listed")
	}
	if !strings.Contains(out, "file-00") || !strings.Contains(out, "file-02") {
		t.Errorf("entries below the cap missing: %s", out)
	}

	// Without a cap the full listing comes back, sorted by name.
	b.Reset()
	if err := listFilesUpTo(dir, &b, 0); err != nil {
		t.Fatalf("listFilesUpTo: %v", err)
	}
	if strings.Contains(b.String(), "truncated") {
		t.Errorf("uncapped listing reported a truncation: %s", b.String())
	}
	if strings.Index(b.String(), "file-00") > strings.Index(b.String(), "file-05") {
		t.Errorf("listing not sorted by name: %s", b.String())
	}
}

// TestListFilesToolIsSorted pins that the tool output is deterministic.
func TestListFilesToolIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		_ = os.WriteFile(filepath.Join(dir, name), nil, 0644)
	}

	input, _ := json.Marshal(map[string]string{"path": dir})
	tool := NewLsTool()
	first, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Error("ls output is not deterministic")
	}
	if !strings.Contains(first, "Contents of") {
		t.Errorf("listing missing its header: %s", first)
	}
}

// ─── grep / list_code_definition_names ──────────────────────────────

// TestSearchFilesIsDeterministic pins the sort stability: two identical
// searches return byte-identical output.
func TestSearchFilesIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		var b strings.Builder
		for i := 1; i <= 20; i++ {
			fmt.Fprintf(&b, "// line %d needle here\nfunc f%d() {}\n", i, i)
		}
		_ = os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0644)
	}

	tool := NewGrepTool()
	input, _ := json.Marshal(map[string]string{"path": dir, "regex": "needle"})
	first, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Error("grep output is not deterministic across runs")
	}
	if !strings.Contains(first, "Line 1, Col 1") {
		t.Errorf("results not ordered by line: %.200s", first)
	}
}

// TestSearchFilesTruncationNotice pins the cap behavior: results are capped,
// the total is reported truthfully and a notice says what was dropped.
func TestSearchFilesTruncationNotice(t *testing.T) {
	// Force the pure-Go fallback: rg kills its scan early at the cap, so its
	// totals are inherently approximate — honest totals are the Go path's
	// contract (rg behaviour is covered in search_rg_test).
	rgPathOverride = "rg-does-not-exist-for-this-test"
	t.Cleanup(func() { rgPathOverride = "" })

	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= maxSearchResults+60; i++ {
		fmt.Fprintf(&b, "needle-%d\n", i)
	}
	_ = os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0644)

	tool := NewGrepTool()
	input, _ := json.Marshal(map[string]string{"path": dir, "regex": "needle"})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, fmt.Sprintf("Found %d matches", maxSearchResults+60)) {
		t.Errorf("the true match count must be reported: %.200s", out)
	}
	if !strings.Contains(out, "[Results truncated:") {
		t.Errorf("truncation notice missing: %s", tail(out, 300))
	}
}

// TestFormatCodeDefinitionsDeterministic pins the deterministic file ordering
// (map iteration order must not leak into the output).
func TestFormatCodeDefinitionsDeterministic(t *testing.T) {
	defs := []CodeDefinition{
		{Name: "bFunc", Type: "function", Line: 10, File: "b.go", Language: "go"},
		{Name: "A", Type: "struct", Line: 2, File: "a.go", Language: "go"},
		{Name: "aFunc", Type: "function", Line: 8, File: "a.go", Language: "go"},
	}

	first := formatCodeDefinitions(defs)
	for i := 0; i < 20; i++ {
		shuffled := []CodeDefinition{defs[2], defs[0], defs[1]}
		if got := formatCodeDefinitions(shuffled); got != first {
			t.Fatalf("formatCodeDefinitions is not deterministic:\n%s\n---\n%s", first, got)
		}
	}

	if strings.Index(first, "a.go") > strings.Index(first, "b.go") {
		t.Errorf("files not in sorted order:\n%s", first)
	}
	aSection := first[strings.Index(first, "a.go"):strings.Index(first, "b.go")]
	if strings.Index(aSection, "A (struct) at line 2") > strings.Index(aSection, "aFunc (function) at line 8") {
		t.Errorf("definitions within a file not in line order:\n%s", first)
	}
}

// TestSearchCacheConcurrency exercises the shared cache under contention so a
// -race run covers the locking.
func TestSearchCacheConcurrency(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte("needle\n"), 0644)

	tool := NewGrepTool()
	input, _ := json.Marshal(map[string]string{"path": dir, "regex": "needle"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tool.Execute(context.Background(), input); err != nil {
				t.Errorf("search failed: %v", err)
			}
		}()
	}
	wg.Wait()
}

// ─── interaction tools ───────────────────────────────────────────────────────

// TestAttemptCompletionRequiresResult pins the terminal-tool input contract.
func TestAttemptCompletionRequiresResult(t *testing.T) {
	tool := NewAttemptCompletionTool()
	input, _ := json.Marshal(map[string]string{"result": ""})
	if _, err := tool.Execute(context.Background(), input); err == nil {
		t.Error("expected an error for an empty result")
	}

	input, _ = json.Marshal(map[string]string{"result": "done", "command": "go test ./..."})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "done") || !strings.Contains(out, "go test ./...") {
		t.Errorf("completion summary incomplete: %s", out)
	}
}

// TestPlanModeRespondEchoes pins the plan-mode terminal tool.
func TestPlanModeRespondEchoes(t *testing.T) {
	tool := NewPlanModeRespondTool()
	input, _ := json.Marshal(map[string]string{"response": "the plan"})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "the plan" {
		t.Errorf("plan_mode_respond = %q, want the response echoed", out)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"response":""}`)); err == nil {
		t.Error("expected an error for an empty response")
	}
}

// TestAskFollowupHandlerAndOptionMapping pins the handler contract and the
// "answer by option number" convenience.
func TestAskFollowupHandlerAndOptionMapping(t *testing.T) {
	tool := NewAskFollowupQuestionTool()
	var gotQuestion string
	var gotOptions []string
	tool.SetHandler(func(q string, o []string) (string, error) {
		gotQuestion, gotOptions = q, o
		return "2", nil // the user typed the option number
	})

	input, _ := json.Marshal(map[string]any{
		"question": "which approach?",
		"options":  []string{"rewrite", "patch"},
	})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuestion != "which approach?" || len(gotOptions) != 2 {
		t.Errorf("handler received q=%q options=%v", gotQuestion, gotOptions)
	}
	if !strings.Contains(out, "User answered: patch") {
		t.Errorf("option number 2 should map to \"patch\": %s", out)
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"question":""}`)); err == nil {
		t.Error("expected an error for an empty question")
	}
}

// TestInteractionToolsAreRegistered pins the default tool set that the system
// prompt documents.
func TestInteractionToolsAreRegistered(t *testing.T) {
	reg := InitDefaultRegistry()
	for _, name := range []string{
		"read", "write", "edit", "ls",
		"run", "grep", "glob",
		"attempt_completion", "ask_followup_question", "plan_mode_respond",
	} {
		if _, err := reg.Get(name); err != nil {
			t.Errorf("default registry is missing %s: %v", name, err)
		}
	}
}

// tail returns the last n characters of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// guard against unused imports in exotic configurations
var (
	_ = time.Second
	_ = runtime.GOOS
	_ = os.Getpid
	_ = sync.Mutex{}
)

// allow_multiple: numeric picks are resolved per item ("1,3" → "a, c") and
// single-choice keeps the old one-index mapping.
func TestAskFollowupMultiAnswerMapping(t *testing.T) {
	tool := NewAskFollowupQuestionTool()
	tool.SetHandler(func(q string, o []string) (string, error) { return "1,3", nil })

	input, _ := json.Marshal(map[string]any{
		"question":       "pick several",
		"options":        []string{"a", "b", "c"},
		"allow_multiple": true,
	})
	out, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("multi execute: %v", err)
	}
	if out != "User answered: a, c" {
		t.Errorf("multi mapping = %q, want %q", out, "User answered: a, c")
	}

	// Single-choice: a comma list is NOT split (old contract preserved).
	tool.SetHandler(func(q string, o []string) (string, error) { return "1,3", nil })
	input, _ = json.Marshal(map[string]any{
		"question": "pick one",
		"options":  []string{"a", "b", "c"},
	})
	out, err = tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("single execute: %v", err)
	}
	if out != "User answered: 1,3" {
		t.Errorf("single mapping = %q, want the raw answer", out)
	}
}
