package docparse

// parser_zip.go routes ZIP-based containers to the right extractor by
// inspecting their members — content-first, independent of the file name.
// Covers OOXML (.docx/.xlsx/.pptx + macro-enabled/template variants),
// OpenDocument (.odt/.ods/.odp), EPUB, and falls back to an entry listing
// for any other archive.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/attilabuti/striprtf"
)

// parseRTF strips RTF control words via striprtf (shared by KB import and
// the chat read tool).
func parseRTF(data []byte) (string, error) {
	r, err := striprtf.ExtractText(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("rtf: %w", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("rtf: %w", err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("%w: rtf has no text", ErrNoTextLayer)
	}
	return text, nil
}

func parseZipContainer(path string, data []byte) (string, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("zip open: %w", err)
	}
	has := func(name string) bool {
		for _, f := range r.File {
			if f.Name == name {
				return true
			}
		}
		return false
	}
	anyPrefix := func(prefix string) bool {
		for _, f := range r.File {
			if strings.HasPrefix(f.Name, prefix) {
				return true
			}
		}
		return false
	}

	mimetype := zipMemberText(r, "mimetype")
	switch {
	case strings.HasPrefix(mimetype, "application/epub+zip"):
		return parseEPUB(r)
	case has("word/document.xml"):
		return parseDOCX(data)
	case has("xl/workbook.xml") || anyPrefix("xl/worksheets/"):
		return parseXLSX(path) // reads the container from disk
	case has("ppt/presentation.xml"):
		return parsePPTX(data)
	case has("content.xml"):
		return parseODF(r)
	}
	return archiveListing(r), nil
}

// parseODF extracts text from an OpenDocument container (.odt/.ods/.odp all
// carry their text in content.xml; table cells become one line each so
// spreadsheets stay readable).
func parseODF(r *zip.Reader) (string, error) {
	f, err := r.Open("content.xml")
	if err != nil {
		return "", fmt.Errorf("opendocument missing content.xml: %w", err)
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	var out textWriter
	var pending string
	inPara := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("opendocument xml decode: %w", err)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			switch se.Name.Local {
			case "p", "h":
				inPara = true
				pending = ""
			}
		case xml.EndElement:
			switch se.Name.Local {
			case "p", "h":
				inPara = false
				out.write(pending)
				pending = ""
				out.newline()
			case "table-cell", "table-row":
				out.newline()
			}
		case xml.CharData:
			if inPara {
				pending += string(se)
			}
		}
	}
	text := out.String()
	if text == "" {
		return "", fmt.Errorf("%w: opendocument has no text", ErrNoTextLayer)
	}
	return text, nil
}

// parseEPUB concatenates the stripped text of all XHTML/HTML chapters (in
// file-name order) — enough for indexing and for the agent to read.
func parseEPUB(r *zip.Reader) (string, error) {
	var names []string
	for _, f := range r.File {
		lower := strings.ToLower(f.Name)
		if strings.HasPrefix(lower, "meta-inf/") {
			continue
		}
		if strings.HasSuffix(lower, ".xhtml") || strings.HasSuffix(lower, ".html") || strings.HasSuffix(lower, ".htm") {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		rc, err := r.Open(name)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}
		if text := stripHTML(string(data)); text != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: epub has no text", ErrNoTextLayer)
	}
	return b.String(), nil
}

// archiveListing is the fallback for unknown ZIP archives: the agent at least
// learns what is inside instead of getting a hard error.
func archiveListing(r *zip.Reader) string {
	const maxEntries = 200
	var b strings.Builder
	fmt.Fprintf(&b, "[archive listing: %d entries]\n", len(r.File))
	for i, f := range r.File {
		if i == maxEntries {
			fmt.Fprintf(&b, "… and %d more entries\n", len(r.File)-maxEntries)
			break
		}
		name := f.Name
		if len(name) > 200 {
			name = name[:200] + "…"
		}
		fmt.Fprintf(&b, "%s (%d bytes)\n", name, f.UncompressedSize64)
	}
	return strings.TrimSpace(b.String())
}

// zipMemberText reads a small text member (used for the ODF/EPUB mimetype).
func zipMemberText(r *zip.Reader, name string) string {
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		data, err := io.ReadAll(io.LimitReader(rc, 256))
		rc.Close()
		if err != nil {
			return ""
		}
		return string(data)
	}
	return ""
}
