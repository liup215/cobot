package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSearchFiles(t *testing.T) {
	// Create a temp directory with test files.
	tmpDir := t.TempDir()

	// File 1: simple text with matches.
	f1 := filepath.Join(tmpDir, "a.go")
	content1 := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n\nfunc foo() {}\n"
	if err := os.WriteFile(f1, []byte(content1), 0644); err != nil {
		t.Fatal(err)
	}

	// File 2: no match.
	f2 := filepath.Join(tmpDir, "b.txt")
	if err := os.WriteFile(f2, []byte("just some text\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// File 3: large binary file that should be skipped.
	f3 := filepath.Join(tmpDir, "big.bin")
	if err := os.WriteFile(f3, make([]byte, maxFileSize+1), 0644); err != nil {
		t.Fatal(err)
	}

	// Test 1: literal search for "func".
	tool := NewGrepTool()
	input, _ := json.Marshal(GrepInput{
		Path:  tmpDir,
		Regex: "func",
	})
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	if !strings.Contains(output, "Found 2 matches") {
		t.Errorf("expected 2 func matches, got output:\n%s", output)
	}

	// Should NOT contain the big binary file.
	if strings.Contains(output, "big.bin") {
		t.Error("binary file should be skipped")
	}

	// Test 2: regex search with metacharacter.
	input2, _ := json.Marshal(GrepInput{
		Path:  tmpDir,
		Regex: `func \w+`,
	})
	output2, err := tool.Execute(context.Background(), input2)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output2, "Found 2 matches") {
		t.Errorf("expected 2 regex matches, got:\n%s", output2)
	}

	// Test 3: file pattern filter.
	input3, _ := json.Marshal(GrepInput{
		Path:        tmpDir,
		Regex:       "func",
		FilePattern: "*.go",
	})
	output3, err := tool.Execute(context.Background(), input3)
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(output3, "a.go") {
		t.Errorf("expected a.go in results, got:\n%s", output3)
	}
	if strings.Contains(output3, "b.txt") {
		t.Error("b.txt should be filtered out by pattern")
	}
}

func TestIsLiteralPattern(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"hello", true},
		{"func main", true},
		{"hello.world", false},
		{"foo*", false},
		{"a+b", false},
		{"[abc]", false},
		{"(foo)", false},
		{"^start", false},
		{"end$", false},
		{"a|b", false},
	}

	for _, tc := range tests {
		got := isLiteralPattern(tc.input)
		if got != tc.expected {
			t.Errorf("isLiteralPattern(%q) = %v, want %v", tc.input, got, tc.expected)
		}
	}
}

func TestFindFilesSkipsDirs(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a normal file.
	os.WriteFile(filepath.Join(tmpDir, "ok.txt"), []byte("ok"), 0644)

	// Create a node_modules dir that should be skipped.
	os.MkdirAll(filepath.Join(tmpDir, "node_modules", "foo"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "node_modules", "foo", "bad.txt"), []byte("bad"), 0644)

	files, err := findFiles(tmpDir, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if !strings.Contains(files[0], "ok.txt") {
		t.Errorf("expected ok.txt, got %s", files[0])
	}
}

// BenchmarkSearchInFile benchmarks search performance.
func BenchmarkSearchInFile(b *testing.B) {
	// Create a realistic Go source file (~500 lines).
	var content strings.Builder
	content.WriteString("package main\n\n")
	for i := 0; i < 100; i++ {
		content.WriteString("// comment line\n")
		content.WriteString("func Function")
		content.WriteString(strconv.Itoa(i))
		content.WriteString("() {\n\t// body\n}\n\n")
	}
	content.WriteString("func main() {\n\tprintln(\"done\")\n}\n")
	data := content.String()

	tmpDir := b.TempDir()
	f := filepath.Join(tmpDir, "test.go")
	os.WriteFile(f, []byte(data), 0644)

	// Benchmark literal search.
	b.Run("literal", func(b *testing.B) {
		srh := &literalSearcher{pattern: []byte("func main")}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			searchInFile(f, srh)
		}
	})

	// Benchmark regex search.
	b.Run("regex", func(b *testing.B) {
		re, _ := regexp.Compile(`func \w+`)
		srh := &regexSearcher{re: re}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			searchInFile(f, srh)
		}
	})
}

// BenchmarkSearchConcurrent benchmarks concurrent search.
func BenchmarkSearchConcurrent(b *testing.B) {
	tmpDir := b.TempDir()

	// Create 100 files with 50 lines each.
	content := strings.Repeat("func hello() { println(\"world\") }\n", 50)
	for i := 0; i < 100; i++ {
		f := filepath.Join(tmpDir, "f"+strconv.Itoa(i)+".go")
		os.WriteFile(f, []byte(content), 0644)
	}

	tool := NewGrepTool()
	input, _ := json.Marshal(GrepInput{
		Path:  tmpDir,
		Regex: "println",
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tool.Execute(context.Background(), input)
	}
}
