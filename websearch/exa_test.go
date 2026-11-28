package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// exaBlockText is the reply shape the public endpoint actually sends (captured
// from a live call): one human-readable block per hit, metadata in the header
// and the excerpt after "Highlights:".
const exaBlockText = `Title: Cambridge International AS & A Level Biology (9700)
URL: https://www.cambridgeinternational.org/programmes-and-qualifications/biology-9700/
Published: N/A
Author: N/A
Highlights:
# Cambridge International AS & A Level Biology (9700)

The syllabus includes the main theoretical concepts.

---

Title: 9700 syllabus update 2025-2027
URL: https://www.cambridgeinternational.org/Images/9700-update.pdf
Published: N/A
Author: N/A
Highlights:
Changes to the syllabus for examination from 2025: a new topic on climate change.`

// sseEnvelope wraps a tool text payload in the SSE + JSON-RPC framing.
func sseEnvelope(t *testing.T, text string) string {
	t.Helper()
	envelope := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"_meta":   map[string]any{"searchTime": 12.3},
		},
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return "event: message\ndata: " + string(encoded) + "\n\n"
}

func exaServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	return server
}

// TestExaProviderParsesBlockReply pins the block parser: two hits, metadata
// lines excluded from the snippet, trailing "---" removed.
func TestExaProviderParsesBlockReply(t *testing.T) {
	server := exaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseEnvelope(t, exaBlockText))
	})

	provider := NewExaProvider(WithProviderEndpoint(server.URL))
	results, err := provider.Search(context.Background(), Query{Text: "9700 syllabus", NumResults: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (%+v)", len(results), results)
	}
	if results[0].Title != "Cambridge International AS & A Level Biology (9700)" {
		t.Errorf("title = %q", results[0].Title)
	}
	if results[0].URL != "https://www.cambridgeinternational.org/programmes-and-qualifications/biology-9700/" {
		t.Errorf("url = %q", results[0].URL)
	}
	if !strings.Contains(results[0].Snippet, "main theoretical concepts") {
		t.Errorf("snippet missing body text: %q", results[0].Snippet)
	}
	for _, leaked := range []string{"Published:", "Author:", "---"} {
		if strings.Contains(results[0].Snippet, leaked) {
			t.Errorf("snippet leaked header/footer %q: %q", leaked, results[0].Snippet)
		}
	}
	if !strings.Contains(results[1].Snippet, "climate change") {
		t.Errorf("second snippet = %q", results[1].Snippet)
	}
}

// TestExaProviderParsesJSONReply keeps the JSON variant working: some
// deployments answer with a JSON document instead of blocks.
func TestExaProviderParsesJSONReply(t *testing.T) {
	inner := `{"results":[{"title":"Cell structure","url":"https://example.org/cell","highlights":["Organelles carry out specific functions.","Mitochondria release ATP."]}]}`
	server := exaServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sseEnvelope(t, inner))
	})

	results, err := NewExaProvider(WithProviderEndpoint(server.URL)).Search(context.Background(), Query{Text: "cell", NumResults: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Snippet != "Organelles carry out specific functions. Mitochondria release ATP." {
		t.Errorf("snippet = %q", results[0].Snippet)
	}
}

// TestExaProviderEncodesFiltersIntoQuery pins the public endpoint's limitation:
// it ignores structured filters, so they must ride in the query text or the
// search silently widens.
func TestExaProviderEncodesFiltersIntoQuery(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotNum   float64
	)
	server := exaServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Params struct {
				Arguments struct {
					Query      string  `json:"query"`
					NumResults float64 `json:"numResults"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("request body is not JSON-RPC: %v", err)
		}
		gotQuery = request.Params.Arguments.Query
		gotNum = request.Params.Arguments.NumResults
		io.WriteString(w, sseEnvelope(t, exaBlockText))
	})

	provider := NewExaProvider(WithProviderEndpoint(server.URL))
	if _, err := provider.Search(context.Background(), Query{
		Text:       "photosynthesis",
		NumResults: 7,
		Recency:    RecencyWeek,
		Domains:    []string{"cambridgeinternational.org", "-pinterest.com"},
	}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, want := range []string{"photosynthesis", "site:cambridgeinternational.org", "-site:pinterest.com", "past week"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %q", gotQuery, want)
		}
	}
	if gotNum != 7 {
		t.Errorf("numResults = %v, want 7", gotNum)
	}
	if !strings.Contains(gotPath, "tools=web_search_exa") {
		t.Errorf("request path %q does not select the search tool", gotPath)
	}
}

// TestExaProviderRateLimitCarriesCooldown pins the 429 contract the client
// relies on to decide between "retry" and "next provider".
func TestExaProviderRateLimitCarriesCooldown(t *testing.T) {
	server := exaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "slow down")
	})

	_, err := NewExaProvider(WithProviderEndpoint(server.URL)).Search(context.Background(), Query{Text: "x", NumResults: 1})

	var rate *RateLimitError
	if !errors.As(err, &rate) {
		t.Fatalf("err = %v, want *RateLimitError", err)
	}
	if rate.RetryAfter != 2*time.Second {
		t.Errorf("RetryAfter = %v, want 2s", rate.RetryAfter)
	}
	if rate.Provider != "exa" {
		t.Errorf("Provider = %q", rate.Provider)
	}
}

// TestExaProviderReportsRPCError keeps a JSON-RPC error from looking like "no
// results".
func TestExaProviderReportsRPCError(t *testing.T) {
	server := exaServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32602,\"message\":\"numResults too large\"}}\n\n")
	})

	_, err := NewExaProvider(WithProviderEndpoint(server.URL)).Search(context.Background(), Query{Text: "x", NumResults: 1})
	if err == nil || !strings.Contains(err.Error(), "numResults too large") {
		t.Fatalf("err = %v, want the RPC message", err)
	}
}
