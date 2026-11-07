package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupFindFilesTree(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(tmpDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main.go", "package main\n")
	write("src/util/helper.go", "package util\n")
	write("README.md", "doc\n")
	write("node_modules/lib.js", "junk\n")
	return tmpDir
}

func TestFindFilesFDIntegration(t *testing.T) {
	if fdBinary() == "" {
		t.Skip("fd not installed")
	}
	tmpDir := setupFindFilesTree(t)

	tool := NewGlobTool()

	// Glob filter: only .go files, node_modules excluded.
	input, _ := json.Marshal(GlobInput{Path: tmpDir, Pattern: "*.go"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output, "Found 2 files") {
		t.Errorf("expected 2 .go files, got:\n%s", output)
	}
	if strings.Contains(output, "node_modules") || strings.Contains(output, "README.md") {
		t.Errorf("excluded entries leaked:\n%s", output)
	}

	// Empty pattern lists all files (node_modules still excluded).
	input2, _ := json.Marshal(GlobInput{Path: tmpDir})
	output2, err := tool.Execute(context.Background(), input2)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output2, "Found 3 files") {
		t.Errorf("expected 3 files, got:\n%s", output2)
	}
}

func TestFindFilesFallsBackWhenFdBroken(t *testing.T) {
	fdPathOverride = "fd-definitely-not-a-real-binary"
	defer func() { fdPathOverride = "" }()

	tmpDir := setupFindFilesTree(t)

	tool := NewGlobTool()
	input, _ := json.Marshal(GlobInput{Path: tmpDir, Pattern: "*.go"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("fallback execute failed: %v", err)
	}
	if !strings.Contains(output, "main.go") || !strings.Contains(output, "helper.go") {
		t.Errorf("expected .go files via pure-Go fallback, got:\n%s", output)
	}
	if strings.Contains(output, "node_modules") {
		t.Errorf("node_modules leaked in fallback:\n%s", output)
	}
}

func TestFindFilesNoMatches(t *testing.T) {
	fdPathOverride = "fd-definitely-not-a-real-binary"
	defer func() { fdPathOverride = "" }()

	tmpDir := setupFindFilesTree(t)

	tool := NewGlobTool()
	input, _ := json.Marshal(GlobInput{Path: tmpDir, Pattern: "*.zig"})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output, "No files found") {
		t.Errorf("expected no matches message, got:\n%s", output)
	}
}

func TestFindFilesPathValidation(t *testing.T) {
	tool := NewGlobTool()

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":""}`)); err == nil {
		t.Error("expected error for empty path")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"Z:/definitely/not/here"}`)); err == nil {
		t.Error("expected error for nonexistent path")
	}
}
