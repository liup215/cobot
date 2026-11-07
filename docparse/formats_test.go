package docparse

// parser_formats_test.go covers the content-first dispatch registry: legacy
// Office (OLE), RTF, OpenDocument, EPUB, archive listings, plain-text sniff
// and the ErrNoTextLayer contract. Fixtures live in testdata/ (lib-generated
// samples from the upstream parsers' own test suites).

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Skipf("fixture %s missing: %v", name, err)
	}
	return data
}

func TestExtractTextLegacyDOC(t *testing.T) {
	data := fixture(t, "legacy.doc")
	got, err := ExtractText("report.doc", data)
	if err != nil {
		t.Fatalf("ExtractText(.doc): %v", err)
	}
	if len(got) < 20 {
		t.Errorf("doc text too short: %q", got)
	}
	if strings.Contains(got, "\x00") {
		t.Error("doc text contains NUL bytes")
	}
}

func TestExtractTextLegacyXLS(t *testing.T) {
	data := fixture(t, "legacy.xls")
	got, err := ExtractText("book.xls", data)
	if err != nil {
		t.Fatalf("ExtractText(.xls): %v", err)
	}
	if !strings.Contains(got, "[sheet: ") {
		t.Errorf("missing sheet header: %.200q", got)
	}
	if len(got) < 30 {
		t.Errorf("xls text too short: %q", got)
	}
}

func TestExtractTextRTF(t *testing.T) {
	data := fixture(t, "sample.rtf")
	got, err := ExtractText("notes.rtf", data)
	if err != nil {
		t.Fatalf("ExtractText(.rtf): %v", err)
	}
	if !strings.Contains(got, "Adipiscing") {
		t.Errorf("rtf body missing: %.200q", got)
	}
	if strings.Contains(got, `\par`) || strings.Contains(got, `\fs28`) {
		t.Errorf("rtf control words leaked into text: %.200q", got)
	}
}

func mkZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTextODT(t *testing.T) {
	data := mkZip(t, map[string]string{
		"mimetype":    "application/vnd.oasis.opendocument.text",
		"content.xml": `<?xml version="1.0"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><office:body><office:text><text:p>OpenDocument paragraph one</text:p><text:p>Paragraph two</text:p></office:text></office:body></office:document-content>`,
	})
	got, err := ExtractText("doc.odt", data)
	if err != nil {
		t.Fatalf("ExtractText(.odt): %v", err)
	}
	if !strings.Contains(got, "OpenDocument paragraph one") || !strings.Contains(got, "Paragraph two") {
		t.Errorf("odt text missing: %q", got)
	}
}

func TestExtractTextEPUB(t *testing.T) {
	data := mkZip(t, map[string]string{
		"mimetype":       "application/epub+zip",
		"chapter1.xhtml": `<html><body><h1>Chapter One</h1><p>EPUB body text lives here.</p></body></html>`,
	})
	got, err := ExtractText("book.epub", data)
	if err != nil {
		t.Fatalf("ExtractText(.epub): %v", err)
	}
	if !strings.Contains(got, "Chapter One") || !strings.Contains(got, "EPUB body text lives here.") {
		t.Errorf("epub text missing: %q", got)
	}
}

func TestExtractTextUnknownZipListsEntries(t *testing.T) {
	data := mkZip(t, map[string]string{"papers/a.txt": "hi"})
	got, err := ExtractText("bundle.zip", data)
	if err != nil {
		t.Fatalf("ExtractText(.zip): %v", err)
	}
	if !strings.Contains(got, "[archive listing:") || !strings.Contains(got, "papers/a.txt") {
		t.Errorf("unexpected listing: %q", got)
	}
}

func TestExtractTextPlainSniffAnyName(t *testing.T) {
	// No extension at all — the sniff must accept it.
	got, err := ExtractText("Makefile", []byte("all:\n\tgo build\n"))
	if err != nil {
		t.Fatalf("Makefile: %v", err)
	}
	if !strings.Contains(got, "go build") {
		t.Errorf("Makefile content missing: %q", got)
	}
	// CSV / log style text with extensions outside the old whitelist.
	if _, err := ExtractText("data.csv", []byte("a,b,c\n1,2,3\n")); err != nil {
		t.Errorf("csv: %v", err)
	}
	if _, err := ExtractText("run.log", []byte("2026-11-07 ok\n")); err != nil {
		t.Errorf("log: %v", err)
	}
}

func TestExtractTextHTMLStripsTags(t *testing.T) {
	got, err := ExtractText("page.html", []byte("<html><body><h1>Title</h1><p>Hello <b>world</b></p></body></html>"))
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if strings.Contains(got, "<p>") || !strings.Contains(got, "Hello world") {
		t.Errorf("html not stripped: %q", got)
	}
}

func TestExtractTextBinaryIsErrNoTextLayer(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00, 0x01, 0x02}, 32)...)
	_, err := ExtractText("image.png", png)
	if !errors.Is(err, ErrNoTextLayer) {
		t.Fatalf("want ErrNoTextLayer, got %v", err)
	}
}

func TestNeedsExtractionMagic(t *testing.T) {
	cases := []struct {
		head string
		want bool
	}{
		{"%PDF-1.4", true},
		{`{\rtf1\ansi`, true},
		{"PK\x03\x04rest", true},
		{"\xD0\xCF\x11\xE0\xA1\xB1\x1A\xE1rest", true},
		{"plain text head", false},
		{"\x89PNG\r\n", false},
	}
	for _, c := range cases {
		if got := NeedsExtraction([]byte(c.head)); got != c.want {
			t.Errorf("NeedsExtraction(%q) = %v, want %v", c.head, got, c.want)
		}
	}
}
