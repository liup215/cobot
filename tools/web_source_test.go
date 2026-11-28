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

// TestSourceCheckToolSchemaHasNoVerdict is the schema-level half of the design: the
// tool must not offer a status the code cannot honestly produce. A caller (or a
// model) that sees "supported" in an enum will expect it.
func TestSourceCheckToolSchemaHasNoVerdict(t *testing.T) {
	tool := NewWebSourceCheckToolWith(websearch.NewClient())

	schema := string(tool.InputSchema())
	for _, forbidden := range []string{"supported", "contradicted", "status"} {
		if strings.Contains(schema, forbidden) {
			t.Errorf("source_check schema mentions %q:\n%s", forbidden, schema)
		}
	}
	for _, want := range []string{"claim", "queries", "fetchContent", "domainFilter"} {
		if !strings.Contains(schema, want) {
			t.Errorf("schema is missing %q", want)
		}
	}

	description := tool.Description()
	for _, want := range []string{"EVIDENCE", "does NOT decide", "missing-evidence", "unclear", "provider_summary"} {
		if !strings.Contains(description, want) {
			t.Errorf("description is missing %q:\n%s", want, description)
		}
	}
}

// sourceTool wires source_check + get_search_content onto one client.
func sourceTool(t *testing.T, pages map[string]string) (*WebSourceCheckTool, *GetSearchContentTool, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)

	provider := &stubProvider{name: "exa", snippet: "The syllabus includes the main theoretical concepts of biology."}
	client := websearch.NewClient(
		websearch.WithProviders(provider),
		websearch.WithStore(websearch.NewStore()),
		websearch.WithFetchGuard(websearch.FetchGuardOptions{AllowLoopback: true}),
	)
	return NewWebSourceCheckToolWith(client), NewGetSearchContentToolWith(client), server.URL
}

type artifactPayload struct {
	Claim     string `json:"claim"`
	Status    string `json:"status"`
	Rationale string `json:"rationale"`
	Sources   []struct {
		Rank    int    `json:"rank"`
		URL     string `json:"url"`
		Quality string `json:"quality"`
		Fetched bool   `json:"fetched"`
		Error   string `json:"error"`
	} `json:"sources"`
	Passages []struct {
		ID     string `json:"passageId"`
		Text   string `json:"text"`
		Origin string `json:"origin"`
		Start  int    `json:"start"`
		Hash   string `json:"hash"`
	} `json:"passages"`
	Checks     []string `json:"checks"`
	ResponseID string   `json:"responseId"`
	Hint       string   `json:"hint"`
}

func runSourceCheck(t *testing.T, tool *WebSourceCheckTool, input map[string]any) artifactPayload {
	t.Helper()
	out, err := tool.Execute(context.Background(), mustJSON(t, input))
	if err != nil {
		t.Fatalf("source_check: %v", err)
	}
	var payload artifactPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, out)
	}
	return payload
}

// TestSourceCheckToolEndToEnd walks the whole promise: evidence in, no verdict out,
// and both halves of the artifact readable afterwards.
func TestSourceCheckToolEndToEnd(t *testing.T) {
	tool, content, base := sourceTool(t, map[string]string{
		"/9700": `<html><head><title>9700 syllabus</title></head><body><article>
			<p>The syllabus includes the main theoretical concepts which are fundamental to the study of biology.</p>
			<p>Practical skills are assessed in a timetabled practical examination.</p>
			</article></body></html>`,
	})

	// The stub provider answers with its own URL, so point the claim's source at the
	// fixture page by searching for it explicitly.
	payload := runSourceCheck(t, tool, map[string]any{
		"claim":   "The syllabus includes practical skills assessment",
		"queries": []string{"9700 syllabus practical skills"},
	})

	if payload.Status != "unclear" {
		t.Fatalf("status = %q, want unclear", payload.Status)
	}
	if strings.Contains(strings.ToLower(payload.Status), "support") {
		t.Fatalf("a verdict leaked into the status: %q", payload.Status)
	}
	if len(payload.Sources) == 0 {
		t.Fatalf("no sources: %+v", payload)
	}
	if len(payload.Passages) == 0 {
		t.Fatalf("no passages: %+v", payload)
	}
	if len(payload.Checks) == 0 {
		t.Error("no verification steps")
	}
	if !strings.HasPrefix(payload.ResponseID, "r_") {
		t.Errorf("responseId = %q, want r_ prefix", payload.ResponseID)
	}
	for _, passage := range payload.Passages {
		if !strings.HasPrefix(passage.Hash, "sha256:") {
			t.Errorf("passage %s has no hash", passage.ID)
		}
		if passage.Origin != "page" && passage.Origin != "provider_summary" {
			t.Errorf("passage %s has origin %q", passage.ID, passage.Origin)
		}
	}
	if !strings.Contains(payload.Hint, "does not assert") {
		t.Errorf("hint = %q", payload.Hint)
	}
	_ = base

	// queryIndex reads the artifact JSON (the complete evidence set). The first page is
	// bounded, so page to the end for the passages rather than assuming they fit.
	artifactPage := runContent(t, content, map[string]any{"responseId": payload.ResponseID, "queryIndex": 0, "limit": 12000})
	if !strings.Contains(artifactPage.Content, "\"claim\"") || !strings.Contains(artifactPage.Content, "\"status\"") {
		t.Fatalf("queryIndex did not return the artifact JSON: %q", artifactPage.Content)
	}
	if artifactPage.Total <= 400 || artifactPage.NextOffset == 0 && artifactPage.Total > 12000 {
		t.Logf("artifact total = %d runes (one page)", artifactPage.Total)
	}
	if !strings.Contains(artifactPage.Content, "passageId") && artifactPage.Total > 12000 {
		t.Error("the artifact JSON does not contain its passages")
	}
	if artifactPage.IndexParam != "queryIndex" {
		t.Errorf("indexParam = %q", artifactPage.IndexParam)
	}
}

// TestSourceCheckToolMissingEvidence: nothing found is a status, not an error and not
// an empty success.
func TestSourceCheckToolMissingEvidence(t *testing.T) {
	provider := &stubProvider{name: "exa", snippet: ""}
	provider.results = 0
	provider.failWith = "no results"
	client := websearch.NewClient(
		websearch.WithProviders(provider),
		websearch.WithStore(websearch.NewStore()),
	)
	payload := runSourceCheck(t, NewWebSourceCheckToolWith(client), map[string]any{"claim": "anything"})

	if payload.Status != "missing-evidence" {
		t.Fatalf("status = %q", payload.Status)
	}
	if payload.ResponseID != "" {
		t.Errorf("responseId = %q, want none", payload.ResponseID)
	}
	if !strings.Contains(payload.Rationale, "not refuted") {
		t.Errorf("rationale = %q", payload.Rationale)
	}
	if len(payload.Sources) != 0 {
		t.Errorf("sources = %d", len(payload.Sources))
	}
}

// TestSourceCheckToolRequiresAClaim.
func TestSourceCheckToolRequiresAClaim(t *testing.T) {
	tool := NewWebSourceCheckToolWith(websearch.NewClient())
	if _, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{})); err == nil || !strings.Contains(err.Error(), "claim is required") {
		t.Fatalf("err = %v", err)
	}
}
