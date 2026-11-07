package docparse

// parser_legacy.go extracts text from OLE compound files (the shared
// D0 CF 11 E0 magic): Word 97-2003 (.doc/.dot) via go-word-extractor,
// Excel 97-2003 (.xls/.xlt) via Clownsw/xls, and PowerPoint 97-2003
// (.ppt/.pps/.pot) via a best-effort printable-run recovery (no maintained
// pure-Go PPT parser exists).

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Clownsw/xls"
	word_extractor "github.com/WayneKent/go-word-extractor/pkg/word-extractor"
)

// parseLegacyOffice routes an OLE container by extension (.doc/.xls/.ppt
// share one magic, so the name is the only discriminator).
func parseLegacyOffice(path string, data []byte) (string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".doc", ".dot":
		return extractWordDoc(data)
	case ".xls", ".xlt":
		return extractXLS(data)
	case ".ppt", ".pps", ".pot":
		if text := bestEffortText(data); text != "" {
			return text, nil
		}
		return "", fmt.Errorf("%w: legacy PowerPoint (%s) yielded no readable text", ErrNoTextLayer, filepath.Ext(path))
	}
	return "", fmt.Errorf("%w: OLE compound file with unsupported extension %q (expected .doc/.xls/.ppt …)",
		ErrNoTextLayer, baseOrExt(path))
}

// extractWordDoc pulls the body (plus headers/footnotes when the body is
// empty) out of a Word 97-2003 file.
func extractWordDoc(data []byte) (string, error) {
	doc, err := word_extractor.NewWordExtractor().Extract(data)
	if err != nil {
		return "", fmt.Errorf("doc extract: %w", err)
	}
	text := strings.TrimSpace(doc.GetBody(nil))
	if text == "" {
		text = strings.TrimSpace(doc.GetHeaders(nil))
	}
	if text == "" {
		text = strings.TrimSpace(doc.GetFootnotes(nil))
	}
	if text == "" {
		return "", fmt.Errorf("%w: .doc has no readable text", ErrNoTextLayer)
	}
	return text, nil
}

// extractXLS flattens every sheet of an Excel 97-2003 workbook into
// tab-separated rows (one "[sheet: name]" header per sheet).
func extractXLS(data []byte) (string, error) {
	wb, err := xls.OpenReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("xls extract: %w", err)
	}
	var b strings.Builder
	for i := 0; i < wb.NumSheets(); i++ {
		sheet := wb.GetSheet(i)
		if sheet == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[sheet: %s]\n", sheet.Name)
		for r := 0; r <= int(sheet.MaxRow); r++ {
			row := sheet.Row(r)
			if row == nil {
				continue
			}
			cells := make([]string, 0, 8)
			for c := row.FirstCol(); c < row.LastCol(); c++ {
				cells = append(cells, row.Col(c))
			}
			if line := strings.Join(cells, "\t"); strings.TrimSpace(line) != "" {
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return "", fmt.Errorf("%w: .xls has no readable text", ErrNoTextLayer)
	}
	return text, nil
}

// bestEffortText recovers readable runs from a binary container. Two
// strategies, chosen by detecting which text encoding dominates:
//
//   - UTF-16LE (legacy PowerPoint stores slide text this way) — detected by
//     the density of printable-ASCII chars whose high byte is 0;
//   - single-byte ASCII/CP1252 — printable runs of 4+ chars.
//
// Quality is best-effort — some structural junk may remain, but it beats an
// outright "unsupported" for the agent.
func bestEffortText(data []byte) string {
	// Detect UTF-16LE: >25% of 2-byte pairs must look like ASCII-in-UTF16
	// (printable low byte + zero high byte). CP1252 text has no zero bytes,
	// CFB padding rarely reaches that density.
	pairs, hiZeroPrint := 0, 0
	for i := 0; i+1 < len(data); i += 2 {
		pairs++
		if data[i] >= 0x20 && data[i] < 0x7f && data[i+1] == 0 {
			hiZeroPrint++
		}
	}
	if pairs > 16 && hiZeroPrint*4 > pairs {
		return utf16Runs(data)
	}
	return asciiRuns(data)
}

func utf16Runs(data []byte) string {
	var b strings.Builder
	var run []rune
	flush := func() {
		writeRun(&b, run)
		run = run[:0]
	}
	for i := 0; i+1 < len(data); i += 2 {
		v := rune(data[i]) | rune(data[i+1])<<8
		if v >= 0x20 && v != 0xFFFE && v != 0xFEFF && unicode.IsPrint(v) {
			run = append(run, v)
		} else {
			flush()
		}
	}
	flush()
	return strings.TrimSpace(b.String())
}

func asciiRuns(data []byte) string {
	var b strings.Builder
	var run []rune
	flush := func() {
		writeRun(&b, run)
		run = run[:0]
	}
	for _, ch := range data {
		if ch >= 0x20 && ch < 0x7f {
			run = append(run, rune(ch))
		} else {
			flush()
		}
	}
	flush()
	return strings.TrimSpace(b.String())
}

// writeRun appends a run when it is long enough and carries at least one
// letter or digit (filters structural junk like "Root Entry").
func writeRun(b *strings.Builder, run []rune) {
	if len(run) < 4 {
		return
	}
	hasAlnum := false
	for _, r := range run {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasAlnum = true
			break
		}
	}
	if !hasAlnum {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(string(run))
}
