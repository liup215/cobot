package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liup215/cobot/websearch"
)

// GetSearchContentTool reads back what web_search or fetch_content stored: one
// entry's complete text, in pages, or the passages matching findText.
//
// It serves BOTH stores on purpose. The memory safety of both tools comes from the
// same place — a bounded inline answer plus a responseId — so splitting the reader
// in two would duplicate paging, findText and the expiry message, and the two
// copies would drift. Which entry a responseId refers to is decided by its entry
// kind: search results are selected by queryIndex/query, fetched pages by
// urlIndex/url.
type GetSearchContentTool struct {
	BaseTool
	client *websearch.Client
}

// getSearchContentInput mirrors the JSON schema. FindText is raw because the
// schema accepts one string or an array of them.
type getSearchContentInput struct {
	ResponseID string          `json:"responseId"`
	QueryIndex *int            `json:"queryIndex"`
	URLIndex   *int            `json:"urlIndex"`
	Query      string          `json:"query"`
	URL        string          `json:"url"`
	Offset     int             `json:"offset"`
	Limit      int             `json:"limit"`
	FindText   json.RawMessage `json:"findText"`
	FindMode   string          `json:"findMode"`
}

// contentTarget is the one stored entry this call reads, after both kinds have
// been reduced to "a label plus some text".
type contentTarget struct {
	kind      string
	index     int
	label     string // the query text, or the page URL
	title     string // fetched pages only
	provider  string
	searchErr string
	hits      int
	content   string
	// indexParam names the field that selected this entry, so the reply tells the
	// caller which index to page with next (queryIndex vs urlIndex).
	indexParam string
}

// NewGetSearchContentTool builds the tool on the process-wide client.
func NewGetSearchContentTool() *GetSearchContentTool {
	return NewGetSearchContentToolWith(websearch.Default())
}

// NewGetSearchContentToolWith builds the tool on an injected client, which must be
// the SAME client the search/fetch tools used (it owns the store).
func NewGetSearchContentToolWith(client *websearch.Client) *GetSearchContentTool {
	if client == nil {
		client = websearch.Default()
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"responseId": {
				"type": "string",
				"description": "The responseId returned by web_search (s_…) or fetch_content (f_…)"
			},
			"queryIndex": {
				"type": "integer",
				"minimum": 0,
				"description": "Which query's stored results to read (web_search responses only, default 0)"
			},
			"urlIndex": {
				"type": "integer",
				"minimum": 0,
				"description": "Which fetched page to read (fetch_content responses only, default 0)"
			},
			"query": {
				"type": "string",
				"description": "Select a search result set by its query text instead of queryIndex"
			},
			"url": {
				"type": "string",
				"description": "Select a fetched page by its URL instead of urlIndex"
			},
			"offset": {
				"type": "integer",
				"minimum": 0,
				"description": "Rune offset to start at (default 0)"
			},
			"limit": {
				"type": "integer",
				"minimum": 1,
				"maximum": 12000,
				"description": "Runes to return (default 6000)"
			},
			"findText": {
				"description": "Locate passages instead of paging: one string or an array of strings",
				"anyOf": [
					{"type": "string", "minLength": 1},
					{"type": "array", "items": {"type": "string", "minLength": 1}, "minItems": 1, "maxItems": 10}
				]
			},
			"findMode": {
				"type": "string",
				"enum": ["exact", "case-insensitive", "fuzzy"],
				"description": "Match mode for findText (default case-insensitive)"
			}
		},
		"required": ["responseId"],
		"additionalProperties": false
	}`)

	return &GetSearchContentTool{
		BaseTool: BaseTool{
			name:        "get_search_content",
			description: "Read the full text stored by web_search or fetch_content. Pass its responseId and either page with offset/limit, or pass findText to get bounded passages around each match (±400 runes) with rune offsets. For a fetched page use urlIndex (or url) to pick the page; for a search response use queryIndex (or query). Use findText for a fact or a number on a long page; page when you need to read a source end to end. Stored responses expire after an hour — if the id is rejected, run the search or fetch again.",
			inputSchema: schema,
		},
		client: client,
	}
}

// Execute implements Tool.
func (t *GetSearchContentTool) Execute(_ context.Context, input json.RawMessage) (string, error) {
	var args getSearchContentInput
	if err := ParseInput(input, &args); err != nil {
		return "", err
	}

	id := strings.TrimSpace(args.ResponseID)
	if id == "" {
		return "", fmt.Errorf("responseId is required")
	}

	entry, err := t.client.Store().Get(id)
	if err != nil {
		if errors.Is(err, websearch.ErrNotFound) {
			// One message for unknown AND expired ids: "run it again" is the only
			// useful instruction either way.
			return "", fmt.Errorf("response not found: %s (stored responses expire after an hour — run web_search or fetch_content again)", id)
		}
		return "", fmt.Errorf("get_search_content: %w", err)
	}

	target, err := resolveTarget(entry, args)
	if err != nil {
		return "", err
	}

	terms, err := parseFindTerms(args.FindText)
	if err != nil {
		return "", err
	}

	if len(terms) > 0 {
		return t.readPassages(id, target, terms, args)
	}
	return t.readPage(id, target, args)
}

// resolveTarget picks the stored entry this call reads, refusing the wrong
// selector loudly: silently falling back to index 0 would return a different
// source than the caller asked for.
func resolveTarget(entry *websearch.Entry, args getSearchContentInput) (contentTarget, error) {
	switch entry.Kind {
	case websearch.KindFetch:
		if args.QueryIndex != nil {
			return contentTarget{}, fmt.Errorf("this responseId holds fetched pages, not search results — use urlIndex (or url) instead of queryIndex")
		}
		index := 0
		if args.URLIndex != nil {
			index = *args.URLIndex
		}
		if wanted := strings.TrimSpace(args.URL); wanted != "" {
			index = -1
			for i, doc := range entry.URLs {
				if doc.URL == wanted {
					index = i
					break
				}
			}
			if index < 0 {
				return contentTarget{}, fmt.Errorf("url not found in this response: %s (it holds %d page(s); omit url to read page 0)", wanted, len(entry.URLs))
			}
		}
		if index < 0 || index >= len(entry.URLs) {
			return contentTarget{}, fmt.Errorf("urlIndex %d out of range: %d page(s) stored", index, len(entry.URLs))
		}
		doc := entry.URLs[index]
		if doc.Error != "" && doc.Content == "" {
			return contentTarget{}, fmt.Errorf("page %d was not stored: %s", index, doc.Error)
		}
		return contentTarget{
			kind:       entry.Kind,
			index:      index,
			label:      doc.URL,
			title:      doc.Title,
			searchErr:  doc.Error,
			content:    doc.Content,
			hits:       1,
			indexParam: "urlIndex",
		}, nil

	default:
		if args.URLIndex != nil || strings.TrimSpace(args.URL) != "" {
			return contentTarget{}, fmt.Errorf("this responseId holds search results, not fetched pages — use queryIndex (or query) instead of urlIndex")
		}
		index := 0
		if args.QueryIndex != nil {
			index = *args.QueryIndex
		}
		if wanted := strings.TrimSpace(args.Query); wanted != "" {
			index = -1
			for i, query := range entry.Queries {
				if strings.EqualFold(query.Query, wanted) {
					index = i
					break
				}
			}
			if index < 0 {
				return contentTarget{}, fmt.Errorf("query not found in this response: %s (it holds %d quer%s; omit query to read the first)", wanted, len(entry.Queries), plural(len(entry.Queries)))
			}
		}
		if index < 0 || index >= len(entry.Queries) {
			return contentTarget{}, fmt.Errorf("queryIndex %d out of range: %d quer%s stored", index, len(entry.Queries), plural(len(entry.Queries)))
		}
		query := entry.Queries[index]
		return contentTarget{
			kind:       entry.Kind,
			index:      index,
			label:      query.Query,
			provider:   query.Provider,
			searchErr:  query.Error,
			content:    query.Content,
			hits:       len(query.Results),
			indexParam: "queryIndex",
		}, nil
	}
}

// readPage answers with one page of the stored text.
func (t *GetSearchContentTool) readPage(id string, target contentTarget, args getSearchContentInput) (string, error) {
	page, total, next := websearch.SliceContent(target.content, args.Offset, args.Limit)
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}

	payload := struct {
		ResponseID string `json:"responseId"`
		Kind       string `json:"kind"`
		IndexParam string `json:"indexParam"`
		Index      int    `json:"index"`
		Query      string `json:"query,omitempty"`
		URL        string `json:"url,omitempty"`
		Title      string `json:"title,omitempty"`
		Provider   string `json:"provider,omitempty"`
		SearchErr  string `json:"error,omitempty"`
		Hits       int    `json:"hits,omitempty"`
		Offset     int    `json:"offset"`
		Total      int    `json:"total"`
		NextOffset int    `json:"nextOffset"`
		Content    string `json:"content"`
	}{
		ResponseID: id,
		Kind:       target.kind,
		IndexParam: target.indexParam,
		Index:      target.index,
		Provider:   target.provider,
		SearchErr:  target.searchErr,
		Hits:       target.hits,
		Offset:     offset,
		Total:      total,
		NextOffset: next,
		Content:    page,
	}
	if target.kind == websearch.KindFetch {
		payload.URL = target.label
		payload.Title = target.title
	} else {
		payload.Query = target.label
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("get_search_content: encode result: %w", err)
	}
	return string(encoded), nil
}

// readPassages answers with findText passages.
func (t *GetSearchContentTool) readPassages(id string, target contentTarget, terms []string, args getSearchContentInput) (string, error) {
	mode, err := websearch.ParseFindMode(args.FindMode)
	if err != nil {
		return "", err
	}
	budget := args.Limit
	if budget <= 0 || budget > websearch.MaxPageRunes {
		budget = websearch.MaxPageRunes
	}
	found := websearch.Find(target.content, terms, mode, budget)

	payload := struct {
		ResponseID string `json:"responseId"`
		Kind       string `json:"kind"`
		IndexParam string `json:"indexParam"`
		Index      int    `json:"index"`
		Query      string `json:"query,omitempty"`
		URL        string `json:"url,omitempty"`
		Title      string `json:"title,omitempty"`
		Mode       string `json:"mode"`
		websearch.FindResult
	}{
		ResponseID: id,
		Kind:       target.kind,
		IndexParam: target.indexParam,
		Index:      target.index,
		Mode:       string(mode),
		FindResult: found,
	}
	if target.kind == websearch.KindFetch {
		payload.URL = target.label
		payload.Title = target.title
	} else {
		payload.Query = target.label
	}

	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("get_search_content: encode result: %w", err)
	}
	return string(encoded), nil
}

// parseFindTerms accepts a JSON string or array of strings. An absent value is
// "no findText" (paging), not an error.
func parseFindTerms(raw json.RawMessage) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, fmt.Errorf("findText must not be empty")
		}
		return []string{single}, nil
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("findText must be a string or an array of strings")
	}
	terms := make([]string, 0, len(many))
	for _, term := range many {
		if strings.TrimSpace(term) != "" {
			terms = append(terms, term)
		}
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("findText must contain at least one non-empty string")
	}
	return terms, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
