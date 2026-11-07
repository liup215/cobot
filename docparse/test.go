package docparse

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

func TestParseDOCXInsertsSpacesBetweenRuns(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p>
      <w:r><w:t>Hello</w:t></w:r>
      <w:r><w:t>World</w:t></w:r>
    </w:p>
    <w:p>
      <w:r><w:t>This is a</w:t></w:r>
      <w:r><w:t>test.</w:t></w:r>
    </w:p>
  </w:body>
</w:document>`

	text, err := parseDOCX(mkDocx(xml))
	if err != nil {
		t.Fatalf("parseDOCX: %v", err)
	}
	if !strings.Contains(text, "Hello World") {
		t.Errorf("expected space between runs, got: %q", text)
	}
	if !strings.Contains(text, "This is a test.") {
		t.Errorf("expected space between runs, got: %q", text)
	}
}

func TestParsePPTXInsertsSpacesBetweenRuns(t *testing.T) {
	xml := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
  <p:sp>
    <a:p>
      <a:r><a:t>Hello</a:t></a:r>
      <a:r><a:t>World</a:t></a:r>
    </a:p>
  </p:sp>
</p:sld>`

	text, err := parsePPTX(mkPptx(xml))
	if err != nil {
		t.Fatalf("parsePPTX: %v", err)
	}
	if !strings.Contains(text, "Hello World") {
		t.Errorf("expected space between runs, got: %q", text)
	}
}

func mkDocx(documentXML string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("word/document.xml")
	f.Write([]byte(documentXML))
	zw.Close()
	return buf.Bytes()
}

func mkPptx(slideXML string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("ppt/slides/slide1.xml")
	f.Write([]byte(slideXML))
	zw.Close()
	return buf.Bytes()
}
