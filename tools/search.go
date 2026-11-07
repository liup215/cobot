package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// searchCacheKey uniquely identifies a search request.
type searchCacheKey struct {
	path    string
	pattern string
	regex   string
}

// searchCacheEntry holds cached search results with a TTL.
type searchCacheEntry struct {
	output    *SearchFilesOutput
	createdAt time.Time
}

var (
	searchCache      = make(map[searchCacheKey]*searchCacheEntry)
	searchCacheMu    sync.RWMutex
	searchCacheTTL   = 30 * time.Second // refresh after 30s
	maxSearchResults = 500              // cap results to avoid huge payloads
)

// clearExpiredSearchCache removes stale entries (called lazily on write).
func clearExpiredSearchCache() {
	now := time.Now()
	for k, v := range searchCache {
		if now.Sub(v.createdAt) > searchCacheTTL {
			delete(searchCache, k)
		}
	}
}

// Common directories to skip during file traversal.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"__pycache__":  true,
	".next":        true,
	".nuxt":        true,
	".output":      true,
	".idea":        true,
	".vscode":      true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"out":          true,
	".terraform":   true,
}

// Binary file extensions to skip.
var binaryExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".bin": true, ".o": true, ".a": true, ".obj": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".ico": true, ".webp": true,
	".mp3": true, ".mp4": true, ".avi": true, ".mov": true,
	".zip": true, ".tar": true, ".gz": true, ".rar": true,
	".7z": true, ".pdf": true, ".doc": true, ".docx": true,
	".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".eot": true, ".wasm": true, ".sqlite": true, ".db": true,
}

const (
	// Max file size to search (8MB).
	maxFileSize = 8 << 20
	// Context lines around a match.
	contextLines = 2
	// Worker multiplier over CPU count.
	workerMultiplier = 4
)

// searcher abstracts regex/literal search.
type searcher interface {
	FindAllIndex([]byte) [][]int
	String() string
}

// regexSearcher wraps *regexp.Regexp.
type regexSearcher struct {
	re *regexp.Regexp
}

func (s *regexSearcher) FindAllIndex(b []byte) [][]int {
	return s.re.FindAllIndex(b, -1)
}

func (s *regexSearcher) String() string { return s.re.String() }

// literalSearcher does fast substring search.
type literalSearcher struct {
	pattern []byte
}

func (s *literalSearcher) FindAllIndex(b []byte) [][]int {
	var out [][]int
	p := s.pattern
	if len(p) == 0 {
		return nil
	}
	n := len(p)
	start := 0
	for {
		idx := bytes.Index(b[start:], p)
		if idx < 0 {
			break
		}
		pos := start + idx
		out = append(out, []int{pos, pos + n})
		start = pos + n
	}
	return out
}

func (s *literalSearcher) String() string { return string(s.pattern) }

// GrepTool searches for patterns in files.
type GrepTool struct {
	BaseTool
}

// GrepInput represents the input for grep tool.
type GrepInput struct {
	Path        string `json:"path"`
	Regex       string `json:"regex"`
	FilePattern string `json:"file_pattern,omitempty"`
}

// SearchResult represents a single search result.
type SearchResult struct {
	Path        string `json:"path"`
	Line        int    `json:"line"`
	Column      int    `json:"column"`
	Match       string `json:"match"`
	Context     string `json:"context"`
	ContextLine int    `json:"context_line"`
}

// SearchFilesOutput represents the output of grep tool.
type SearchFilesOutput struct {
	Results      []SearchResult `json:"results"`
	TotalFiles   int            `json:"total_files"`
	TotalMatches int            `json:"total_matches"`
	// Truncated is true when Results was capped at maxSearchResults.
	Truncated bool `json:"truncated,omitempty"`
}

// NewGrepTool creates a new grep tool.
func NewGrepTool() *GrepTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The directory to search in"
			},
			"regex": {
				"type": "string",
				"description": "The regex pattern to search for"
			},
			"file_pattern": {
				"type": "string",
				"description": "Optional glob pattern to filter files (e.g., '*.go')"
			}
		},
		"required": ["path", "regex"]
	}`)

	return &GrepTool{
		BaseTool: BaseTool{
			name: "grep",
			description: "Search for a regex pattern in files within a directory. Returns context-rich results with file paths, line numbers, and surrounding context. Powered by ripgrep (fast, respects .gitignore) when installed. " +
				"Prefer this over reading whole files when looking for code. To find files by NAME, use glob instead.",
			inputSchema: schema,
		},
	}
}

// Execute searches for the pattern.
func (t *GrepTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req GrepInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if req.Regex == "" {
		return "", fmt.Errorf("regex is required")
	}

	// Check cache first.
	path := filepath.Clean(req.Path)
	cacheKey := searchCacheKey{path: path, pattern: req.FilePattern, regex: req.Regex}
	searchCacheMu.RLock()
	if entry, ok := searchCache[cacheKey]; ok && time.Since(entry.createdAt) < searchCacheTTL {
		searchCacheMu.RUnlock()
		return formatSearchResults(entry.output), nil
	}
	searchCacheMu.RUnlock()

	// Fast path: ripgrep when installed — parallel traversal, SIMD matching
	// and .gitignore awareness make it orders of magnitude faster than the
	// pure-Go walk on large repositories. Falls back when rg is missing or
	// fails to run; cancellation is propagated instead.
	if out, ok, err := searchFilesRipgrep(ctx, path, req.Regex, req.FilePattern); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		// rg broken: fall through to the pure-Go implementation.
	} else if ok {
		// Cap results without falsifying the total: Truncated + the format
		// notice tell the model how much was dropped (kept from cobot during
		// the gline alignment).
		if len(out.Results) > maxSearchResults {
			out.Results = out.Results[:maxSearchResults]
			out.Truncated = true
		}
		if out.TotalMatches < len(out.Results) {
			out.TotalMatches = len(out.Results)
		}
		storeSearchCache(cacheKey, out)
		return formatSearchResults(out), nil
	}

	output, err := searchFilesGo(ctx, path, req.Regex, req.FilePattern)
	if err != nil {
		return "", err
	}

	// Store in cache.
	storeSearchCache(cacheKey, output)

	// Format output.
	return formatSearchResults(output), nil
}

// storeSearchCache caches a search output under the given key.
func storeSearchCache(key searchCacheKey, output *SearchFilesOutput) {
	searchCacheMu.Lock()
	clearExpiredSearchCache()
	searchCache[key] = &searchCacheEntry{output: output, createdAt: time.Now()}
	searchCacheMu.Unlock()
}

// searchFilesGo is the pure-Go fallback: walk the tree, read candidate files
// and match with Go's regexp/bytes search using a worker pool.
func searchFilesGo(ctx context.Context, path, regex, filePattern string) (*SearchFilesOutput, error) {
	// Build appropriate searcher (literal fast path when possible).
	var srh searcher
	if isLiteralPattern(regex) {
		srh = &literalSearcher{pattern: []byte(regex)}
	} else {
		re, err := regexp.Compile(regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern: %w", err)
		}
		srh = &regexSearcher{re: re}
	}

	// Check if path exists.
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("path not found: %s", path)
		}
		return nil, fmt.Errorf("failed to stat path: %w", err)
	}

	// If path is a file, search just that file.
	var files []string
	if !info.IsDir() {
		files = []string{path}
	} else {
		files, err = findFiles(path, filePattern)
		if err != nil {
			return nil, fmt.Errorf("failed to find files: %w", err)
		}
	}

	// Search in files concurrently.
	output := searchFilesConcurrent(ctx, files, srh)

	// Cap results to avoid huge payloads; TotalMatches keeps the TRUE count
	// so the truncation notice can say how much was dropped.
	if len(output.Results) > maxSearchResults {
		output.Truncated = true
		output.Results = output.Results[:maxSearchResults]
	}
	return output, nil
}

// isLiteralPattern returns true if the pattern contains no regex metacharacters.
func isLiteralPattern(s string) bool {
	for _, r := range s {
		switch r {
		case '\\', '.', '*', '?', '+', '[', ']', '(', ')', '{', '}',
			'^', '$', '|':
			return false
		}
	}
	return true
}

// searchFilesConcurrent searches files using a worker pool.
func searchFilesConcurrent(ctx context.Context, files []string, srh searcher) *SearchFilesOutput {
	numWorkers := runtime.GOMAXPROCS(0) * workerMultiplier
	if numWorkers > len(files) {
		numWorkers = len(files)
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	fileCh := make(chan string, numWorkers)
	resultCh := make(chan []SearchResult, numWorkers)

	var wg sync.WaitGroup

	// Start workers.
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range fileCh {
				select {
				case <-ctx.Done():
					return
				default:
				}
				results, _ := searchInFile(file, srh)
				if len(results) > 0 {
					resultCh <- results
				}
			}
		}()
	}

	// Wait for workers then close result channel.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	// Send files.
	go func() {
		for _, f := range files {
			select {
			case <-ctx.Done():
				break
			case fileCh <- f:
			}
		}
		close(fileCh)
	}()

	// Collect results.
	output := &SearchFilesOutput{
		Results:    make([]SearchResult, 0),
		TotalFiles: len(files),
	}
	for results := range resultCh {
		output.Results = append(output.Results, results...)
	}
	output.TotalMatches = len(output.Results)

	// Sort by path, then line number.
	sort.Slice(output.Results, func(i, j int) bool {
		if output.Results[i].Path != output.Results[j].Path {
			return output.Results[i].Path < output.Results[j].Path
		}
		return output.Results[i].Line < output.Results[j].Line
	})

	return output
}

// findFiles finds all files matching the pattern.
func findFiles(root string, pattern string) ([]string, error) {
	var files []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors.
		}

		if info.IsDir() {
			name := info.Name()
			if skipDirs[name] {
				return filepath.SkipDir
			}
			// Skip hidden directories.
			if strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden files.
		name := info.Name()
		if strings.HasPrefix(name, ".") {
			return nil
		}

		// Skip large and binary files.
		if info.Size() > maxFileSize {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(name))
		if binaryExts[ext] {
			return nil
		}

		// Apply file pattern filter.
		if pattern != "" {
			matched, err := filepath.Match(pattern, name)
			if err != nil {
				return nil
			}
			if !matched {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	return files, err
}

// searchInFile searches for pattern in a single file.
func searchInFile(path string, srh searcher) ([]SearchResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Quick check: if literal pattern not in file at all, skip.
	if ls, ok := srh.(*literalSearcher); ok {
		if !bytes.Contains(data, ls.pattern) {
			return nil, nil
		}
	}

	// Split into lines preserving line endings for accurate counting.
	content := string(data)
	lines := strings.Split(content, "\n")

	// Pre-calculate line start positions in the raw string.
	lineStarts := make([]int, len(lines))
	pos := 0
	for i := 0; i < len(lines); i++ {
		lineStarts[i] = pos
		pos += len(lines[i])
		if i < len(lines)-1 {
			pos++ // Account for "\n".
		}
	}

	var results []SearchResult
	lineIdx := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		locs := srh.FindAllIndex([]byte(line))
		if len(locs) == 0 {
			continue
		}

		// Get context range.
		ctxStart := i - contextLines
		if ctxStart < 0 {
			ctxStart = 0
		}
		ctxEnd := i + contextLines + 1
		if ctxEnd > len(lines) {
			ctxEnd = len(lines)
		}

		// Pre-allocate string builder.
		var b strings.Builder
		b.Grow(256)
		for j := ctxStart; j < ctxEnd; j++ {
			if j > ctxStart {
				b.WriteByte('\n')
			}
			if j == i {
				b.WriteString("> ")
			} else {
				b.WriteString("  ")
			}
			b.WriteString(strconv.Itoa(j + 1))
			b.WriteString(": ")
			b.WriteString(lines[j])
		}
		contextStr := b.String()

		for _, loc := range locs {
			start, end := loc[0], loc[1]
			// Convert byte column to rune column for display.
			col := utf8.RuneCountInString(line[:start]) + 1
			result := SearchResult{
				Path:        path,
				Line:        i + 1,
				Column:      col,
				Match:       line[start:end],
				Context:     contextStr,
				ContextLine: i - ctxStart + 1,
			}
			results = append(results, result)
		}
		lineIdx++
		_ = lineIdx // Unused.
	}

	return results, nil
}

// formatSearchResults formats search results for display.
func formatSearchResults(output *SearchFilesOutput) string {
	if len(output.Results) == 0 {
		return fmt.Sprintf("No matches found in %d files.", output.TotalFiles)
	}

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Found %d matches in %d files:\n\n", output.TotalMatches, output.TotalFiles))

	// Group by file (already sorted).
	var currentPath string
	for i, r := range output.Results {
		if r.Path != currentPath {
			if i > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString("📄 ")
			builder.WriteString(r.Path)
			builder.WriteByte('\n')
			currentPath = r.Path
		}
		// Include a compact context snippet. The full context is available on
		// demand via read using line_number.
		builder.WriteString(fmt.Sprintf("   Line %d, Col %d: %s\n", r.Line, r.Column, r.Match))
		if r.Context != "" {
			builder.WriteString(r.Context)
			builder.WriteByte('\n')
		}
	}

	if output.Truncated {
		if output.TotalMatches > len(output.Results) {
			fmt.Fprintf(&builder,
				"\n[Results truncated: %d of %d matches shown. Narrow the regex or the path to see the rest.]\n",
				len(output.Results), output.TotalMatches)
		} else {
			fmt.Fprintf(&builder,
				"\n[Results truncated: showing the first %d matches; more may exist. Narrow the regex or the path to see the rest.]\n",
				len(output.Results))
		}
	}

	return builder.String()
}

// ListCodeDefinitionNamesTool lists code definitions (functions, classes, etc.)
type ListCodeDefinitionNamesTool struct {
	BaseTool
}

// ListCodeDefinitionNamesInput represents the input for list_code_definition_names tool
type ListCodeDefinitionNamesInput struct {
	Path        string `json:"path"`
	FilePattern string `json:"file_pattern,omitempty"`
}

// CodeDefinition represents a code definition
type CodeDefinition struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // function, class, method, etc.
	Line     int    `json:"line"`
	File     string `json:"file"`
	Language string `json:"language"`
}

// NewListCodeDefinitionNamesTool creates a new list_code_definition_names tool
func NewListCodeDefinitionNamesTool() *ListCodeDefinitionNamesTool {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "The directory to analyze"
			},
			"file_pattern": {
				"type": "string",
				"description": "Optional glob pattern to filter files (e.g., '*.go')"
			}
		},
		"required": ["path"]
	}`)

	return &ListCodeDefinitionNamesTool{
		BaseTool: BaseTool{
			name:        "list_code_definition_names",
			description: "List definition names (functions, classes, methods, etc.) in code files within a directory. Provides insights into codebase structure.",
			inputSchema: schema,
		},
	}
}

// listDefCacheKey uniquely identifies a list_code_definition_names request.
type listDefCacheKey struct {
	path    string
	pattern string
}

var (
	listDefCache         = make(map[listDefCacheKey]*listDefCacheEntry)
	listDefCacheMu       sync.RWMutex
	listDefCacheTTL      = 30 * time.Second
	maxDefinitionResults = 500
)

type listDefCacheEntry struct {
	defs      []CodeDefinition
	createdAt time.Time
}

// Execute lists code definitions
func (t *ListCodeDefinitionNamesTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var req ListCodeDefinitionNamesInput
	if err := ParseInput(input, &req); err != nil {
		return "", err
	}

	if req.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Clean path
	path := filepath.Clean(req.Path)

	// Check cache first.
	cacheKey := listDefCacheKey{path: path, pattern: req.FilePattern}
	listDefCacheMu.RLock()
	if entry, ok := listDefCache[cacheKey]; ok && time.Since(entry.createdAt) < listDefCacheTTL {
		listDefCacheMu.RUnlock()
		return formatCodeDefinitions(entry.defs), nil
	}
	listDefCacheMu.RUnlock()

	// Find files
	files, err := findFiles(path, req.FilePattern)
	if err != nil {
		return "", fmt.Errorf("failed to find files: %w", err)
	}

	// Analyze each file
	allDefs := make([]CodeDefinition, 0, 64)
	for _, file := range files {
		defs := analyzeFile(file)
		allDefs = append(allDefs, defs...)
		if len(allDefs) >= maxDefinitionResults {
			allDefs = allDefs[:maxDefinitionResults]
			break
		}
	}

	// Store in cache.
	listDefCacheMu.Lock()
	for k, v := range listDefCache {
		if time.Since(v.createdAt) > listDefCacheTTL {
			delete(listDefCache, k)
		}
	}
	listDefCache[cacheKey] = &listDefCacheEntry{defs: allDefs, createdAt: time.Now()}
	listDefCacheMu.Unlock()

	// Format output
	return formatCodeDefinitions(allDefs), nil
}

// analyzeFile analyzes a single file for code definitions
func analyzeFile(path string) []CodeDefinition {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var defs []CodeDefinition
	language := detectLanguage(path)

	switch language {
	case "go":
		defs = analyzeGoCode(path, string(content))
	case "python":
		defs = analyzePythonCode(path, string(content))
	case "javascript", "typescript":
		defs = analyzeJavaScriptCode(path, string(content))
	default:
		// Generic analysis
		defs = analyzeGenericCode(path, string(content))
	}

	return defs
}

// detectLanguage detects the programming language from file extension
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js":
		return "javascript"
	case ".ts":
		return "typescript"
	case ".java":
		return "java"
	case ".cpp", ".cc", ".cxx":
		return "cpp"
	case ".c":
		return "c"
	case ".rs":
		return "rust"
	default:
		return "unknown"
	}
}

// analyzeGoCode analyzes Go code for definitions
func analyzeGoCode(path string, content string) []CodeDefinition {
	var defs []CodeDefinition
	lines := strings.Split(content, "\n")

	// Simple regex patterns for Go
	funcPattern := regexp.MustCompile(`^func\s+(\w+)`)
	typePattern := regexp.MustCompile(`^type\s+(\w+)\s+(struct|interface)`)

	for i, line := range lines {
		lineNum := i + 1

		if matches := funcPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[1],
				Type:     "function",
				Line:     lineNum,
				File:     path,
				Language: "go",
			})
		}

		if matches := typePattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[1],
				Type:     matches[2],
				Line:     lineNum,
				File:     path,
				Language: "go",
			})
		}
	}

	return defs
}

// analyzePythonCode analyzes Python code for definitions
func analyzePythonCode(path string, content string) []CodeDefinition {
	var defs []CodeDefinition
	lines := strings.Split(content, "\n")

	// Simple patterns for Python
	funcPattern := regexp.MustCompile(`^def\s+(\w+)`)
	classPattern := regexp.MustCompile(`^class\s+(\w+)`)

	for i, line := range lines {
		lineNum := i + 1

		if matches := funcPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[1],
				Type:     "function",
				Line:     lineNum,
				File:     path,
				Language: "python",
			})
		}

		if matches := classPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[1],
				Type:     "class",
				Line:     lineNum,
				File:     path,
				Language: "python",
			})
		}
	}

	return defs
}

// analyzeJavaScriptCode analyzes JavaScript/TypeScript code for definitions
func analyzeJavaScriptCode(path string, content string) []CodeDefinition {
	var defs []CodeDefinition
	lines := strings.Split(content, "\n")

	// Simple patterns for JS/TS
	funcPattern := regexp.MustCompile(`^(async\s+)?function\s+(\w+)`)
	classPattern := regexp.MustCompile(`^class\s+(\w+)`)
	arrowFuncPattern := regexp.MustCompile(`(const|let|var)\s+(\w+)\s*=\s*(async\s*)?\(`)

	for i, line := range lines {
		lineNum := i + 1

		if matches := funcPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[2],
				Type:     "function",
				Line:     lineNum,
				File:     path,
				Language: "javascript",
			})
		}

		if matches := classPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[1],
				Type:     "class",
				Line:     lineNum,
				File:     path,
				Language: "javascript",
			})
		}

		if matches := arrowFuncPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[2],
				Type:     "function",
				Line:     lineNum,
				File:     path,
				Language: "javascript",
			})
		}
	}

	return defs
}

// analyzeGenericCode provides generic code analysis
func analyzeGenericCode(path string, content string) []CodeDefinition {
	// Fallback: try to find common patterns
	var defs []CodeDefinition
	lines := strings.Split(content, "\n")

	// Generic function pattern
	funcPattern := regexp.MustCompile(`^(func|function|def|void|int|string|bool)\s+(\w+)`)

	for i, line := range lines {
		lineNum := i + 1
		if matches := funcPattern.FindStringSubmatch(line); matches != nil {
			defs = append(defs, CodeDefinition{
				Name:     matches[2],
				Type:     "function",
				Line:     lineNum,
				File:     path,
				Language: "unknown",
			})
		}
	}

	return defs
}

// formatCodeDefinitions formats code definitions for display
func formatCodeDefinitions(defs []CodeDefinition) string {
	if len(defs) == 0 {
		return "No code definitions found."
	}

	// Group by file — sorted (files + per-file line order) so output is
	// deterministic regardless of map iteration order (kept from cobot
	// during the gline alignment).
	fileDefs := make(map[string][]CodeDefinition)
	files := make([]string, 0, len(defs))
	for _, d := range defs {
		if _, ok := fileDefs[d.File]; !ok {
			files = append(files, d.File)
		}
		fileDefs[d.File] = append(fileDefs[d.File], d)
	}
	sort.Strings(files)

	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Found %d definitions:\n\n", len(defs)))

	for _, file := range files {
		definitions := fileDefs[file]
		sort.SliceStable(definitions, func(i, j int) bool {
			return definitions[i].Line < definitions[j].Line
		})
		builder.WriteString(fmt.Sprintf("📄 %s\n", file))
		for _, d := range definitions {
			icon := "🔧"
			switch d.Type {
			case "class":
				icon = "🏗️"
			case "interface":
				icon = "🔗"
			case "struct":
				icon = "📦"
			}
			builder.WriteString(fmt.Sprintf("   %s %s (%s) at line %d\n", icon, d.Name, d.Type, d.Line))
		}
		builder.WriteString("\n")
	}

	return builder.String()
}
