package tools

// ripgrep-backed fast path for grep.
//
// When rg is installed it replaces the pure-Go walk-and-read loop with rg's
// parallel traversal, SIMD-accelerated matching and .gitignore awareness.
// The `rg --json` stream is parsed into the same SearchResult shape the Go
// fallback produces, so tool output stays identical for the model.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/liup215/cobot/shell"
)

// rgSkipGlob mirrors the pure-Go fallback's hardcoded skipDirs for
// repositories without a .gitignore. Hidden directories (dot-prefixed) are
// already excluded by rg's default hidden filter.
//
// NOTE: the globs must NOT contain a slash (no "!dir/**"). Slash-bearing
// globs anchor to the start of the full path, which never matches when the
// search root is passed to rg as an absolute path. A bare name matches the
// directory at any depth in both relative and absolute roots.
const rgSkipGlob = "!{__pycache__,build,dist,node_modules,out,target,vendor}"

// searchFilesRipgrep runs rg and parses its JSON stream.
// It returns ok=false when rg is unavailable or failed to run, signaling the
// caller to fall back to the pure-Go implementation. Cancellation errors are
// propagated (not swallowed into a fallback re-search).
func searchFilesRipgrep(ctx context.Context, path, pattern, filePattern string) (*SearchFilesOutput, bool, error) {
	rg := rgBinary()
	if rg == "" {
		return nil, false, nil
	}

	args := []string{
		"--json",
		"--no-messages",
		// Same file-size ceiling as the Go fallback.
		"--max-filesize", strconv.Itoa(maxFileSize>>20) + "M",
		// Bound per-line output so minified files cannot explode the result.
		"--max-columns", "500",
		"--max-columns-preview",
		"-C", strconv.Itoa(contextLines),
		"--glob", rgSkipGlob,
	}
	if filePattern != "" {
		args = append(args, "--glob", filePattern)
	}
	args = append(args, "--", pattern, path)

	cmd := exec.CommandContext(ctx, rg, args...)
	shell.HideConsole(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("rg pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, false, err
		}
		// Missing binary, bad flag, disk error: let the caller fall back to
		// the pure-Go implementation.
		return nil, false, fmt.Errorf("rg start failed: %w", err)
	}

	// Stream-parse rg's JSON while it runs. When the result cap is reached
	// the parser calls onCap and we kill rg mid-scan instead of letting it
	// finish the whole repository — on broad patterns (thousands of matches
	// against a 500-result cap) this skips most of the scan and most of the
	// parse, which dominates end-to-end latency.
	killed := false
	res := parseRipgrepStream(stdout, func() {
		killed = true
		_ = cmd.Process.Kill()
	})

	waitErr := cmd.Wait()

	if killed {
		return res, true, nil
	}
	if waitErr != nil {
		if errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, context.DeadlineExceeded) {
			return nil, false, waitErr
		}
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 {
			// rg exit code 1 = "no matches": a valid empty result.
			return &SearchFilesOutput{Results: []SearchResult{}}, true, nil
		}
		// Anything else: let the caller fall back to the pure-Go implementation.
		return nil, false, fmt.Errorf("rg failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}

	return res, true, nil
}

// rgEvent is a single line of `rg --json` output.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Match struct {
				Text string `json:"text"`
			} `json:"match"`
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

// rgMatchRef is one reported match awaiting its context assembly at file end.
type rgMatchRef struct {
	line int
	col  int // 1-based rune column
	text string
}

// rgPending accumulates per-file state between rg's begin/end events;
// context lines following a match only arrive later in the stream.
type rgPending struct {
	path    string
	lines   map[int]string
	matches []rgMatchRef
}

// parseRipgrepOutput converts a complete `rg --json` output string into
// SearchFilesOutput. Thin wrapper kept for tests; the live path is
// parseRipgrepStream.
func parseRipgrepOutput(out string) *SearchFilesOutput {
	return parseRipgrepStream(strings.NewReader(out), nil)
}

// parseRipgrepStream converts `rg --json` output read from r into
// SearchFilesOutput, parsing line by line as the data arrives so parsing
// overlaps rg's execution. rg emits begin -> (context|match)* -> end per
// file in order, so trailing context is available by the time each file's
// end event flushes it.
//
// Once len(Results) reaches maxSearchResults the parser truncates to the
// cap and calls onCap exactly once, then stops reading — the caller is
// expected to kill the rg process so it stops scanning the rest of the
// repository. onCap may be nil (tests / complete output).
func parseRipgrepStream(r io.Reader, onCap func()) *SearchFilesOutput {
	res := &SearchFilesOutput{}
	var cur *rgPending

	flush := func() {
		if cur == nil {
			return
		}
		for _, m := range cur.matches {
			res.Results = append(res.Results, buildRipgrepResult(cur, m))
		}
		if len(cur.matches) > 0 {
			res.TotalFiles++
		}
		cur = nil
	}

	stop := func() bool {
		if len(res.Results) < maxSearchResults {
			return false
		}
		// Cap reached: present the same shape Execute's post-cap would
		// (exactly maxSearchResults results, TotalMatches clamped to match).
		res.Results = res.Results[:maxSearchResults]
		res.TotalMatches = len(res.Results)
		if onCap != nil {
			onCap()
		}
		return true
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 256*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev rgEvent
		if json.Unmarshal(line, &ev) != nil {
			continue // skip malformed/truncated lines
		}
		switch ev.Type {
		case "begin":
			flush()
			cur = &rgPending{path: ev.Data.Path.Text, lines: map[int]string{}}
		case "match":
			if cur == nil {
				continue
			}
			text := trimEOL(ev.Data.Lines.Text)
			cur.lines[ev.Data.LineNumber] = text
			// Expand every submatch into its own result, mirroring the Go
			// fallback which reports one result per in-line occurrence.
			for _, sm := range ev.Data.Submatches {
				col := 1
				if sm.Start >= 0 && sm.Start <= len(text) {
					col = utf8.RuneCountInString(text[:sm.Start]) + 1
				}
				cur.matches = append(cur.matches, rgMatchRef{
					line: ev.Data.LineNumber, col: col, text: sm.Match.Text,
				})
				res.TotalMatches++
			}
		case "context":
			if cur == nil {
				continue
			}
			cur.lines[ev.Data.LineNumber] = trimEOL(ev.Data.Lines.Text)
		case "end":
			flush()
			if stop() {
				return res
			}
		}
	}
	flush() // input truncated without a final end event
	return res
}

// trimEOL strips the trailing newline rg includes in line text (and the CR
// of CRLF files).
func trimEOL(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// buildRipgrepResult assembles the context block for one match, using the
// exact same "> N: line" format as the pure-Go fallback.
func buildRipgrepResult(acc *rgPending, m rgMatchRef) SearchResult {
	ctxStart := m.line - contextLines
	if ctxStart < 1 {
		ctxStart = 1
	}
	ctxEnd := m.line + contextLines
	// Clamp the window's end to lines rg actually reported (the match may
	// sit at EOF, or --max-columns/preview may omit far-away lines).
	for ctxEnd > m.line {
		if _, ok := acc.lines[ctxEnd]; ok {
			break
		}
		ctxEnd--
	}

	var b strings.Builder
	for j := ctxStart; j <= ctxEnd; j++ {
		text, ok := acc.lines[j]
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if j == m.line {
			b.WriteString("> ")
		} else {
			b.WriteString("  ")
		}
		b.WriteString(strconv.Itoa(j))
		b.WriteString(": ")
		b.WriteString(text)
	}

	return SearchResult{
		Path:        acc.path,
		Line:        m.line,
		Column:      m.col,
		Match:       m.text,
		Context:     b.String(),
		ContextLine: m.line - ctxStart + 1,
	}
}
