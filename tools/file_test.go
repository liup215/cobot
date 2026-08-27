package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceInFileTool_SingleBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	_ = os.WriteFile(path, []byte("hello world\nfoo bar\n"), 0644)

	tool := NewReplaceInFileTool()
	input, _ := json.Marshal(map[string]string{
		"path":    path,
		"search":  "foo bar",
		"replace": "baz qux",
	})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Block 1: replaced") {
		t.Errorf("expected block success message, got: %s", result)
	}

	content, _ := os.ReadFile(path)
	if string(content) != "hello world\nbaz qux\n" {
		t.Errorf("unexpected content: %q", string(content))
	}
}

func TestReplaceInFileTool_MultiBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	_ = os.WriteFile(path, []byte("alpha beta\ngamma delta\nepsilon zeta\n"), 0644)

	tool := NewReplaceInFileTool()
	input, _ := json.Marshal(map[string]interface{}{
		"path": path,
		"replacements": []map[string]string{
			{"search": "alpha beta", "replace": "ONE TWO"},
			{"search": "epsilon zeta", "replace": "THREE FOUR"},
		},
	})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Changes (2 replacements)") {
		t.Errorf("expected 2 replacements summary, got: %s", result)
	}

	content, _ := os.ReadFile(path)
	expected := "ONE TWO\ngamma delta\nTHREE FOUR\n"
	if string(content) != expected {
		t.Errorf("unexpected content: %q, want: %q", string(content), expected)
	}
}

func TestReplaceInFileTool_NotFoundFeedback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	_ = os.WriteFile(path, []byte("the quick brown fox jumps over the lazy dog\n"), 0644)

	tool := NewReplaceInFileTool()
	input, _ := json.Marshal(map[string]string{
		"path":    path,
		"search":  "the slow green fox",
		"replace": "replaced",
	})

	_, err := tool.Execute(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for missing search text")
	}
	msg := err.Error()
	if !strings.Contains(msg, "search content not found") {
		t.Errorf("expected 'search content not found' in error, got: %s", msg)
	}
	if !strings.Contains(msg, "TROUBLESHOOTING") {
		t.Errorf("expected TROUBLESHOOTING steps in error, got: %s", msg)
	}
	if !strings.Contains(msg, "EXACTLY") {
		t.Errorf("expected 'EXACTLY' hint in error, got: %s", msg)
	}
}

func TestNormalizeWhitespace(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello\t\tworld", "hello world"},
		{"a  b\nc\r\nd", "a b c d"},
		{"nochange", "nochange"},
	}
	for _, tc := range tests {
		got := normalizeWhitespace(tc.input)
		if got != tc.expected {
			t.Errorf("normalizeWhitespace(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestFindNearestMatch(t *testing.T) {
	content := "func hello() {\n\treturn 42\n}\n"
	search := "func goodbye() {"
	m := findNearestMatch(content, search)
	if m.Score == 0 {
		t.Error("expected non-zero similarity score")
	}
	if !strings.HasPrefix(m.Text, "func ") {
		t.Errorf("expected nearest match to start with 'func ', got: %q", m.Text)
	}
}

func TestJaccardSimilarity(t *testing.T) {
	if jaccardSimilarity(map[string]int{"ab": 1}, map[string]int{"ab": 1}) != 1.0 {
		t.Error("identical sets should score 1.0")
	}
	if jaccardSimilarity(map[string]int{"ab": 1}, map[string]int{"xy": 1}) != 0.0 {
		t.Error("disjoint sets should score 0.0")
	}
}

func TestComputeDiff(t *testing.T) {
	oldC := "a\nb\nc\n"
	newC := "a\nB\nc\nd\n"
	diff := computeDiff(oldC, newC)
	if !strings.Contains(diff, "-b") {
		t.Error("expected removed line 'b'")
	}
	if !strings.Contains(diff, "+B") {
		t.Error("expected added line 'B'")
	}
	if !strings.Contains(diff, "+d") {
		t.Error("expected added line 'd'")
	}
}

func TestReadFileTool_LineRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	content := "line1\nline2\nline3\nline4\nline5\n"
	_ = os.WriteFile(path, []byte(content), 0644)

	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]interface{}{
		"path":       path,
		"start_line": 2,
		"end_line":   4,
	})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lines 2-4") {
		t.Errorf("expected line range header, got: %s", result)
	}
	if !strings.Contains(result, "line2") || !strings.Contains(result, "line4") {
		t.Errorf("expected lines 2 and 4 in output, got: %s", result)
	}
	if strings.Contains(result, "line1") || strings.Contains(result, "line5") {
		t.Errorf("expected lines 1 and 5 to be excluded, got: %s", result)
	}
}

func TestReadFileTool_LargeFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	big := strings.Repeat("x", 110*1024)
	_ = os.WriteFile(path, []byte(big), 0644)

	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "File too large") {
		t.Errorf("expected 'File too large' guard, got: %s", result)
	}
	if !strings.Contains(result, "search_files") {
		t.Errorf("expected guidance to use search_files, got: %s", result)
	}
}
