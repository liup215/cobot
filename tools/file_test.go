package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestReadFileTool_DefaultChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	var lines []string
	for i := 1; i <= 1500; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	content := strings.Join(lines, "\n")
	_ = os.WriteFile(path, []byte(content), 0644)

	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lines 1-1000") {
		t.Errorf("expected default chunk header, got: %s", result)
	}
	if !strings.Contains(result, "line1") || !strings.Contains(result, "line1000") {
		t.Errorf("expected first and last line of chunk, got: %s", result)
	}
	if strings.Contains(result, "line1001") {
		t.Errorf("expected line 1001 to be excluded, got: %s", result)
	}
	if !strings.Contains(result, "line_number=1001") {
		t.Errorf("expected continuation hint, got: %s", result)
	}
}

func TestReadFileTool_LineNumberChunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	var lines []string
	for i := 1; i <= 300; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	content := strings.Join(lines, "\n")
	_ = os.WriteFile(path, []byte(content), 0644)

	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]interface{}{
		"path":        path,
		"line_number": 50,
	})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lines 50-300") {
		t.Errorf("expected chunk header, got: %s", result)
	}
	if !strings.Contains(result, "line50") || !strings.Contains(result, "line300") {
		t.Errorf("expected lines 50 and 300 in output, got: %s", result)
	}
	if strings.Contains(result, "line49") {
		t.Errorf("expected line 49 to be excluded, got: %s", result)
	}
}

func TestReadFileTool_LargeChunkTruncated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// One enormous line exceeding the 100KB chunk cap.
	big := strings.Repeat("x", 110*1024)
	_ = os.WriteFile(path, []byte(big), 0644)

	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]string{"path": path})

	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Chunk truncated") {
		t.Errorf("expected chunk truncation message, got: %s", result)
	}
}

func TestReadFileTool_LimitParam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "l.txt")
	var lines []string
	for i := 1; i <= 3000; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)

	tool := NewReadFileTool()

	// limit=1000 reads 1000 lines starting at 1.
	input, _ := json.Marshal(map[string]interface{}{"path": path, "limit": 1000})
	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lines 1-1000") || !strings.Contains(result, "line1000") {
		t.Errorf("expected lines 1-1000 (header: %.60s), line1000 present: %v", result, strings.Contains(result, "line1000"))
	}
	if strings.Contains(result, "line1001\n") {
		t.Errorf("expected line 1001 excluded")
	}

	// limit is clamped to 2000.
	input, _ = json.Marshal(map[string]interface{}{"path": path, "limit": 99999})
	result, err = tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lines 1-2000") {
		t.Errorf("expected clamp to 2000 lines, got: %s", result[:60])
	}

	// line_number beyond EOF is a clear error with the total.
	input, _ = json.Marshal(map[string]interface{}{"path": path, "line_number": 5000})
	_, err = tool.Execute(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "5000") || !strings.Contains(err.Error(), "3000") {
		t.Errorf("expected beyond-EOF error with totals, got: %v", err)
	}
}

func TestReadFileTool_EmptyAndImage(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(empty, []byte{}, 0644)
	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]string{"path": empty})
	result, err := tool.Execute(context.Background(), input)
	if err != nil || !strings.Contains(result, "Empty file") {
		t.Errorf("expected empty-file notice, got: %q, err=%v", result, err)
	}

	img := filepath.Join(dir, "pic.png")
	_ = os.WriteFile(img, []byte{0x89, 'P', 'N', 'G', 0x00, 0x01}, 0644)
	input, _ = json.Marshal(map[string]string{"path": img})
	result, err = tool.Execute(context.Background(), input)
	if err != nil || !strings.Contains(result, "Image file") || !strings.Contains(result, "png") {
		t.Errorf("expected image metadata notice, got: %q, err=%v", result, err)
	}
	if strings.Contains(result, "\x89PNG") {
		t.Errorf("binary content must not leak into output")
	}
}

func TestReadFileTool_TildeExpansion(t *testing.T) {
	home, _ := os.UserHomeDir()
	if expandPath("~") != home {
		t.Errorf("expandPath(~) = %q, want %q", expandPath("~"), home)
	}
	if got := expandPath("~/foo/bar"); got != filepath.Join(home, "foo", "bar") {
		t.Errorf("expandPath(~/foo/bar) = %q", got)
	}
	if got := expandPath("/tmp/x"); got != "/tmp/x" {
		t.Errorf("non-tilde path must be untouched, got %q", got)
	}
}

func TestReadFileTool_RelativePathResolvesAbs(t *testing.T) {
	dir := t.TempDir()
	// chdir into temp dir and use a bare relative filename.
	old, _ := os.Getwd()
	defer os.Chdir(old)
	_ = os.Chdir(dir)

	_ = os.WriteFile(filepath.Join(dir, "rel.txt"), []byte("hello"), 0644)
	tool := NewReadFileTool()
	input, _ := json.Marshal(map[string]string{"path": "rel.txt"})
	result, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, filepath.Join(dir, "rel.txt")) {
		t.Errorf("expected absolute path in prefix, got: %s", result)
	}
}
