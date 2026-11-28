package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liup215/cobot/websearch"
)

// GetSearchContentTool reads back what web_search stored: one query's complete
// provider text, in pages, or the passages matching findText.
//
// It is the second half of the pair. Without it web_search would have to return
// everything inline (context blow-up) or nothing (no way to read the full
// source), so the two tools are always registered together.
type GetSearchContentTool struct {
	BaseTool
	client *websearch.Client
}

// getSearchContentInput mirrors the JSON schema. FindText is raw because the
// schema accepts either one string or an array of them.
type getSearchContentInput struct {
	ResponseID string          `json:"responseId"`
	QueryIndex *int            `json:"queryIndex"`
	Offset     int             `json:"offset"`
	Limit      int             `json:"limit"`
	FindText   json.RawMessage `json:"findText"`
	FindMode   string          `json:"findMode"`
}

// NewGetSearchContentTool builds the tool on the process-wide client.
func NewGetSearchContentTool() *GetSearchContentTool {
	return NewGetSearchContentToolWith(websearch.Default())
}

// NewGetSearchContentToolWith builds the tool on an injected client, which must
// be the SAME client the search tool used (it owns the store).
func NewGetSearchContentToolWith(client *websearch.Client) *GetSearchContentTool {
	if client == nil {
		client = websearch.Default()
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"responseId": {
				"type": "string",
				"description": "The responseId returned by web_search"
			},
			"queryIndex": {
				"type": "integer",
				"minimum": 0,
				"description": "Which query's stored results to read (default 0)"
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
			description: "Read the full text stored by web_search. Pass its responseId and either page with offset/limit, or pass findText to get bounded passages around each match (±400 runes) with rune offsets. Use findText for a fact or a number on a long page; page when you need to read a source end to end. Stored responses expire after an hour — if the id is rejected, run web_search again.",
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
			// One message for unknown AND expired ids: "search again" is the
			// only useful instruction either way.
			return "", fmt.Errorf("response not found: %s (stored responses expire after an hour — run web_search again)", id)
		}
		return "", fmt.Errorf("get_search_content: %w", err)
	}

	index := 0
	if args.QueryIndex != nil {
		index = *args.QueryIndex
	}
	if index < 0 || index >= len(entry.Queries) {
		return "", fmt.Errorf("queryIndex %d out of range: %d quer%s stored", index, len(entry.Queries), plural(len(entry.Queries)))
	}
	query := entry.Queries[index]

	terms, err := parseFindTerms(args.FindText)
	if err != nil {
		return "", err
	}

	out, err := t.read(id, index, query, terms, args)
	if err != nil {
		return "", err
	}
	return out, nil
}

func (t *GetSearchContentTool) read(
	id string,
	index int,
	query websearch.StoredQuery,
	terms []string,
	args getSearchContentInput,
) (string, error) {
	if len(terms) > 0 {
		mode, err := websearch.ParseFindMode(args.FindMode)
		if err != nil {
			return "", err
		}
		budget := args.Limit
		if budget <= 0 || budget > websearch.MaxPageRunes {
			budget = websearch.MaxPageRunes
		}
		found := websearch.Find(query.Content, terms, mode, budget)
		payload := struct {
			ResponseID string `json:"responseId"`
			QueryIndex int    `json:"queryIndex"`
			Query      string `json:"query"`
			Mode       string `json:"mode"`
			websearch.FindResult
		}{
			ResponseID: id,
			QueryIndex: index,
			Query:      query.Query,
			Mode:       string(mode),
			FindResult: found,
		}
		encoded, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return "", fmt.Errorf("get_search_content: encode result: %w", err)
		}
		return string(encoded), nil
	}

	page, total, next := websearch.SliceContent(query.Content, args.Offset, args.Limit)
	payload := struct {
		ResponseID string `json:"responseId"`
		QueryIndex int    `json:"queryIndex"`
		Query      string `json:"query"`
		Provider   string `json:"provider,omitempty"`
		SearchErr  string `json:"searchError,omitempty"`
		Hits       int    `json:"hits"`
		Offset     int    `json:"offset"`
		Total      int    `json:"total"`
		NextOffset int    `json:"nextOffset"`
		Content    string `json:"content"`
	}{
		ResponseID: id,
		QueryIndex: index,
		Query:      query.Query,
		Provider:   query.Provider,
		SearchErr:  query.Error,
		Hits:       len(query.Results),
		Offset:     args.Offset,
		Total:      total,
		NextOffset: next,
		Content:    page,
	}
	if payload.Offset < 0 {
		payload.Offset = 0
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
