package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Read limits shared by read_file. Files above MaxReadFileBytes are returned
// truncated from the head with an explicit notice (never silently, never as a
// hard refusal), and a requested line range above MaxReadRangeBytes is
// truncated the same way.
const (
	// MaxReadFileBytes caps a whole-file read.
	MaxReadFileBytes = 64 * 1024

	// MaxReadRangeBytes caps a start_line/end_line range read.
	MaxReadRangeBytes = 100 * 1024
)

// ReadFileTool reads the contents of a file
type ReadFileTool struct {
	BaseTool
}

// ReadFileInput represents the input for read_file tool
type ReadFileInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// NewReadFileTool creates a new read_file tool
func NewReadFileTool() *ReadFileTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path of the file to read"
			},
			"start_line": {
				"type": "integer",
				"description": "Optional 1-based starting line number. Use this to read a specific range instead of the whole file."
			},
			"end_line": {
				"type": "integer",
				"description": "Optional 1-based ending line number (0 means read to end). Use together with start_line."
			}
		},
		"required": ["path"]
	}`)

	return &ReadFileTool{
		BaseTool: BaseTool{
			name: "read_file",
			description: fmt.Sprintf(
				"Read the contents of a file at the specified path. Whole-file reads are capped at "+
					"%dKB and line-range reads at %dKB; anything larger is returned truncated from the "+
					"head with a notice reporting the total size. Prefer search_files first to find the "+
					"relevant sections, then use start_line/end_line to read only that range.",
				MaxReadFileBytes/1024, MaxReadRangeBytes/1024),
			inputSchema: schema,
		},
	}
}

// Execute reads the file
func (t *ReadFileTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ReadFileInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Clean the path
	path := filepath.Clean(req.Path)

	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", path)
		}
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s", path)
	}

	// Read file
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// If a line range is requested, slice the content first.
	if req.StartLine > 0 || req.EndLine > 0 {
		sliced, start, end, err := sliceLines(content, req.StartLine, req.EndLine)
		if err != nil {
			return "", fmt.Errorf("invalid line range: %w", err)
		}
		// Even for ranges, enforce a byte cap so an enormous range cannot
		// explode the context. The cut backs off to a rune boundary so a
		// multi-byte character is never split in half.
		if len(sliced) > MaxReadRangeBytes {
			head := truncateUTF8Prefix(string(sliced), MaxReadRangeBytes)
			return fmt.Sprintf("%s\n\n[Range truncated: requested lines %d-%d, %d bytes total, showing first %d bytes. Re-read with a narrower range for the rest.]",
				head, start, end, len(sliced), len(head)), nil
		}
		return fmt.Sprintf("[Lines %d-%d of %s]\n%s", start, end, path, string(sliced)), nil
	}

	// Large-file strategy: return the head of the file with an explicit
	// truncation notice instead of refusing, so the model keeps working
	// context (imports, types, first definitions) and knows exactly what it
	// did not see.
	if len(content) > MaxReadFileBytes {
		lineCount := countLines(content)
		head := truncateUTF8Prefix(string(content), MaxReadFileBytes)
		return fmt.Sprintf(
			"%s\n\n[File truncated: %s is %d bytes and %d lines, showing the first %d bytes. "+
				"Use start_line/end_line for a specific range or search_files to locate sections.]",
			head, path, len(content), lineCount, len(head),
		), nil
	}

	return string(content), nil
}

// WriteFileTool writes content to a file
type WriteFileTool struct {
	BaseTool
}

// WriteFileInput represents the input for write_to_file tool
type WriteFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// NewWriteFileTool creates a new write_to_file tool
func NewWriteFileTool() *WriteFileTool {
	return &WriteFileTool{
		BaseTool: BaseTool{
			name: "write_to_file",
			description: "Write content to a file at the specified path. If the file exists, it will be overwritten " +
				"atomically: the new content is written to a temporary file and renamed into place, so a failed " +
				"write never destroys the previous content. Use this when creating new files or completely " +
				"rewriting existing files.",
			inputSchema: PathAndContentSchema,
		},
	}
}

// Execute writes the file
func (t *WriteFileTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req WriteFileInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Clean the path
	path := filepath.Clean(req.Path)

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	// Check if file exists
	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
	}

	// Write file atomically: on any failure the previous content survives.
	if err := writeFileAtomic(path, []byte(req.Content)); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	action := "created"
	if existed {
		action = "overwritten"
	}

	return fmt.Sprintf("File %s successfully: %s (%d bytes)", action, path, len(req.Content)), nil
}

// writeFileAtomic replaces path with data through a temporary file in the same
// directory followed by a rename, so readers never observe a partial file and
// a failed write leaves the original untouched. The previous file's mode is
// preserved; new files get 0644.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ReplaceInFileTool replaces content in a file using SEARCH/REPLACE blocks
type ReplaceInFileTool struct {
	BaseTool
}

// ReplacementBlock represents a single search/replace operation within a file.
type ReplacementBlock struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

// ReplaceInFileInput represents the input for replace_in_file tool.
// Supports two calling styles:
//   - Single block: path + search + replace
//   - Multiple blocks: path + replacements (array of {search, replace})
//
// When editing the same file multiple times, use a single call with the replacements array.
type ReplaceInFileInput struct {
	Path         string             `json:"path"`
	Search       string             `json:"search"`
	Replace      string             `json:"replace"`
	Replacements []ReplacementBlock `json:"replacements"`
}

// NewReplaceInFileTool creates a new replace_in_file tool
func NewReplaceInFileTool() *ReplaceInFileTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path of the file to modify"
			},
			"search": {
				"type": "string",
				"description": "The exact content to search for (single block style). Use this for one targeted modification."
			},
			"replace": {
				"type": "string",
				"description": "The content to replace with (single block style)"
			},
			"replacements": {
				"type": "array",
				"description": "Array of replacement blocks for multiple modifications in one call. Preferred when editing the same file multiple times.",
				"items": {
					"type": "object",
					"properties": {
						"search": {
							"type": "string",
							"description": "The exact content to find"
						},
						"replace": {
							"type": "string",
							"description": "The content to replace with"
						}
					},
					"required": ["search", "replace"]
				}
			}
		},
		"required": ["path"]
	}`)

	return &ReplaceInFileTool{
		BaseTool: BaseTool{
			name: "replace_in_file",
			description: "Replace specific content in a file using exact search/replace. Supports single blocks or an " +
				"array of replacements for multiple edits. All blocks are applied to an in-memory copy first: if any " +
				"block fails to match, the file is left untouched and the error names the failing block. The result is " +
				"written back atomically. Use this for targeted modifications to existing files.",
			inputSchema: schema,
		},
	}
}

// Execute replaces content in the file with full error feedback, multi-block
// support, whitespace tolerance, and diff output.
func (t *ReplaceInFileTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ReplaceInFileInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Normalize single-block input into the multi-block form
	var blocks []ReplacementBlock
	if len(req.Replacements) > 0 {
		blocks = req.Replacements
	} else {
		blocks = []ReplacementBlock{{Search: req.Search, Replace: req.Replace}}
	}

	// Clean the path
	path := filepath.Clean(req.Path)

	// Read existing file
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", path)
		}
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	content := string(contentBytes)

	// Apply replacements sequentially
	var results []string
	for i, block := range blocks {
		idx := strings.Index(content, block.Search)
		if idx == -1 {
			// Try normalized whitespace matching as fallback
			normalizedContent := normalizeWhitespace(content)
			normalizedSearch := normalizeWhitespace(block.Search)
			if nidx := strings.Index(normalizedContent, normalizedSearch); nidx != -1 {
				// Map normalized index back to original content via line-based anchor
				content, err = applyLineAnchorFallback(content, block.Search, block.Replace)
				if err == nil {
					results = append(results, fmt.Sprintf("Block %d: applied with whitespace normalization", i+1))
					continue
				}
			}
			// Build detailed error with helpful guidance
			return "", fmt.Errorf(
				"Block %d – search content not found in file.\n\n"+
					"The search must match EXACTLY, including:\n"+
					"  • All whitespace (spaces, tabs, newlines)\n"+
					"  • Indentation characters\n"+
					"  • Trailing spaces\n\n"+
					"TROUBLESHOOTING:\n"+
					"1. Use read_file to get the CURRENT file content\n"+
					"2. Copy-paste the EXACT text you want to replace\n"+
					"3. Check for tabs vs spaces - they are different!\n"+
					"4. Try searching for a smaller unique substring\n"+
					"5. For complex edits, consider using write_to_file instead",
				i+1,
			)
		}
		content = content[:idx] + block.Replace + content[idx+len(block.Search):]
		results = append(results, fmt.Sprintf("Block %d: replaced at byte offset %d", i+1, idx))
	}

	// Write back atomically: a failed write leaves the original file intact.
	if err := writeFileAtomic(path, []byte(content)); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	// Compute unified diff for feedback
	diff := computeDiff(string(contentBytes), content)
	if diff == "" {
		diff = "(no visual change)"
	}

	var summary strings.Builder
	summary.WriteString(fmt.Sprintf("File modified successfully: %s\n\n", path))
	for _, r := range results {
		summary.WriteString(r + "\n")
	}
	summary.WriteString(fmt.Sprintf("\nChanges (%d replacements):\n%s\n", len(blocks), diff))
	return summary.String(), nil
}

// normalizeWhitespace collapses runs of spaces/tabs/newlines into a single space
// to enable fuzzy matching when only whitespace differs.
func normalizeWhitespace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			inSpace = true
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// matchInfo holds a candidate snippet and its similarity score.
type matchInfo struct {
	Text  string
	Score float64
}

// findNearestMatch returns the substring of content that is most similar to search.
// It slides a window of len(search)±20% across the content and uses Jaccard similarity
// on character bigrams.
func findNearestMatch(content, search string) matchInfo {
	searchLen := len(search)
	if searchLen == 0 {
		return matchInfo{Text: "(empty search)", Score: 0}
	}
	searchGrams := buildBigrams(search)
	best := matchInfo{Score: -1}
	minWin := max(1, searchLen-searchLen/5)
	maxWin := searchLen + searchLen/5

	for win := minWin; win <= maxWin; win++ {
		for i := 0; i+win <= len(content); i++ {
			sub := content[i : i+win]
			grams := buildBigrams(sub)
			score := jaccardSimilarity(searchGrams, grams)
			if score > best.Score {
				best = matchInfo{Text: sub, Score: score}
				if score == 1.0 {
					return best
				}
			}
		}
	}
	return best
}

// buildBigrams returns a set of character bigrams for similarity comparison.
func buildBigrams(s string) map[string]int {
	runes := []rune(s)
	grams := make(map[string]int)
	for i := 0; i < len(runes)-1; i++ {
		g := string(runes[i]) + string(runes[i+1])
		grams[g]++
	}
	return grams
}

// jaccardSimilarity computes Jaccard index between two multisets.
func jaccardSimilarity(a, b map[string]int) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	intersection := 0
	for k, va := range a {
		if vb, ok := b[k]; ok {
			if va < vb {
				intersection += va
			} else {
				intersection += vb
			}
		}
	}
	union := 0
	for k, v := range a {
		union += v
		if b[k] > v {
			union += b[k] - v
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			union += v
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// applyLineAnchorFallback attempts to match by first identifying a unique line
// from the search block and then applying the replacement around that anchor.
func applyLineAnchorFallback(content, search, replace string) (string, error) {
	searchLines := strings.Split(search, "\n")
	if len(searchLines) == 0 {
		return "", fmt.Errorf("empty search block")
	}
	// Pick the longest line as anchor – most likely to be unique.
	var anchor string
	for _, line := range searchLines {
		if len(line) > len(anchor) {
			anchor = line
		}
	}
	anchor = strings.TrimSpace(anchor)
	if anchor == "" {
		return "", fmt.Errorf("no usable anchor line")
	}
	contentLines := strings.Split(content, "\n")
	for i, cl := range contentLines {
		if strings.TrimSpace(cl) == anchor {
			// Verify surrounding context roughly matches
			start := max(0, i-len(searchLines)/2)
			end := min(len(contentLines), start+len(searchLines))
			window := strings.Join(contentLines[start:end], "\n")
			if strings.Contains(normalizeWhitespace(window), normalizeWhitespace(search)) {
				// Found approximate match – replace window
				newContent := strings.Join(contentLines[:start], "\n")
				if start > 0 {
					newContent += "\n"
				}
				newContent += replace
				if end < len(contentLines) {
					newContent += "\n"
				}
				newContent += strings.Join(contentLines[end:], "\n")
				return newContent, nil
			}
		}
	}
	return "", fmt.Errorf("line anchor not found")
}

// computeDiff returns a simple unified-style diff for feedback.
func computeDiff(oldContent, newContent string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")
	var b strings.Builder
	b.WriteString("```diff\n")
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			b.WriteString(" " + oldLines[i] + "\n")
			i++
			j++
		} else {
			// Show removed line (if any)
			if i < len(oldLines) {
				b.WriteString("-" + oldLines[i] + "\n")
				i++
			}
			// Show added line (if any)
			if j < len(newLines) {
				b.WriteString("+" + newLines[j] + "\n")
				j++
			}
		}
	}
	for i < len(oldLines) {
		b.WriteString("-" + oldLines[i] + "\n")
		i++
	}
	for j < len(newLines) {
		b.WriteString("+" + newLines[j] + "\n")
		j++
	}
	b.WriteString("```")
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sliceLines extracts lines [startLine, endLine] from content using 1-based
// line numbers. endLine == 0 means "to the end". It returns the sliced bytes
// along with the effective start/end line numbers.
func sliceLines(content []byte, startLine, endLine int) ([]byte, int, int, error) {
	if startLine < 0 {
		startLine = 0
	}
	if startLine == 0 {
		startLine = 1
	}
	if endLine > 0 && endLine < startLine {
		return nil, startLine, endLine, fmt.Errorf("end_line (%d) must be >= start_line (%d)", endLine, startLine)
	}

	lines := strings.Split(string(content), "\n")
	if startLine > len(lines) {
		return []byte{}, len(lines), len(lines), nil
	}
	effectiveEnd := endLine
	if effectiveEnd == 0 || effectiveEnd > len(lines) {
		effectiveEnd = len(lines)
	}

	selected := lines[startLine-1 : effectiveEnd]
	out := strings.Join(selected, "\n")
	if effectiveEnd < len(lines) {
		out += "\n"
	}
	return []byte(out), startLine, effectiveEnd, nil
}

// countLines returns the number of newline-separated lines.
func countLines(content []byte) int {
	return strings.Count(string(content), "\n") + 1
}

// ListFilesTool lists files and directories
type ListFilesTool struct {
	BaseTool
}

// ListFilesInput represents the input for list_files tool
type ListFilesInput struct {
	Path string `json:"path"`
}

// NewListFilesTool creates a new list_files tool
func NewListFilesTool() *ListFilesTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The directory path to list"
			}
		},
		"required": ["path"]
	}`)

	return &ListFilesTool{
		BaseTool: BaseTool{
			name:        "list_files",
			description: "List files and directories at the specified path. Use this to explore the file system.",
			inputSchema: schema,
		},
	}
}

// Execute lists files
func (t *ListFilesTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ListFilesInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Clean the path
	path := filepath.Clean(req.Path)

	// Check if path exists
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

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Contents of %s:\n\n", path))

	err = listFiles(path, &result)

	if err != nil {
		return "", err
	}

	return result.String(), nil
}

// MaxListEntries caps how many directory entries list_files reports. The
// remaining count is reported in a trailing notice.
const MaxListEntries = 500

// listFiles writes the directory listing. os.ReadDir returns entries sorted by
// filename, which the tool relies on for stable output.
func listFiles(path string, result *strings.Builder) error {
	return listFilesUpTo(path, result, MaxListEntries)
}

// listFilesUpTo writes at most max entries and appends a truncation notice when
// the directory holds more.
func listFilesUpTo(path string, result *strings.Builder, max int) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	shown := 0
	for _, entry := range entries {
		if max > 0 && shown >= max {
			fmt.Fprintf(result,
				"\n[Listing truncated: %d of %d entries shown. Narrow the path to see the rest.]\n",
				shown, len(entries))
			return nil
		}
		prefix := "📄 "
		if entry.IsDir() {
			prefix = "📁 "
		}
		result.WriteString(fmt.Sprintf("%s%s\n", prefix, entry.Name()))
		shown++
	}

	return nil
}
