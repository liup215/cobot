package tools

// read_image_test.go pins the image classification of `read` (P1 of the
// read-sees-images design): vision rasters come back as an [image: <path>]
// marker that the host provider converts into an image part, other image
// formats report metadata, and SVG takes the text path.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeImage(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadVisionImageEmitsMarker(t *testing.T) {
	cases := []struct {
		name   string
		header []byte
	}{
		{"a.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")},
		{"b.jpg", []byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00\x01")},
		{"c.gif", []byte("GIF89a\x01\x00\x01\x00\x00")},
		{"d.webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeImage(t, tc.name, tc.header)
			out, err := runReadTool(t, map[string]any{"path": path})
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			// The marker must stand alone on the first line so the provider's
			// line-based parser isolates it cleanly.
			if !strings.HasPrefix(out, "[image: "+path+"]\n") {
				t.Errorf("marker line missing/not standalone: %q", out)
			}
			if !strings.Contains(out, "attached for vision") {
				t.Errorf("caption missing: %q", out)
			}
			for _, leak := range []string{"\x89PNG", "GIF89a", "\xFF\xD8"} {
				if strings.Contains(out, leak) {
					t.Errorf("raw image bytes leaked into the result")
				}
			}
		})
	}
}

func TestReadNonVisionImageMetadata(t *testing.T) {
	// TIFF sniffs as image/tiff but no mainstream vision API accepts it.
	path := writeImage(t, "t.tiff", []byte("II\x2A\x00\x08\x00\x00\x00"))
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "[Image file:") || !strings.Contains(out, "cannot be sent to vision") {
		t.Errorf("want the non-vision metadata notice, got: %q", out)
	}
	if strings.Contains(out, "[image: ") {
		t.Errorf("non-vision format must not emit a marker: %q", out)
	}
}

func TestReadICOExtFallback(t *testing.T) {
	// Go's sniffer does not know ICO — the extension is the only signal.
	path := writeImage(t, "icon.ico", []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00})
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "cannot be sent to vision") {
		t.Errorf("want the non-vision metadata notice, got: %q", out)
	}
}

func TestReadVisionImageTooLarge(t *testing.T) {
	path := writeImage(t, "big.png", []byte("\x89PNG\r\n\x1a\n"))
	if err := os.Truncate(path, 11<<20); err != nil { // sparse, > 10MB cap
		t.Fatal(err)
	}
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "too large for the vision pipeline") {
		t.Errorf("oversized image must report metadata, got: %q", out)
	}
	if strings.Contains(out, "[image: ") {
		t.Errorf("oversized image must not emit a marker: %q", out)
	}
}

func TestReadSVGTakesTextPath(t *testing.T) {
	// SVG sniffs as XML/text: its source is genuinely readable, so it must
	// NOT be classified as an image anymore.
	src := `<svg xmlns="http://www.w3.org/2000/svg"><text>hello-label</text></svg>`
	path := writeImage(t, "diagram.svg", []byte(src))
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "hello-label") {
		t.Errorf("svg source not readable: %q", out)
	}
	if strings.Contains(out, "[image: ") || strings.Contains(out, "[Image file:") {
		t.Errorf("svg must take the text path: %q", out)
	}
}

func TestReadImageSniffBeatsExtension(t *testing.T) {
	// Content-first: text hiding behind a .png name is read as text.
	path := writeImage(t, "notes.png", []byte("hello from a mislabeled png"))
	out, err := runReadTool(t, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out, "hello from a mislabeled png") {
		t.Errorf("text content not returned: %q", out)
	}
	if strings.Contains(out, "[image: ") {
		t.Errorf("mislabeled text must not emit a marker: %q", out)
	}
}
