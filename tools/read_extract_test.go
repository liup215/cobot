package tools

// read_extract_test.go pins the document-extraction path of `read`: the
// docparse registry (PDF/Office/RTF/…) is wired into the line-paged reader,
// replacing the desktop's old read_local_file tool.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runReadTool(t *testing.T, input map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return NewReadFileTool().Execute(context.Background(), raw)
}

// buildPDF assembles a minimal single-page PDF with a correct xref table.
func buildPDF(t *testing.T, text string) []byte {
	t.Helper()
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefPos := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xrefPos)
	return buf.Bytes()
}

// buildDocx assembles a minimal valid DOCX (zip + word/document.xml).
func buildDocx(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>%s</w:t></w:r></w:p></w:body></w:document>`, text)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadExtractsPDF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.pdf")
	if err := os.WriteFile(path, buildPDF(t, "Photosynthesis converts light into chemical energy."), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if !strings.Contains(out, "text extracted from .pdf") {
		t.Errorf("missing extraction marker in header: %.200s", out)
	}
	if !strings.Contains(out, "Photosynthesis converts light into chemical energy.") {
		t.Errorf("pdf text not extracted: %q", out)
	}
	if strings.Contains(out, "[binary file:") {
		t.Error("PDF must not fall back to the binary notice")
	}
}

func TestReadExtractsDOCX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.docx")
	if err := os.WriteFile(path, buildDocx(t, "Quarterly revenue grew 12 percent."), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read docx: %v", err)
	}
	if !strings.Contains(out, "text extracted from .docx") {
		t.Errorf("missing extraction marker: %.200s", out)
	}
	if !strings.Contains(out, "Quarterly revenue grew 12 percent.") {
		t.Errorf("docx text not extracted: %q", out)
	}
}

// TestReadExtractsLegacyDOC exercises a real Word 97-2003 binary (OLE magic →
// docparse legacy branch → go-word-extractor).
func TestReadExtractsLegacyDOC(t *testing.T) {
	path := filepath.Join("..", "docparse", "testdata", "legacy.doc")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("legacy.doc fixture missing: %v", err)
	}
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read .doc: %v", err)
	}
	if !strings.Contains(out, "text extracted from .doc") {
		t.Errorf("missing extraction marker: %.200s", out)
	}
	if strings.Contains(out, "[binary file:") {
		t.Error("legacy .doc must not fall back to the binary notice")
	}
}

// TestReadBinaryNotices: images keep gline's metadata notice; unknown
// binaries return the docparse no-text-layer notice as a RESULT (the model
// must not retry) — never binary garbage in either case.
func TestReadBinaryNotices(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), 0x00, 0x01, 0x02), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadTool(t, map[string]any{"path": png})
	if err != nil {
		t.Fatalf("read png: %v", err)
	}
	if !strings.Contains(out, "[Image file:") {
		t.Errorf("image metadata notice missing: %q", out)
	}
	if strings.Contains(out, "\x89PNG") {
		t.Error("raw image bytes leaked into the result")
	}

	blob := filepath.Join(dir, "blob.dat")
	if err := os.WriteFile(blob, []byte{0x00, 0xFF, 0xFE, 0x01, 0x02, 0x03}, 0644); err != nil {
		t.Fatal(err)
	}
	out, err = runReadTool(t, map[string]any{"path": blob})
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !strings.Contains(out, "[binary file:") || !strings.Contains(out, "no text layer") {
		t.Errorf("want the binary notice, got: %q", out)
	}
}

// TestReadExtractedTextIsLinePaged: line_number indexes the extracted text,
// so the grep → read → edit workflow keeps working on documents.
func TestReadExtractedTextIsLinePaged(t *testing.T) {
	// A docx with several paragraphs → several extracted lines.
	var body strings.Builder
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&body, "<w:p><w:r><w:t>paragraph %d</w:t></w:r></w:p>", i)
	}
	path := filepath.Join(t.TempDir(), "paged.docx")
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("word/document.xml")
	w.Write([]byte(xml))
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	out, err := runReadTool(t, map[string]any{"path": path, "line_number": 2, "limit": 2})
	if err != nil {
		t.Fatalf("read paged docx: %v", err)
	}
	if !strings.Contains(out, "paragraph 2") || !strings.Contains(out, "paragraph 3") {
		t.Errorf("page starting at line 2 wrong: %q", out)
	}
	if strings.Contains(out, "paragraph 1") || strings.Contains(out, "paragraph 4") {
		t.Errorf("limit=2 leaked lines outside the window: %q", out)
	}
	if !strings.Contains(out, "text extracted from .docx") {
		t.Errorf("extraction marker missing: %q", out)
	}
}
