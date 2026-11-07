// parser.go provides document parsing for supported file types using pure Go libraries.
package docparse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/razvandimescu/gopdf/pdf"
)

// ErrNoTextLayer reports that a file yielded no readable text — an
// unsupported binary container, an image, a scanned PDF… Callers (the chat
// read tool, KB import) surface it as a friendly notice instead of
// a hard failure, and tools must not retry the same file.
var ErrNoTextLayer = errors.New("no text layer")

// textWriter joins text runs with a single space when needed so that words
// from adjacent runs do not get glued together.
type textWriter struct {
	b    strings.Builder
	last byte // 0 means empty
}

func (w *textWriter) write(s string) {
	if s == "" {
		return
	}
	if w.last != 0 && w.last != ' ' && w.last != '\n' && w.last != '\t' && s[0] != ' ' {
		w.b.WriteByte(' ')
	}
	w.b.WriteString(s)
	w.last = s[len(s)-1]
}

func (w *textWriter) newline() {
	if w.last == '\n' {
		return
	}
	w.b.WriteByte('\n')
	w.last = '\n'
}

func (w *textWriter) tab() {
	if w.last == '\t' {
		return
	}
	w.b.WriteByte('\t')
	w.last = '\t'
}

func (w *textWriter) String() string { return strings.TrimSpace(w.b.String()) }

// ParseDocument reads a file and returns plain text for indexing (knowledge
// base import) and for the read tool's document-extraction path.
//
// Dispatch is CONTENT-FIRST (magic bytes) so renamed or extension-less files
// still route to the right extractor; the extension only disambiguates
// formats whose bytes look like plain text (HTML/RTF) and legacy OLE
// containers (.doc/.xls/.ppt share one magic):
//
//	%PDF              → gopdf text extraction
//	{\rtf             → striprtf control-word stripping
//	PK\x03\x04        → zip container → OOXML / OpenDocument / EPUB /
//	                    archive entry listing
//	D0 CF 11 E0 …     → OLE compound file → legacy .doc / .xls / .ppt
//	.html/.htm/.xhtml → tag stripping
//	else              → plain-text sniff: ANY text/code/data format with any
//	                    or no extension (.csv .log .sql .tex .vue Makefile …)
//	otherwise         → ErrNoTextLayer
func ParseDocument(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return ExtractText(path, data)
}

// ExtractText is ParseDocument for in-memory bytes (also used by tests).
func ExtractText(path string, data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, pdfMagic):
		return parsePDF(path)
	case bytes.HasPrefix(data, rtfMagic):
		return parseRTF(data)
	case bytes.HasPrefix(data, zipMagic):
		return parseZipContainer(path, data)
	case bytes.HasPrefix(data, cfbMagic):
		return parseLegacyOffice(path, data)
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".html", ".htm", ".xhtml":
		return stripHTML(string(data)), nil
	}
	if LooksLikePlainText(data) {
		return string(data), nil
	}
	return "", fmt.Errorf("%w: unsupported binary file (%s)", ErrNoTextLayer, baseOrExt(path))
}

// Magic bytes shared with the chat read tool (tools/file_tools.go) so both
// sides decide "needs extraction" identically.
var (
	pdfMagic = []byte("%PDF")
	rtfMagic = []byte(`{\rtf`)
	zipMagic = []byte("PK\x03\x04")
	cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
)

// NeedsExtraction reports whether a file head requires document extraction
// (PDF/RTF/ZIP/OLE) rather than plain byte paging.
func NeedsExtraction(head []byte) bool {
	return bytes.HasPrefix(head, pdfMagic) ||
		bytes.HasPrefix(head, rtfMagic) ||
		bytes.HasPrefix(head, zipMagic) ||
		bytes.HasPrefix(head, cfbMagic)
}

// LooksLikePlainText reports whether data is safe to treat as text: valid
// UTF-8, no NUL bytes and not mostly control characters.
func LooksLikePlainText(data []byte) bool {
	if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return false
	}
	ctrl := 0
	for _, b := range data {
		if b < 0x09 || (b > 0x0d && b < 0x20) {
			ctrl++
		}
	}
	return ctrl*20 <= len(data)
}

func baseOrExt(path string) string {
	if e := filepath.Ext(path); e != "" {
		return e
	}
	return filepath.Base(path)
}

// parsePDF extracts plain text from a PDF file via gopdf (pure Go).
// gopdf reconstructs text lines and handles intra-word spacing internally,
// avoiding the per-letter spacing problems seen with coordinate-based parsers.
func parsePDF(path string) (string, error) {
	doc, err := pdf.OpenFile(path)
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}
	var all strings.Builder
	for i := 0; i < doc.NumPages(); i++ {
		page := doc.Page(i)
		lines, err := page.TextLines()
		if err != nil {
			return "", fmt.Errorf("extract page %d: %w", i+1, err)
		}
		for _, line := range lines {
			if line.Text == "" {
				continue
			}
			if all.Len() > 0 {
				all.WriteByte('\n')
			}
			all.WriteString(line.Text)
		}
		if i < doc.NumPages()-1 && len(lines) > 0 {
			all.WriteByte('\n')
		}
	}
	result := strings.TrimSpace(all.String())
	if result == "" {
		return "", fmt.Errorf("%w: no text extracted from pdf: %s", ErrNoTextLayer, path)
	}
	return result, nil
}

// parseDOCX extracts plain text from a DOCX (OOXML) file.
func parseDOCX(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx open: %w", err)
	}
	f, err := r.Open("word/document.xml")
	if err != nil {
		return "", fmt.Errorf("docx missing document.xml: %w", err)
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	var out textWriter
	var inText bool
	var pending string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("docx xml decode: %w", err)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if se.Name.Local == "t" {
				inText = true
				pending = ""
			}
		case xml.EndElement:
			if se.Name.Local == "t" {
				inText = false
				out.write(pending)
				pending = ""
			}
			if se.Name.Local == "p" {
				out.newline()
			}
		case xml.CharData:
			if inText {
				pending += string(se)
			}
		}
	}
	return out.String(), nil
}

// parseXLSX extracts plain text from an XLSX file using only archive/zip + encoding/xml.
func parseXLSX(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("xlsx open: %w", err)
	}

	shared := []string{}
	if f, err := r.Open("xl/sharedStrings.xml"); err == nil {
		defer f.Close()
		dec := xml.NewDecoder(f)
		var inText bool
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			switch se := tok.(type) {
			case xml.StartElement:
				if se.Name.Local == "t" {
					inText = true
				}
			case xml.EndElement:
				if se.Name.Local == "t" {
					inText = false
				}
				if se.Name.Local == "si" {
					shared = append(shared, "")
				}
			case xml.CharData:
				if inText && len(shared) > 0 {
					shared[len(shared)-1] += string(se)
				}
			}
		}
	}

	var out textWriter
	for _, sheetFile := range r.File {
		if !strings.HasPrefix(sheetFile.Name, "xl/worksheets/sheet") || !strings.HasSuffix(sheetFile.Name, ".xml") {
			continue
		}
		rc, err := sheetFile.Open()
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(rc)
		var inText bool
		var val strings.Builder
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			switch se := tok.(type) {
			case xml.StartElement:
				if se.Name.Local == "v" || se.Name.Local == "t" {
					inText = true
					val.Reset()
				}
			case xml.EndElement:
				if se.Name.Local == "v" || se.Name.Local == "t" {
					inText = false
					text := val.String()
					if n, err := strconv.Atoi(text); err == nil && n >= 0 && n < len(shared) {
						text = shared[n]
					}
					if text != "" {
						out.write(text)
						out.tab()
					}
				}
				if se.Name.Local == "c" {
					out.newline()
				}
			case xml.CharData:
				if inText {
					val.Write(se)
				}
			}
		}
		rc.Close()
		out.newline()
	}
	return out.String(), nil
}

// parsePPTX extracts plain text from a PPTX file.
func parsePPTX(data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("pptx open: %w", err)
	}

	var out textWriter
	for _, f := range r.File {
		if !strings.HasPrefix(f.Name, "ppt/slides/slide") || !strings.HasSuffix(f.Name, ".xml") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(rc)
		var inText bool
		var pending string
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			switch se := tok.(type) {
			case xml.StartElement:
				if se.Name.Local == "t" {
					inText = true
					pending = ""
				}
			case xml.EndElement:
				if se.Name.Local == "t" {
					inText = false
					out.write(pending)
					pending = ""
				}
				if se.Name.Local == "p" {
					out.newline()
				}
			case xml.CharData:
				if inText {
					pending += string(se)
				}
			}
		}
		rc.Close()
		out.newline()
	}
	return out.String(), nil
}

// stripHTML extracts visible text from HTML.
func stripHTML(s string) string {
	var out strings.Builder
	var inTag bool
	var inScript bool
	for i := 0; i < len(s); {
		if inScript {
			if idx := strings.Index(s[i:], "</script>"); idx >= 0 {
				i += idx + len("</script>")
				inScript = false
				continue
			}
			break
		}
		if s[i] == '<' {
			if strings.HasPrefix(strings.ToLower(s[i:]), "<script") {
				inScript = true
				i++
				continue
			}
			inTag = true
			i++
			continue
		}
		if s[i] == '>' {
			inTag = false
			i++
			continue
		}
		if !inTag {
			out.WriteByte(s[i])
		}
		i++
	}
	return strings.TrimSpace(out.String())
}
