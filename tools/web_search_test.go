package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/liup215/cobot/websearch"
)

// stubProvider is a scripted search backend: no network, deterministic text.
type stubProvider struct {
	name      string
	snippet   string
	results   int
	failWith  string
	callCount int
}

func (s *stubProvider) Name() string    { return s.name }
func (s *stubProvider) Available() bool { return true }

func (s *stubProvider) Search(_ context.Context, q websearch.Query) ([]websearch.Result, error) {
	s.callCount++
	if s.failWith != "" {
		return nil, errString(s.failWith)
	}
	count := s.results
	if count == 0 {
		count = 1
	}
	out := make([]websearch.Result, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, websearch.Result{
			Title:   "Result " + q.Text,
			URL:     "https://example.org/" + strings.ReplaceAll(q.Text, " ", "-"),
			Snippet: s.snippet,
		})
	}
	return out, nil
}

type errString string

func (e errString) Error() string { return string(e) }

func stubClient(provider websearch.Provider) *websearch.Client {
	return websearch.NewClient(
		websearch.WithProviders(provider),
		websearch.WithStore(websearch.NewStore()),
	)
}

type searchPayload struct {
	ResponseID string `json:"responseId"`
	Providers  []string
	Queries    []struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Error    string `json:"error"`
		Results  []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	} `json:"queries"`
	OmittedResults int    `json:"omittedResults"`
	Hint           string `json:"hint"`
}

func runSearch(t *testing.T, client *websearch.Client, input map[string]any) searchPayload {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	out, err := NewWebSearchToolWith(client).Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}

	var payload searchPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return payload
}

// TestWebSearchReturnsBoundedJSON pins the inline contract: numResults is
// honoured, the snippet is cut to the rune cap, and the answer points at
// get_search_content.
func TestWebSearchReturnsBoundedJSON(t *testing.T) {
	long := strings.Repeat("细胞结构的限制因素 ", 200) // ~1800 runes
	provider := &stubProvider{name: "exa", snippet: long, results: 5}
	client := stubClient(provider)

	payload := runSearch(t, client, map[string]any{"query": "cell structure", "numResults": 2})

	if !strings.HasPrefix(payload.ResponseID, "s_") {
		t.Errorf("responseId = %q", payload.ResponseID)
	}
	if len(payload.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(payload.Queries))
	}
	if got := payload.Queries[0].Provider; got != "exa" {
		t.Errorf("provider = %q", got)
	}
	if len(payload.Queries[0].Results) != 2 {
		t.Fatalf("inline results = %d, want numResults=2", len(payload.Queries[0].Results))
	}
	if runes := utf8Len(payload.Queries[0].Results[0].Snippet); runes > 402 {
		t.Errorf("inline snippet = %d runes, want <= 402", runes)
	}
	if !strings.Contains(payload.Hint, "get_search_content") || !strings.Contains(payload.Hint, payload.ResponseID) {
		t.Errorf("hint does not point at the stored content: %q", payload.Hint)
	}
}

// TestWebSearchThenReadStoredPages walks the pair end to end: the truncated
// inline snippet is fully readable afterwards, in pages.
func TestWebSearchThenReadStoredPages(t *testing.T) {
	long := strings.Repeat("limiting factor ", 100) + "TAIL-MARKER"
	client := stubClient(&stubProvider{name: "exa", snippet: long})

	search := runSearch(t, client, map[string]any{"query": "enzymes"})
	id := search.ResponseID

	first := runContent(t, NewGetSearchContentToolWith(client), map[string]any{"responseId": id, "limit": 100})
	if utf8Len(first.Content) != 100 {
		t.Fatalf("first page = %d runes, want 100", utf8Len(first.Content))
	}
	if first.Total <= 100 {
		t.Fatalf("total = %d, want the full stored text", first.Total)
	}
	if first.NextOffset != 100 {
		t.Errorf("nextOffset = %d, want 100", first.NextOffset)
	}
	if !strings.Contains(first.Content, "Result enzymes") {
		t.Errorf("first page lacks the title line: %q", first.Content)
	}

	// Walk to the end and prove the whole snippet is there.
	var all strings.Builder
	all.WriteString(first.Content)
	var page contentPayload
	next := first.NextOffset
	for next > 0 {
		page = runContent(t, NewGetSearchContentToolWith(client), map[string]any{"responseId": id, "offset": next, "limit": 400})
		all.WriteString(page.Content)
		next = page.NextOffset
	}
	if !strings.Contains(all.String(), "TAIL-MARKER") {
		t.Errorf("stored text is missing its tail: %q", all.String())
	}
}

// TestGetSearchContentFindText locates a passage instead of paging.
func TestGetSearchContentFindText(t *testing.T) {
	client := stubClient(&stubProvider{name: "exa", snippet: "Light intensity affects the rate of photosynthesis. Temperature matters too."})
	search := runSearch(t, client, map[string]any{"query": "photosynthesis"})

	found := runFind(t, NewGetSearchContentToolWith(client), map[string]any{"responseId": search.ResponseID, "findText": "temperature"})
	if found.TotalMatches != 1 || len(found.Passages) != 1 {
		t.Fatalf("found = %+v", found)
	}
	if !strings.Contains(found.Passages[0].Text, "Temperature") {
		t.Errorf("passage = %q", found.Passages[0].Text)
	}

	// A list of terms is accepted too.
	multi := runFind(t, NewGetSearchContentToolWith(client), map[string]any{
		"responseId": search.ResponseID,
		"findText":   []string{"light intensity", "temperature"},
	})
	if multi.TotalMatches != 2 {
		t.Errorf("totalMatches = %d, want 2", multi.TotalMatches)
	}
}

// TestGetSearchContentErrors pins the three refusals a model can actually hit.
func TestGetSearchContentErrors(t *testing.T) {
	client := stubClient(&stubProvider{name: "exa"})
	search := runSearch(t, client, map[string]any{"query": "one", "queries": []string{"one", "two"}})

	cases := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{"missing id", map[string]any{}, "responseId is required"},
		{"unknown id", map[string]any{"responseId": "s_deadbeef"}, "expire"},
		{"index out of range", map[string]any{"responseId": search.ResponseID, "queryIndex": 5}, "out of range"},
		{"empty findText", map[string]any{"responseId": search.ResponseID, "findText": "  "}, "findText"},
		{"bad mode", map[string]any{"responseId": search.ResponseID, "findText": "x", "findMode": "regex"}, "findMode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.input)
			_, err := NewGetSearchContentToolWith(client).Execute(context.Background(), raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestWebSearchSurfacesQueryErrors: a provider failure belongs in the payload
// (so the model can report it), not as a tool error.
func TestWebSearchSurfacesQueryErrors(t *testing.T) {
	client := stubClient(&stubProvider{name: "exa", failWith: "upstream timeout"})
	payload := runSearch(t, client, map[string]any{"query": "x"})

	if !strings.Contains(payload.Queries[0].Error, "upstream timeout") {
		t.Fatalf("error = %q", payload.Queries[0].Error)
	}
	if len(payload.Queries[0].Results) != 0 {
		t.Errorf("failed query returned results: %+v", payload.Queries[0].Results)
	}
}

// TestWebSearchRequiresAQuery: an empty call is a caller mistake, so it is a
// tool error with an instruction, not an empty success.
func TestWebSearchRequiresAQuery(t *testing.T) {
	client := stubClient(&stubProvider{name: "exa"})
	raw, _ := json.Marshal(map[string]any{})
	_, err := NewWebSearchToolWith(client).Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "query is required") {
		t.Fatalf("err = %v", err)
	}
}

// TestWebToolsAreRegisteredTogether asserts the whole family is in the default
// registry for BOTH modes: a bounded answer with no reader would be dead weight, and
// searching/fetching/checking are legitimate planning actions.
func TestWebToolsAreRegisteredTogether(t *testing.T) {
	registry := InitDefaultRegistry()

	for _, mode := range []string{"plan", "act"} {
		names := make(map[string]bool)
		for _, tool := range registry.GetForMode(mode) {
			names[tool.Name()] = true
		}
		for _, want := range []string{"web_search", "fetch_content", "get_search_content", "source_check"} {
			if !names[want] {
				t.Errorf("mode %s: %s is not registered", mode, want)
			}
		}
	}

	info, err := registry.GetInfo("web_search")
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Category != CategoryNetwork {
		t.Errorf("category = %q, want %q", info.Category, CategoryNetwork)
	}
	if info.RequiresConfirmation {
		t.Error("web_search asks for confirmation; it only reads")
	}
}

// TestWebSearchSchemaDocumentsTheCaps keeps the schema and the constants from
// drifting apart — a schema that promises 20 results while the code clamps at 10
// is a lie the model cannot see.
func TestWebSearchSchemaDocumentsTheCaps(t *testing.T) {
	schema := string(NewWebSearchToolWith(stubClient(&stubProvider{name: "exa"})).InputSchema())
	for _, want := range []string{`"maximum": 10`, `"day"`, `"week"`, `"month"`, `"year"`, "domainFilter", "recencyFilter"} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema is missing %s", want)
		}
	}
	if max := websearch.MaxNumResults; max != 10 {
		t.Errorf("MaxNumResults = %d, schema says 10", max)
	}
}

type contentPayload struct {
	ResponseID string `json:"responseId"`
	Kind       string `json:"kind"`
	IndexParam string `json:"indexParam"`
	Index      int    `json:"index"`
	QueryIndex int    `json:"queryIndex"`
	Query      string `json:"query"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Offset     int    `json:"offset"`
	Total      int    `json:"total"`
	NextOffset int    `json:"nextOffset"`
	Content    string `json:"content"`
}

type findPayload struct {
	Passages []struct {
		Match  string `json:"match"`
		Text   string `json:"text"`
		Offset int    `json:"offset"`
	} `json:"passages"`
	TotalMatches    int  `json:"totalMatches"`
	ReturnedMatches int  `json:"returnedMatches"`
	Truncated       bool `json:"truncated"`
}

// toolExecutor is what both web tools satisfy: a stored response is read through
// whichever tool holds the shared client, so the test helpers take the tool (not a
// client) and cannot accidentally build a second one with its own store.
type toolExecutor interface {
	Execute(ctx context.Context, input json.RawMessage) (string, error)
}

func runContent(t *testing.T, tool toolExecutor, input map[string]any) contentPayload {
	t.Helper()
	raw, _ := json.Marshal(input)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("get_search_content: %v", err)
	}
	var payload contentPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return payload
}

func runFind(t *testing.T, tool toolExecutor, input map[string]any) findPayload {
	t.Helper()
	raw, _ := json.Marshal(input)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("get_search_content: %v", err)
	}
	var payload findPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return payload
}

func utf8Len(s string) int {
	count := 0
	for range s {
		count++
	}
	return count
}
