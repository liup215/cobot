package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// ─── execute_command output ──────────────────────────────────────────────────

// TestTruncateCommandOutputKeepsRuneBoundaries pins that the 32KB output cap
// never splits a multi-byte character at either cut: Chinese output must stay
// readable instead of degrading into U+FFFD at the truncation edges.
func TestTruncateCommandOutputKeepsRuneBoundaries(t *testing.T) {
	long := "ab" + strings.Repeat("汉", 20000) + "MIDDLE-MARKER" + strings.Repeat("z", 40*1024)
	got := truncateCommandOutput(long)

	if len(got) > MaxCommandOutputBytes {
		t.Errorf("truncated output too long: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("truncated output is not valid UTF-8")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Error("truncated output contains U+FFFD — a rune was split at a truncation boundary")
	}
	if !strings.HasPrefix(got, "ab汉") {
		t.Error("the head of the output must be kept")
	}
	if !strings.HasSuffix(got, "zzz") {
		t.Error("the tail of the output must be kept")
	}
	if strings.Contains(got, "MIDDLE-MARKER") {
		t.Error("the middle of the output should be dropped")
	}
	if !strings.Contains(got, "[... output truncated ...]") {
		t.Error("truncation marker missing")
	}
}

// ─── read_file ───────────────────────────────────────────────────────────────

// runReadFile reads a temp file through the read_file tool.
func runReadFile(t *testing.T, content string, startLine, endLine int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	input, err := json.Marshal(ReadFileInput{Path: path, StartLine: startLine, EndLine: endLine})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	out, err := NewReadFileTool().Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	return out
}

// noticeHead splits a truncated result into the shown content and its notice.
func noticeHead(t *testing.T, result, notice string) string {
	t.Helper()
	idx := strings.Index(result, notice)
	if idx < 0 {
		t.Fatalf("truncation notice %q missing from result", notice)
	}
	return result[:idx]
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

// TestReadFileWholeFileTruncationKeepsRuneBoundaries pins the 64KB whole-file
// cap: the cut lands inside a multi-byte rune in this fixture, and the result
// must back off to the rune boundary instead of emitting U+FFFD.
func TestReadFileWholeFileTruncationKeepsRuneBoundaries(t *testing.T) {
	// One byte of ASCII before the cap, then Chinese: the naive byte cut at
	// MaxReadFileBytes would keep 2 of the 3 bytes of the rune at the edge.
	content := strings.Repeat("a", MaxReadFileBytes-1) + strings.Repeat("汉", 5000)
	result := runReadFile(t, content, 0, 0)

	head := noticeHead(t, result, "\n\n[File truncated:")
	if len(head) > MaxReadFileBytes {
		t.Errorf("shown content is %d bytes, cap is %d", len(head), MaxReadFileBytes)
	}
	assertValidUTF8(t, "shown content", head)
	// The notice reports the bytes actually shown, not the nominal cap.
	if !strings.Contains(result, "showing the first "+strconv.Itoa(len(head))+" bytes") {
		t.Errorf("notice does not report the actual shown size (%d): %.200s", len(head), result)
	}
	if !strings.HasPrefix(head, strings.Repeat("a", 100)) {
		t.Error("the head of the file must be kept")
	}
}

// TestReadFileRangeTruncationKeepsRuneBoundaries pins the 100KB line-range cap
// the same way.
func TestReadFileRangeTruncationKeepsRuneBoundaries(t *testing.T) {
	// Two bytes before the cap so the naive cut at MaxReadRangeBytes lands in
	// the middle of a rune.
	content := strings.Repeat("a", MaxReadRangeBytes-2) +
		strings.Repeat("中文内容\n", 200)
	result := runReadFile(t, content, 1, 0)

	head := noticeHead(t, result, "\n\n[Range truncated:")
	if len(head) > MaxReadRangeBytes {
		t.Errorf("shown range is %d bytes, cap is %d", len(head), MaxReadRangeBytes)
	}
	assertValidUTF8(t, "shown range", head)
	if !strings.Contains(result, "showing first "+strconv.Itoa(len(head))+" bytes") {
		t.Errorf("notice does not report the actual shown size (%d): %.200s", len(head), result)
	}
	if !strings.HasSuffix(head, "a") {
		t.Errorf("the cut did not back off to the rune boundary cleanly: tail=%q", head[max(0, len(head)-8):])
	}
}
