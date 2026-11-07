package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// TestTruncateUTF8Prefix pins the rune-boundary semantics of the head cut:
// the byte cap still holds, but the cut moves back so a multi-byte rune is
// never split (a split rune surfaces as U+FFFD in the model's context).
func TestTruncateUTF8Prefix(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		maxBytes  int
		wantValid bool // false = the input itself is invalid UTF-8; only the cap is asserted
	}{
		{"ascii passthrough", "hello world", 100, true},
		{"exact fit", "hello", 5, true},
		{"empty string", "", 10, true},
		{"zero cap", "hello", 0, true},
		{"negative cap", "hello", -1, true},
		{"cut inside ascii", "hello world", 7, true},
		{"cut inside cjk rune", "abc" + strings.Repeat("汉", 100), 10, true},
		{"cjk only", strings.Repeat("汉", 100), 10, true},
		{"emoji cut", strings.Repeat("🙂", 100), 11, true}, // 4-byte runes
		{"already invalid bytes", "ab\xff\xfecd", 4, false},
		{"continuation bytes only", "\x80\x80\x80\x80\x80", 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8Prefix(tc.s, tc.maxBytes)
			if tc.maxBytes <= 0 {
				if got != "" {
					t.Fatalf("got %q, want empty string for a non-positive cap", got)
				}
				return
			}
			if len(got) > tc.maxBytes {
				t.Errorf("result is %d bytes, cap is %d", len(got), tc.maxBytes)
			}
			if !strings.HasPrefix(tc.s, got) {
				t.Errorf("result %q is not a prefix of the input", got)
			}
			if !tc.wantValid {
				return
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("result contains U+FFFD: %q", got)
			}
		})
	}

	// A rune is either fully kept or fully dropped: cutting the CJK string at
	// byte 10 (one byte into the fourth rune) yields exactly three runes.
	if got := truncateUTF8Prefix(strings.Repeat("汉", 100), 10); got != strings.Repeat("汉", 3) {
		t.Errorf("cut inside a rune did not back off to the boundary: %q", got)
	}
}

// TestTruncateUTF8Suffix pins the rune-boundary semantics of the tail cut.
func TestTruncateUTF8Suffix(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		maxBytes  int
		wantValid bool // false = the input itself is invalid UTF-8; only the cap is asserted
	}{
		{"ascii passthrough", "hello world", 100, true},
		{"exact fit", "hello", 5, true},
		{"empty string", "", 10, true},
		{"zero cap", "hello", 0, true},
		{"negative cap", "hello", -1, true},
		{"cut inside ascii", "hello world", 7, true},
		{"cut inside cjk rune", strings.Repeat("汉", 100) + "abc", 10, true},
		{"cjk only", strings.Repeat("汉", 100), 10, true},
		{"already invalid bytes", "ab\xff\xfe", 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateUTF8Suffix(tc.s, tc.maxBytes)
			if tc.maxBytes <= 0 {
				if got != "" {
					t.Fatalf("got %q, want empty string for a non-positive cap", got)
				}
				return
			}
			if len(got) > tc.maxBytes {
				t.Errorf("result is %d bytes, cap is %d", len(got), tc.maxBytes)
			}
			if !strings.HasSuffix(tc.s, got) {
				t.Errorf("result %q is not a suffix of the input", got)
			}
			if !tc.wantValid {
				return
			}
			if !utf8.ValidString(got) {
				t.Errorf("result is not valid UTF-8: %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("result contains U+FFFD: %q", got)
			}
		})
	}

	// The suffix starts on the rune boundary: three whole runes, not a
	// one-byte stub of a fourth.
	if got := truncateUTF8Suffix(strings.Repeat("汉", 100), 10); got != strings.Repeat("汉", 3) {
		t.Errorf("cut inside a rune did not advance to the boundary: %q", got)
	}
}

// ─── read (byte-cap rune safety) ─────────────────────────────────────────────

// runRead reads a temp file through the read tool (line_number/limit page).
func runRead(t *testing.T, content string, lineNumber, limit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	input, err := json.Marshal(ReadFileInput{Path: path, LineNumber: lineNumber, Limit: limit})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	out, err := NewReadFileTool().Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

// assertValidUTF8 fails the test when s is not valid UTF-8 or contains U+FFFD.
func assertValidUTF8(t *testing.T, label, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s is not valid UTF-8", label)
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		t.Errorf("%s contains U+FFFD — a multi-byte rune was split", label)
	}
}

// TestReadFileByteCapKeepsRuneBoundaries pins the 50KB byte cap on a single
// oversized line (no newline to cut at): the cut must back off to a valid
// UTF-8 boundary instead of emitting U+FFFD into the model's context.
func TestReadFileByteCapKeepsRuneBoundaries(t *testing.T) {
	// One byte of ASCII before the cap, then Chinese: a naive byte cut at
	// readFileMaxBytes would keep 2 of the 3 bytes of the rune at the edge.
	content := strings.Repeat("a", readFileMaxBytes-1) + strings.Repeat("汉", 5000)
	result := runRead(t, content, 0, 0)

	idx := strings.Index(result, "[Chunk truncated:")
	if idx < 0 {
		t.Fatalf("chunk truncation notice missing: %.200s", result)
	}
	head := result[:idx]
	// Strip the "[Lines x-y of …]\n" prefix line and the trailing blank
	// line to get the shown content itself.
	if nl := strings.IndexByte(head, '\n'); nl >= 0 {
		head = head[nl+1:]
	}
	head = strings.TrimRight(head, "\n")
	if len(head) > readFileMaxBytes {
		t.Errorf("shown content is %d bytes, cap is %d", len(head), readFileMaxBytes)
	}
	assertValidUTF8(t, "shown content", head)
	if !strings.HasPrefix(head, strings.Repeat("a", 100)) {
		t.Error("the head of the file must be kept")
	}
	if !strings.Contains(result, "Continue with line_number=") {
		t.Errorf("notice must tell the model how to continue: %.300s", result)
	}
}
