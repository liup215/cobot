package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/liup215/cobot/websearch"
)

// WebSearchTool searches the web through cobot's provider chain and returns a
// BOUNDED answer plus a responseId for the rest.
//
// The contract in one sentence: this tool shows titles, URLs and the head of
// each snippet, and get_search_content reads the full text back. That split is
// the whole design (ported from pi's pi-web-access) — a search provider returns
// kilobytes of page text per hit, so an unbounded tool result is how a context
// window dies mid-task.
type WebSearchTool struct {
	BaseTool
	client *websearch.Client
}

// webSearchInput mirrors the JSON schema one-to-one.
type webSearchInput struct {
	Query      string   `json:"query"`
	Queries    []string `json:"queries"`
	NumResults int      `json:"numResults"`
	Recency    string   `json:"recencyFilter"`
	Domains    []string `json:"domainFilter"`
	Provider   string   `json:"provider"`
}

// NewWebSearchTool builds the tool on the process-wide client, whose store is
// shared with NewGetSearchContentTool.
func NewWebSearchTool() *WebSearchTool {
	return NewWebSearchToolWith(websearch.Default())
}

// NewWebSearchToolWith builds the tool on an injected client (hosts and tests).
func NewWebSearchToolWith(client *websearch.Client) *WebSearchTool {
	if client == nil {
		client = websearch.Default()
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "A single search query"
			},
			"queries": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Run several queries at once (up to 4, three in flight). Vary the angle instead of rephrasing the same one."
			},
			"numResults": {
				"type": "integer",
				"minimum": 1,
				"maximum": 10,
				"description": "Results per query (default 5)"
			},
			"recencyFilter": {
				"type": "string",
				"enum": ["day", "week", "month", "year"],
				"description": "Only results this recent"
			},
			"domainFilter": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Limit to these domains; prefix an entry with - to exclude it (e.g. \"cambridgeinternational.org\", \"-pinterest.com\")"
			},
			"provider": {
				"type": "string",
				"description": "Omit for the automatic chain, or name one provider (exa, tavily)"
			}
		},
		"additionalProperties": false
	}`)

	return &WebSearchTool{
		BaseTool: BaseTool{
			name:        "web_search",
			description: "Search the web for sources or answers. Returns each query's title/URL/snippet and stores the FULL provider text under a responseId — read it with get_search_content (page it with offset/limit, or locate passages with findText). Prefer queries[] with 2-4 varied angles over re-running one phrase. Results beyond the inline budget are counted, not silently dropped. Works with no configuration; TAVILY_API_KEY adds a page-grounded second provider. For a specific page's full text, this is not a fetcher — ask for the hit you want and quote what get_search_content returns.",
			inputSchema: schema,
		},
		client: client,
	}
}

// Execute implements Tool.
func (t *WebSearchTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args webSearchInput
	if err := ParseInput(input, &args); err != nil {
		return "", err
	}

	response, err := t.client.Search(ctx, websearch.SearchRequest{
		Query:      args.Query,
		Queries:    args.Queries,
		NumResults: args.NumResults,
		Recency:    args.Recency,
		Domains:    args.Domains,
		Provider:   args.Provider,
	})
	if err != nil {
		return "", err
	}

	// JSON, not prose: the payload is the same object a host renders as a card
	// and the model reads as text, so there is exactly one representation to
	// keep honest (Tool.Execute explicitly allows JSON).
	out, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("web_search: encode result: %w", err)
	}
	return string(out), nil
}
