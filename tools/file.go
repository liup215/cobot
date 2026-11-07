package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/liup215/cobot/docparse"
)

// ReadFileTool reads the contents of a file
type ReadFileTool struct {
	BaseTool
}

// writeFileAtomic writes via a temp file + rename so a crash mid-write can
// never truncate the target (preserved from cobot's file.go during the
// gline tool-surface alignment).
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
	cleanup := func(e error) error {
		tmp.Close()
		os.Remove(tmpName)
		return e
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
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

// ReadFileInput represents the input for read tool
type ReadFileInput struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// NewReadFileTool creates a new read tool
const (
	readFileChunkLines = 1000      // default page size
	readFileMaxLines   = 2000      // hard cap per read (raise via limit)
	readFileMaxBytes   = 50 * 1024 // byte cap on returned content

	// maxExtractBytes caps files sent through document extraction (the whole
	// file is parsed into text in memory).
	maxExtractBytes = 64 << 20
)

// headBytes returns the first 64 bytes of a buffer for magic sniffing.
func headBytes(b []byte) []byte {
	if len(b) > 64 {
		return b[:64]
	}
	return b
}

// imageExts marks extensions treated as image files: their bytes cannot be
// returned as text, so the tool reports metadata instead of binary garbage.
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".bmp": true, ".svg": true, ".ico": true,
	".tiff": true, ".tif": true, ".heic": true,
}

// expandPath resolves a leading ~/ to the user's home directory.
func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// formatBytes renders a byte count for human-readable notices.
func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func NewReadFileTool() *ReadFileTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The path of the file to read. Supports ~ (home), relative, and absolute paths."
			},
			"line_number": {
				"type": "integer",
				"description": "Optional 1-based starting line number. If omitted, reads from line 1."
			},
			"limit": {
				"type": "integer",
				"description": "Optional number of lines to read (default 1000, max 2000)."
			}
		},
		"required": ["path"]
	}`)

	return &ReadFileTool{
		BaseTool: BaseTool{
			name: "read",
			description: "Read lines from a file. Supports ~ (home), relative, and absolute paths. " +
				fmt.Sprintf("Reads %d lines by default starting at line_number; pass limit for more (max %d per read). ", readFileChunkLines, readFileMaxLines) +
				"Documents (PDF, Word incl. .doc, Excel incl. .xls, PowerPoint, OpenDocument, RTF, HTML, EPUB) have their " +
				"text extracted automatically before paging — line_number works on the extracted text; a zip bundle " +
				"returns its entry listing. Images and files with no text layer report metadata/a notice instead of " +
				"content — do not retry those. " +
				"Output reports the line range, total line count and the next line_number to continue from. " +
				"For large files, use grep first to find a relevant section, then read that chunk with line_number. " +
				"If output is truncated, continue from the reported next line_number instead of re-reading the whole file. " +
				"Do not use run (cat/head/tail) to read files.",
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

	// Resolve the path: expand ~, then make absolute against the cwd so the
	// output prefix is stable no matter where later tools run.
	path := filepath.Clean(expandPath(req.Path))
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	// Check if file exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s (check the path, or use glob/grep to locate it)", path)
		}
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file: %s (use ls or glob to explore it)", path)
	}

	// Image files: binary content cannot be returned as text — report
	// metadata instead of returning base64 garbage that would flood context.
	ext := strings.ToLower(filepath.Ext(path))
	if imageExts[ext] {
		return fmt.Sprintf("[Image file: %s (%s, %s). Image content cannot be displayed yet. "+
			"If you need its pixels, process it via run (e.g. a script); otherwise ask the user to describe it.]",
			path, strings.TrimPrefix(ext, "."), formatBytes(int(info.Size()))), nil
	}

	// Read file
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// Document extraction: containers (PDF/Office/RTF/ODF/EPUB/zip) and
	// tag-based formats route through docparse first — the shared registry
	// also used by knowledge-base imports — so a PDF behaves like any other
	// line-paged text file. Plain text comes back verbatim from ExtractText,
	// so the line paging below is identical for both paths.
	if docparse.NeedsExtraction(headBytes(content)) && int64(len(content)) > maxExtractBytes {
		return "", fmt.Errorf("file too large to extract (%d MB, max %d MB) — read it in another way", int64(len(content))>>20, maxExtractBytes>>20)
	}
	text, err := docparse.ExtractText(path, content)
	if err != nil {
		if errors.Is(err, docparse.ErrNoTextLayer) {
			return fmt.Sprintf("[binary file: %s, %d bytes — no text layer (image/archive/unsupported format); use information already present in the conversation]", path, info.Size()), nil
		}
		return "", err
	}
	extracted := text != string(content)
	content = []byte(text)

	// Empty file: short-circuit before line math.
	if len(content) == 0 {
		return fmt.Sprintf("[Empty file: %s]", path), nil
	}

	// Single pass over the raw bytes for the total line count (no string copy).
	lineCount := bytes.Count(content, []byte("\n")) + 1

	// Page size: default chunk, raisable via limit up to the hard cap.
	limit := req.Limit
	if limit <= 0 {
		limit = readFileChunkLines
	}
	if limit > readFileMaxLines {
		limit = readFileMaxLines
	}

	startLine := req.LineNumber
	if startLine <= 0 {
		startLine = 1
	}
	if startLine > lineCount {
		return "", fmt.Errorf("line_number %d is beyond end of file (%s has %d lines)", startLine, path, lineCount)
	}
	endLine := startLine + limit - 1

	sliced, start, end, err := sliceLines(content, startLine, endLine)
	if err != nil {
		return "", fmt.Errorf("invalid line number: %w", err)
	}

	prefix := fmt.Sprintf("[Lines %d-%d of %s (total %d lines)]\n", start, end, path, lineCount)
	if extracted {
		prefix = fmt.Sprintf("[Lines %d-%d of %s (total %d lines, text extracted from %s)]\n",
			start, end, path, lineCount, strings.ToLower(filepath.Ext(path)))
	}

	// Byte cap on returned content so enormous lines cannot flood context.
	// Cut at the last complete line within the cap; a single oversized line
	// falls back to a raw cut trimmed to a valid UTF-8 boundary.
	if len(sliced) > readFileMaxBytes {
		cut := sliced[:readFileMaxBytes]
		if idx := bytes.LastIndexByte(cut, '\n'); idx > 0 {
			cut = cut[:idx+1]
		} else {
			for len(cut) > 0 && !utf8.Valid(cut) {
				cut = cut[:len(cut)-1]
			}
		}
		shownLines := start + bytes.Count(cut, []byte("\n")) - 1
		return fmt.Sprintf("%s%s\n\n[Chunk truncated: %s byte limit reached (chunk was %s). Continue with line_number=%d.]",
			prefix, string(cut), formatBytes(readFileMaxBytes), formatBytes(len(sliced)), shownLines+1), nil
	}

	// Large file guard: if this is not the full file, tell the model how to move forward.
	if end < lineCount {
		return fmt.Sprintf("%s%s\n\n[File continues: showing lines %d-%d of %d (%d remaining). Use line_number=%d to read the next chunk, or raise limit (max %d).]",
			prefix, string(sliced), start, end, lineCount, lineCount-end, end+1, readFileMaxLines), nil
	}

	return prefix + string(sliced), nil
}

// WriteFileTool writes content to a file
type WriteFileTool struct {
	BaseTool
}

// WriteFileInput represents the input for write tool
type WriteFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// NewWriteFileTool creates a new write tool
func NewWriteFileTool() *WriteFileTool {
	return &WriteFileTool{
		BaseTool: BaseTool{
			name: "write",
			description: "Write content to a file at the specified path. If the file exists, it will be overwritten. Use this when creating new files or completely rewriting existing files. " +
				"For small targeted changes to an existing file, use edit instead — a full overwrite risks losing content you have not read.",
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

	// Write file
	if err := writeFileAtomic(path, []byte(req.Content)); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	action := "created"
	if existed {
		action = "overwritten"
	}

	return fmt.Sprintf("File %s successfully: %s (%d bytes)", action, path, len(req.Content)), nil
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

// ReplaceInFileInput represents the input for edit tool.
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

// NewReplaceInFileTool creates a new edit tool
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
			name: "edit",
			description: "Replace content in a file by exact string match. search must match the file EXACTLY, including whitespace and indentation — read the file first and copy-paste the text to replace. " +
				"Supports a single block or an array of replacements for multiple edits in one call. Use it for targeted modifications; use write for new files or full rewrites.",
			inputSchema: schema,
		},
	}
}

// Execute replaces content in the file with full error feedback,
// multi-block support, whitespace tolerance, and diff output.
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
					"1. Use read to get the CURRENT file content\n"+
					"2. Copy-paste the EXACT text you want to replace\n"+
					"3. Check for tabs vs spaces - they are different!\n"+
					"4. Try searching for a smaller unique substring\n"+
					"5. For complex edits, consider using write instead",
				i+1,
			)
		}
		content = content[:idx] + block.Replace + content[idx+len(block.Search):]
		results = append(results, fmt.Sprintf("Block %d: replaced at byte offset %d", i+1, idx))
	}

	// Write back
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

// estimateTokensFromBytes gives a conservative token estimate from raw bytes.
func estimateTokensFromBytes(b []byte) int {
	// Approximate: 1 token per 3 bytes for ASCII-heavy text; 1 token per byte
	// for CJK-heavy text. We use a middle ground of 1 token per 2 bytes to be
	// conservative without overcounting pure ASCII.
	return len(b) / 2
}

// countLines removed: read tool now counts lines in a single bytes.Count pass
// over the raw content (no string copy).

// LsTool lists files and directories
type LsTool struct {
	BaseTool
}

// LsInput represents the input for ls tool
type LsInput struct {
	Path string `json:"path"`
}

// NewLsTool creates a new ls tool
func NewLsTool() *LsTool {
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

	return &LsTool{
		BaseTool: BaseTool{
			name: "ls",
			description: "List the files and directories at the specified path, with names, types and sizes. Use it to explore a directory. " +
				"To find files by name pattern anywhere below a directory, use glob; to search file contents, use grep.",
			inputSchema: schema,
		},
	}
}

// Execute lists files
func (t *LsTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req LsInput
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

func listFiles(path string, result *strings.Builder) error {
	return listFilesUpTo(path, result, MaxListEntries)
}

// MaxListEntries caps how many directory entries the ls tool reports. The
// remaining count is reported in a trailing notice (kept from cobot during
// the gline alignment — an uncapped listing of a huge directory would blow
// the context budget).
const MaxListEntries = 500

// listFilesUpTo writes at most max entries and appends a truncation notice
// when the directory holds more. max <= 0 means no cap.
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
