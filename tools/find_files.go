package tools

// GlobTool recursively finds files by name using a glob pattern.
//
// When fd (fd-find) is installed it handles the search — parallel traversal
// with .gitignore awareness, far faster than a Go walk on large repos. When
// fd is missing the tool falls back to the pure-Go findFiles walk shared
// with grep.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/liup215/cobot/shell"
)

// fdSkipDirs mirrors the pure-Go fallback's hardcoded skip list for repos
// without a .gitignore (fd already skips hidden entries and VCS ignores).
var fdSkipDirs = []string{"node_modules", "vendor", "__pycache__", "dist", "build", "target", "out"}

// GlobTool lists files recursively by name pattern.
type GlobTool struct {
	BaseTool
}

// GlobInput represents the input for glob tool.
type GlobInput struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern,omitempty"`
}

// NewGlobTool creates a new glob tool.
func NewGlobTool() *GlobTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The directory to search in"
			},
			"pattern": {
				"type": "string",
				"description": "Optional glob pattern to match file names (e.g., '*.go', '*config*'). Omit to list all files."
			}
		},
		"required": ["path"]
	}`)

	return &GlobTool{
		BaseTool: BaseTool{
			name:        "glob",
			description: "Find files recursively by name using a glob pattern (e.g. '*.go', '*config*'). Respects .gitignore and skips hidden/vendor directories. Returns up to 500 matching file paths. Use grep to search file CONTENTS.",
			inputSchema: schema,
		},
	}
}

// Execute finds files matching the pattern.
func (t *GlobTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req GlobInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	path := filepath.Clean(req.Path)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("path not found: %s", path)
		}
		return "", fmt.Errorf("failed to stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", path)
	}

	// Fast path: fd when installed. Falls back when fd is missing or fails
	// to run; cancellation is propagated instead.
	var files []string
	if out, ok, err := findFilesFD(ctx, path, req.Pattern); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		// fd broken: fall through to the pure-Go walk.
	} else if ok {
		files = out
	}

	if files == nil {
		files, err = findFiles(path, req.Pattern)
		if err != nil {
			return "", fmt.Errorf("failed to find files: %w", err)
		}
		sort.Strings(files)
		if len(files) > maxSearchResults {
			files = files[:maxSearchResults]
		}
	}

	return formatFindFiles(files), nil
}

// findFilesFD runs fd and returns matching file paths.
// It returns ok=false when fd is unavailable or failed, signaling the caller
// to fall back to the pure-Go implementation.
func findFilesFD(ctx context.Context, path, pattern string) ([]string, bool, error) {
	fd := fdBinary()
	if fd == "" {
		return nil, false, nil
	}

	args := []string{
		"--type", "f",
		"--max-results", strconv.Itoa(maxSearchResults),
		"--no-messages",
	}
	for _, d := range fdSkipDirs {
		args = append(args, "--exclude", d)
	}
	// fd: with --glob the PATTERN positional is interpreted as a glob;
	// --search-path separates the search root from the pattern.
	if pattern != "" {
		args = append(args, "--glob", pattern)
	} else {
		args = append(args, "--glob", "*")
	}
	args = append(args, "--search-path", path)

	cmd := exec.CommandContext(ctx, fd, args...)
	shell.HideConsole(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		return nil, false, fmt.Errorf("fd failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var files []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	sort.Strings(files)
	return files, true, nil
}

// formatFindFiles renders the tool's text output.
func formatFindFiles(files []string) string {
	if len(files) == 0 {
		return "No files found."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Found %d files:\n", len(files)))
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	if len(files) >= maxSearchResults {
		b.WriteString(fmt.Sprintf("(output capped at %d files — narrow the pattern for more specific results)\n", maxSearchResults))
	}
	return b.String()
}
