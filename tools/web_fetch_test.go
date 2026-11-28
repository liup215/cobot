package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liup215/cobot/websearch"
)

// fetchTool wires fetch_content + get_search_content onto ONE client, which is how
// the registry builds them (they share the store through websearch.Default()).
func fetchTool(t *testing.T, handler http.HandlerFunc) (*WebFetchTool, *GetSearchContentTool, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := websearch.NewClient(
		websearch.WithProviders(&stubProvider{name: "exa"}),
		// The fixture server is on 127.0.0.1; the gate's own tests live in
		// websearch/ssrf_test cases (fetch_test.go).
		websearch.WithFetchGuard(websearch.FetchGuardOptions{AllowLoopback: true}),
		websearch.WithStore(websearch.NewStore()),
	)
	return NewWebFetchToolWith(client), NewGetSearchContentToolWith(client), server.URL
}

type fetchPayload struct {
	ResponseID string `json:"responseId"`
	URLs       []struct {
		URL      string `json:"url"`
		Title    string `json:"title"`
		MIMEType string `json:"mimeType"`
		Status   int    `json:"status"`
		Via      string `json:"via"`
		Chars    int    `json:"chars"`
		Excerpt  string `json:"excerpt"`
		Error    string `json:"error"`
	} `json:"urls"`
	Hint string `json:"hint"`
}

func runFetch(t *testing.T, tool *WebFetchTool, input map[string]any) fetchPayload {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("fetch_content: %v", err)
	}
	var payload fetchPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return payload
}

// TestFetchToolReturnsBoundedExcerpt: the model gets a head, an id, and a hint that
// names the reader.
func TestFetchToolReturnsBoundedExcerpt(t *testing.T) {
	article := `<html><head><title>9700 syllabus</title></head><body><article><h1>Overview</h1><p>` +
		strings.Repeat("Cell structure and enzymes. ", 500) +
		`</p></article></body></html>`

	tool, _, base := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, article)
	})

	payload := runFetch(t, tool, map[string]any{"url": base + "/9700"})
	if !strings.HasPrefix(payload.ResponseID, "f_") {
		t.Fatalf("responseId = %q, want f_ prefix", payload.ResponseID)
	}
	doc := payload.URLs[0]
	if doc.Error != "" {
		t.Fatalf("error = %s", doc.Error)
	}
	if doc.Via != "html" {
		t.Errorf("via = %q", doc.Via)
	}
	if len([]rune(doc.Excerpt)) > websearch.ExcerptRunes+2 {
		t.Errorf("excerpt = %d runes, want <= %d", len([]rune(doc.Excerpt)), websearch.ExcerptRunes+2)
	}
	if doc.Chars <= websearch.ExcerptRunes {
		t.Errorf("chars = %d, want the full stored length", doc.Chars)
	}
	for _, want := range []string{"get_search_content", payload.ResponseID, "urlIndex"} {
		if !strings.Contains(payload.Hint, want) {
			t.Errorf("hint %q is missing %q", payload.Hint, want)
		}
	}
}

// TestFetchThenPageByURLIndex is the handoff the pair exists for.
func TestFetchThenPageByURLIndex(t *testing.T) {
	tool, content, base := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch r.URL.Path {
		case "/a":
			fmt.Fprint(w, "page A: "+strings.Repeat("alpha ", 300)+"END-A")
		case "/b":
			fmt.Fprint(w, "page B: "+strings.Repeat("beta ", 300)+"END-B")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	payload := runFetch(t, tool, map[string]any{"urls": []string{base + "/a", base + "/b"}})
	if payload.ResponseID == "" {
		t.Fatalf("no responseId: %+v", payload)
	}

	second := runContent(t, content, map[string]any{"responseId": payload.ResponseID, "urlIndex": 1, "limit": 50})
	if !strings.Contains(second.Content, "page B") {
		t.Fatalf("urlIndex 1 read the wrong page: %q", second.Content)
	}
	if second.NextOffset != 50 {
		t.Errorf("nextOffset = %d", second.NextOffset)
	}

	// Selecting by URL must reach the same page.
	byURL := runContent(t, content, map[string]any{"responseId": payload.ResponseID, "url": base + "/b", "limit": 50})
	if byURL.Content != second.Content {
		t.Errorf("url selection differs from urlIndex:\n%q\n%q", byURL.Content, second.Content)
	}

	// Walk to the end: END-B proves the whole page was stored.
	var all strings.Builder
	all.WriteString(second.Content)
	next := second.NextOffset
	for next > 0 {
		page := runContent(t, content, map[string]any{"responseId": payload.ResponseID, "urlIndex": 1, "offset": next, "limit": 2000})
		all.WriteString(page.Content)
		next = page.NextOffset
	}
	if !strings.Contains(all.String(), "END-B") {
		t.Errorf("stored page is truncated: %q", all.String())
	}

	// findText works on a fetched page too.
	found := runFind(t, content, map[string]any{"responseId": payload.ResponseID, "urlIndex": 1, "findText": "END-B"})
	if found.TotalMatches == 0 {
		t.Errorf("findText found nothing in a fetched page")
	}
}

// TestContentToolRefusesTheWrongIndexKind: a search id with urlIndex (or the
// reverse) must be refused with the right instruction, never silently read index 0.
func TestContentToolRefusesTheWrongIndexKind(t *testing.T) {
	tool, content, base := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "text")
	})

	fetched := runFetch(t, tool, map[string]any{"url": base + "/x"})
	if _, err := content.Execute(context.Background(), mustJSON(t, map[string]any{
		"responseId": fetched.ResponseID, "queryIndex": 0,
	})); err == nil || !strings.Contains(err.Error(), "use urlIndex") {
		t.Fatalf("search selector on a fetch id: %v", err)
	}

	search := runSearch(t, websearch.NewClient(
		websearch.WithProviders(&stubProvider{name: "exa"}),
		websearch.WithStore(websearch.NewStore()),
	), map[string]any{"query": "x"})
	// The search above used a different client, so its id is unknown here — which
	// is exactly what a mixed-up transcript would see.
	if _, err := content.Execute(context.Background(), mustJSON(t, map[string]any{
		"responseId": search.ResponseID, "urlIndex": 0,
	})); err == nil || !strings.Contains(err.Error(), "response not found") {
		t.Fatalf("foreign id: %v", err)
	}
}

// TestFetchToolInputRules pins the schema's documented limits.
func TestFetchToolInputRules(t *testing.T) {
	tool, _, _ := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {})

	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"no url", map[string]any{}, "URL is required"},
		{"too many", map[string]any{"urls": []string{"https://a.example", "https://b.example", "https://c.example", "https://d.example"}}, "too many URLs"},
		{"bad mode", map[string]any{"url": "https://a.example", "mode": "render"}, "mode must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), mustJSON(t, tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// A refused private URL is a RESULT error (the model needs to know which URL and
	// why), not a tool error.
	privateTool, _, _ := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {})
	payload := runFetch(t, privateTool, map[string]any{"url": "http://169.254.169.254/latest/meta-data/"})
	if !strings.Contains(payload.URLs[0].Error, "private") {
		t.Fatalf("error = %q, want the private-address refusal", payload.URLs[0].Error)
	}
	if payload.ResponseID != "" {
		t.Errorf("a refused URL produced a responseId")
	}
}

// TestFetchToolSchemaAndDescription: the description is where the model learns the
// order of operations (search → fetch → read), so it must name them.
func TestFetchToolSchemaAndDescription(t *testing.T) {
	tool, _, _ := fetchTool(t, func(w http.ResponseWriter, r *http.Request) {})

	schema := string(tool.InputSchema())
	for _, want := range []string{`"url"`, `"urls"`, `"readable"`, `"raw"`} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema is missing %s", want)
		}
	}

	description := tool.Description()
	for _, want := range []string{"responseId", "get_search_content", "findText", "Private"} {
		if !strings.Contains(description, want) {
			t.Errorf("description is missing %q:\n%s", want, description)
		}
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
