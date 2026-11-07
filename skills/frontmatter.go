package skills

import (
	"fmt"
	"strings"
)

// parseFrontmatter extracts the YAML-ish frontmatter key/value pairs from a
// SKILL.md body. It intentionally implements only the subset the Agent
// Skills specification uses for routing: `key: value` scalars (optionally
// quoted), block scalars (`>` / `|` with optional `-` chomping) and simple
// nested indentation folded into the parent key. No external YAML library —
// the package stays dependency-free like the rest of cobot.
//
// The frontmatter block must open the file with a `---` line and close with
// `---` (or `...`). Anything outside the block is ignored.
func parseFrontmatter(content string) (map[string]string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("missing frontmatter (file must start with ---)")
	}

	meta := make(map[string]string)
	var currentKey string
	var blockMode byte // 0 = none, '>' = folded, '|' = literal
	var blockIndent int
	var blockLines []string

	flushBlock := func() {
		if currentKey == "" || blockMode == 0 {
			return
		}
		text := strings.Join(blockLines, "\n")
		if blockMode == '>' {
			// folded: join with spaces, blank lines become newlines
			text = foldLines(blockLines)
		}
		meta[currentKey] = strings.TrimRight(text, "\n")
		blockMode = 0
		blockLines = nil
	}

	closed := false
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if trimmed == "---" || trimmed == "..." {
			flushBlock()
			closed = true
			break
		}

		if blockMode != 0 {
			// Inside a block scalar: content must be indented past the key.
			if trimmed == "" {
				blockLines = append(blockLines, "")
				continue
			}
			indent := indentOf(line)
			if indent > blockIndent {
				blockLines = append(blockLines, trimmed)
				continue
			}
			flushBlock()
			// fall through: this line is a normal key line
		}

		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Nested line (indented continuation): fold into the previous value.
		if indentOf(line) > 0 && currentKey != "" && !strings.Contains(trimmed, ": ") && !strings.HasSuffix(trimmed, ":") {
			meta[currentKey] = strings.TrimSpace(meta[currentKey] + " " + trimmed)
			continue
		}

		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue // not a key line; ignore
		}
		key := strings.TrimSpace(trimmed[:colon])
		if key == "" {
			continue
		}
		value := strings.TrimSpace(trimmed[colon+1:])

		// Block scalar indicator: content follows on subsequent indented lines.
		if value == ">" || value == ">-" || value == "|" || value == "|-" {
			blockMode = value[0]
			blockIndent = indentOf(line)
			currentKey = key
			blockLines = nil
			continue
		}

		currentKey = key
		meta[key] = unquote(value)
	}

	if !closed {
		return nil, fmt.Errorf("unterminated frontmatter (missing closing ---)")
	}
	return meta, nil
}

// foldLines joins YAML folded-scalar (>) lines: single newlines become
// spaces, blank lines become newlines.
func foldLines(lines []string) string {
	var b strings.Builder
	pendingNewline := false
	for _, l := range lines {
		if l == "" {
			pendingNewline = true
			continue
		}
		if b.Len() > 0 {
			if pendingNewline {
				b.WriteString("\n")
			} else {
				b.WriteString(" ")
			}
		}
		b.WriteString(l)
		pendingNewline = false
	}
	return b.String()
}

// unquote strips matching single or double quotes from a scalar value.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// indentOf counts leading spaces on a line.
func indentOf(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}
