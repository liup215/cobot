package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/liup215/cobot/websearch"
)

// WebFetchTool reads ONE page — the complement of web_search.
//
// Why a separate tool instead of "includeContent" on the search: a search returns
// candidates, and only the caller knows which of them is worth reading. Bundling
// fetching into searching either wastes bandwidth on hits nobody reads, or returns
// a page-sized blob from a call whose answer should be a list.
//
// The result is a bounded excerpt plus a responseId; the full text is read back
// with get_search_content. That split exists because a fetched page is routinely
// 50–300KB of text, which is not something a tool result may hand a context
// window — see the package doc in websearch.
type WebFetchTool struct {
	BaseTool
	client *websearch.Client
}

// webFetchInput mirrors the JSON schema one-to-one.
type webFetchInput struct {
	URL  string   `json:"url"`
	URLs []string `json:"urls"`
	Mode string   `json:"mode"`
}

// NewWebFetchTool builds the tool on the process-wide client, whose store is shared
// with web_search and get_search_content.
func NewWebFetchTool() *WebFetchTool {
	return NewWebFetchToolWith(websearch.Default())
}

// NewWebFetchToolWith builds the tool on an injected client (hosts and tests).
func NewWebFetchToolWith(client *websearch.Client) *WebFetchTool {
	if client == nil {
		client = websearch.Default()
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "The page to read"
			},
			"urls": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Up to 3 pages to read at once — use this to compare sources, not to crawl"
			},
			"mode": {
				"type": "string",
				"enum": ["readable", "raw"],
				"description": "readable (default) extracts the article as markdown: HTML, PDF and office documents are converted to text. raw returns the body as-is, for APIs, JSON and plain-text files."
			}
		},
		"additionalProperties": false
	}`)

	return &WebFetchTool{
		BaseTool: BaseTool{
			name:        "fetch_content",
			description: "Read ONE web page (or PDF/office document) in full and return its text. Use it after web_search when the snippet is not enough, or when the user gives a link. Returns a bounded excerpt plus a responseId — read the whole text with get_search_content (urlIndex, offset/limit) and locate a specific fact with findText. readable mode converts HTML to markdown (navigation and scripts dropped), PDFs and office documents to text; raw mode returns the body unchanged. Up to 3 URLs per call, for comparing sources. Private and local addresses (localhost, 10.x, 192.168.x, 169.254.x) are refused on purpose. Not a crawler: it will not follow links, render JavaScript, or get past a login.",
			inputSchema: schema,
		},
		client: client,
	}
}

// Execute implements Tool.
func (t *WebFetchTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args webFetchInput
	if err := ParseInput(input, &args); err != nil {
		return "", err
	}

	response, err := t.client.Fetch(ctx, websearch.FetchRequest{
		URL:  args.URL,
		URLs: args.URLs,
		Mode: args.Mode,
	})
	if err != nil {
		return "", err
	}

	out, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fetch_content: encode result: %w", err)
	}
	return string(out), nil
}
