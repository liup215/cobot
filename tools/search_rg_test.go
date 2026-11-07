package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rgFixture builds a minimal `rg --json` stream for one file:
//
//	  1: package main        (context)
//	> 2: hello 世界 func      (match, multibyte before match)
//	  3: func foo()          (match)
//	  4: }                   (context)
func rgFixture() string {
	var b strings.Builder
	b.WriteString(`{"type":"begin","data":{"path":{"text":"a.go"}}}` + "\n")
	b.WriteString(`{"type":"context","data":{"path":{"text":"a.go"},"lines":{"text":"package main\n"},"line_number":1}}` + "\n")
	b.WriteString(`{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"hello 世界 func\n"},"line_number":2,"submatches":[{"match":{"text":"func"},"start":12,"end":16}]}}` + "\n")
	b.WriteString(`{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"func foo()\n"},"line_number":3,"submatches":[{"match":{"text":"func"},"start":0,"end":4}]}}` + "\n")
	b.WriteString(`{"type":"context","data":{"path":{"text":"a.go"},"lines":{"text":"}\n"},"line_number":4}}` + "\n")
	b.WriteString(`{"type":"end","data":{"path":{"text":"a.go"}}}` + "\n")
	return b.String()
}

func TestParseRipgrepOutput(t *testing.T) {
	out := parseRipgrepOutput(rgFixture())

	if out.TotalFiles != 1 {
		t.Errorf("TotalFiles = %d, want 1", out.TotalFiles)
	}
	if out.TotalMatches != 2 {
		t.Errorf("TotalMatches = %d, want 2", out.TotalMatches)
	}
	if len(out.Results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(out.Results))
	}

	r1, r2 := out.Results[0], out.Results[1]

	// Match on line 2 with multibyte chars before it: byte offset 12 is
	// rune offset 8 ("hello 世界"), so the 1-based rune column is 9.
	if r1.Line != 2 || r1.Column != 9 || r1.Match != "func" || r1.Path != "a.go" {
		t.Errorf("r1 = %+v, want line 2 col 9 match func in a.go", r1)
	}
	if r1.ContextLine != 2 { // match sits 2nd within its context window (lines 1..4)
		t.Errorf("r1.ContextLine = %d, want 2", r1.ContextLine)
	}

	// Context block format must match the pure-Go fallback exactly.
	wantCtx1 := "  1: package main\n> 2: hello 世界 func\n  3: func foo()\n  4: }"
	if r1.Context != wantCtx1 {
		t.Errorf("r1.Context =\n%s\nwant:\n%s", r1.Context, wantCtx1)
	}

	if r2.Line != 3 || r2.Column != 1 || r2.Match != "func" {
		t.Errorf("r2 = %+v, want line 3 col 1 match func", r2)
	}
}

func TestParseRipgrepOutputCRLFAndTruncation(t *testing.T) {
	// CRLF line text must not leak \r into context, and a stream cut off
	// before the end event must still flush.
	var b strings.Builder
	b.WriteString(`{"type":"begin","data":{"path":{"text":"w.go"}}}` + "\n")
	b.WriteString(`{"type":"match","data":{"path":{"text":"w.go"},"lines":{"text":"needle here\r\n"},"line_number":1,"submatches":[{"match":{"text":"needle"},"start":0,"end":6}]}}` + "\n")
	out := parseRipgrepOutput(b.String())

	if len(out.Results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(out.Results))
	}
	if strings.Contains(out.Results[0].Context, "\r") {
		t.Errorf("CR leaked into context: %q", out.Results[0].Context)
	}
}

func TestSearchFilesRipgrepIntegration(t *testing.T) {
	if rgBinary() == "" {
		t.Skip("rg not installed")
	}
	tmpDir := t.TempDir()
	f1 := filepath.Join(tmpDir, "a.go")
	if err := os.WriteFile(f1, []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n\nfunc foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Node modules dir without a .gitignore: rg must still skip it via the
	// explicit skip glob.
	nm := filepath.Join(tmpDir, "node_modules")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "junk.js"), []byte("func leak(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool()
	input, _ := json.Marshal(GrepInput{Path: tmpDir, Regex: "func"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output, "Found 2 matches") {
		t.Errorf("expected 2 func matches, got:\n%s", output)
	}
	if strings.Contains(output, "node_modules") {
		t.Error("node_modules should be excluded")
	}
	if !strings.Contains(output, "  1: package main") {
		t.Errorf("context block missing/incorrect:\n%s", output)
	}
}

func TestSearchFilesFallsBackWhenRgBroken(t *testing.T) {
	if rgBinary() == "" {
		t.Skip("rg installed; override would not change behavior")
	}
	rgPathOverride = "rg-definitely-not-a-real-binary"
	defer func() { rgPathOverride = "" }()

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "x.txt"), []byte("needle in haystack\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool()
	input, _ := json.Marshal(GrepInput{Path: tmpDir, Regex: "needle"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("fallback execute failed: %v", err)
	}
	if !strings.Contains(output, "needle") {
		t.Errorf("expected match via pure-Go fallback, got:\n%s", output)
	}
}

func TestSearchFilesRipgrepLargeFileSkipped(t *testing.T) {
	if rgBinary() == "" {
		t.Skip("rg not installed")
	}
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "big.bin"), make([]byte, maxFileSize+1), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewGrepTool()
	input, _ := json.Marshal(GrepInput{Path: tmpDir, Regex: "func"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if strings.Contains(output, "big.bin") {
		t.Error("oversized file should be skipped")
	}
	if !strings.Contains(output, "No matches") {
		t.Errorf("expected no matches, got:\n%s", output)
	}
}

func TestSearchFilesRipgrepEarlyStop(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not installed")
	}
	dir := t.TempDir()
	// 12 files x 60 matches = 720 submatches; the 500-result cap must stop
	// the parse at a file boundary (after ~9 files) instead of consuming
	// the whole stream.
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("needle%d here", i))
	}
	for f := 0; f < 12; f++ {
		name := filepath.Join(dir, fmt.Sprintf("f%02d.txt", f))
		if err := os.WriteFile(name, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			t.Fatal(err)
		}
	}

	out, ok, err := searchFilesRipgrep(context.Background(), dir, "needle", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected rg fast path")
	}
	// At the searchFilesRipgrep level the cap is applied by the parser's
	// early stop (Execute's post-cap is a no-op safety net): exactly 500.
	if len(out.Results) != maxSearchResults {
		t.Fatalf("expected exactly %d results from early stop, got %d", maxSearchResults, len(out.Results))
	}
	if out.TotalMatches != maxSearchResults {
		t.Fatalf("expected TotalMatches clamped to %d, got %d", maxSearchResults, out.TotalMatches)
	}
	// Sanity: first result of a file is line 1 with context.
	if out.Results[0].Line != 1 || out.Results[0].Context == "" {
		t.Fatalf("unexpected first result: %+v", out.Results[0])
	}
}
