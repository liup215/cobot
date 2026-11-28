package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/liup215/cobot/websearch"
)

// WebSourceCheckTool gathers evidence for a claim — and refuses to judge it.
//
// The tool exists because "check this for me" is a different request from "search
// this": the answer has to be checkable (ranked sources, quoted passages, offsets,
// hashes, and what to verify), and it must not be a verdict. See
// websearch/research.go for why the verdict is structurally absent rather than
// discouraged: a word-overlap selector cannot tell "no longer includes X" from
// "still includes X", so any computed status would be a coin flip wearing a label.
type WebSourceCheckTool struct {
	BaseTool
	client *websearch.Client
}

// webSourceCheckInput mirrors the JSON schema.
type webSourceCheckInput struct {
	Claim        string   `json:"claim"`
	Queries      []string `json:"queries"`
	NumResults   int      `json:"numResults"`
	FetchContent *bool    `json:"fetchContent"`
	Recency      string   `json:"recencyFilter"`
	Domains      []string `json:"domainFilter"`
}

// NewWebSourceCheckTool builds the tool on the process-wide client.
func NewWebSourceCheckTool() *WebSourceCheckTool {
	return NewWebSourceCheckToolWith(websearch.Default())
}

// NewWebSourceCheckToolWith builds the tool on an injected client.
func NewWebSourceCheckToolWith(client *websearch.Client) *WebSourceCheckTool {
	if client == nil {
		client = websearch.Default()
	}

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"claim": {
				"type": "string",
				"description": "The statement to gather evidence about, quoted as precisely as possible (numbers, dates, exam-board names and negation matter)"
			},
			"queries": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Searches to run instead of the claim itself (up to 8). Vary the angle: one phrasing of the claim, one for the opposite, one for the primary source"
			},
			"numResults": {
				"type": "integer",
				"minimum": 1,
				"maximum": 10,
				"description": "Sources per query (default 5)"
			},
			"fetchContent": {
				"type": "boolean",
				"description": "Read the top 3 sources in full so passages are quoted page text with offsets (default true). false is faster and leaves only the search engine's paraphrase, which is not a quote"
			},
			"recencyFilter": {
				"type": "string",
				"enum": ["day", "week", "month", "year"],
				"description": "Only sources this recent"
			},
			"domainFilter": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Limit to these domains; prefix with - to exclude (e.g. \"cambridgeinternational.org\")"
			}
		},
		"required": ["claim"],
		"additionalProperties": false
	}`)

	return &WebSourceCheckTool{
		BaseTool: BaseTool{
			name:        "source_check",
			description: "Gather EVIDENCE for a claim: ranked sources, quoted passages with rune offsets and sha256 hashes, and the checks a reader should perform. It does NOT decide whether the claim is true — status is either \"missing-evidence\" (nothing retrieved, so the claim is unchecked) or \"unclear\" (passages retrieved, read them yourself). Use it to verify a fact, a syllabus/exam-board statement, a textbook number or an answer key, then reason from the passages and cite them. Passages marked origin \"page\" are text fetched from the page; \"provider_summary\" is the search engine's paraphrase and is not a quote. Full evidence set is stored under a responseId for get_search_content. For a plain lookup use web_search; to read one specific page use fetch_content.",
			inputSchema: schema,
		},
		client: client,
	}
}

// Execute implements Tool.
func (t *WebSourceCheckTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args webSourceCheckInput
	if err := ParseInput(input, &args); err != nil {
		return "", err
	}

	artifact, err := t.client.SourceCheck(ctx, websearch.SourceCheckRequest{
		Claim:        args.Claim,
		Queries:      args.Queries,
		NumResults:   args.NumResults,
		FetchContent: args.FetchContent,
		Recency:      args.Recency,
		Domains:      args.Domains,
	})
	if err != nil {
		return "", err
	}

	out, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return "", fmt.Errorf("source_check: encode result: %w", err)
	}
	return string(out), nil
}
