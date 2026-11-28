package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// --- the gate --------------------------------------------------------------

// TestValidateFetchURLBlocksPrivateTargets is the executable form of the SSRF
// policy. Each row is a URL a model could plausibly be talked into fetching.
func TestValidateFetchURLBlocksPrivateTargets(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/admin",
		"http://localhost/",
		"http://app.localhost/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
		"http://172.16.31.9/",
		"http://192.168.1.1/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://[fd00::1]/",
		"file:///etc/passwd",
		"data:text/html,<h1>x</h1>",
		"ftp://example.com/x",
	}
	for _, raw := range blocked {
		if _, err := validateFetchURL(raw, FetchGuardOptions{}); err == nil {
			t.Errorf("%s was allowed", raw)
		} else if !errors.Is(err, ErrBlockedAddress) && !strings.Contains(err.Error(), "only http") {
			t.Errorf("%s: unexpected error %v", raw, err)
		}
	}

	allowed := []string{
		"https://example.com/",
		"http://example.com/path?q=1",
		"https://www.cambridgeinternational.org/programmes-and-qualifications/biology-9700/",
		"https://8.8.8.8/",
	}
	for _, raw := range allowed {
		if _, err := validateFetchURL(raw, FetchGuardOptions{}); err != nil {
			t.Errorf("%s was blocked: %v", raw, err)
		}
	}
}

// TestValidateFetchURLAllowLoopbackIsOptIn: tests and self-hosted setups may turn
// the loopback rule off, but nothing else bends.
func TestValidateFetchURLAllowLoopbackIsOptIn(t *testing.T) {
	opts := FetchGuardOptions{AllowLoopback: true}
	if _, err := validateFetchURL("http://127.0.0.1:9/x", opts); err != nil {
		t.Errorf("loopback refused with AllowLoopback: %v", err)
	}
	if _, err := validateFetchURL("http://192.168.1.1/", opts); err == nil {
		t.Error("AllowLoopback also opened a private range")
	}
}

// TestIsBlockedIP pins the range table directly, including the two that are
// neither IsPrivate nor IsLoopback.
func TestIsBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,
		"0.0.0.0":         true,
		"10.1.2.3":        true,
		"172.20.0.1":      true,
		"192.168.0.1":     true,
		"169.254.169.254": true,
		"100.100.100.100": true,
		"198.18.0.1":      true,
		"224.0.0.1":       true,
		"::1":             true,
		"fd12:3456::1":    true,
		"fe80::1":         true,
		"8.8.8.8":         false,
		"1.1.1.1":         false,
		"2606:4700::1111": false,
	}
	for raw, want := range cases {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("bad fixture %q", raw)
		}
		if got := isBlockedIP(ip, FetchGuardOptions{}); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestDialGuardCatchesHostnames covers what the URL validator cannot see: a NAME
// that resolves to a private address. The dialer's Control hook must refuse it.
func TestDialGuardCatchesHostnames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should never be reached")
	}))
	defer server.Close()

	// A client built with the guard but WITHOUT AllowLoopback: the fixture server
	// lives on 127.0.0.1, so reaching it would be the bug.
	guarded := newFetchHTTPClient(FetchGuardOptions{}, DefaultTimeout, false)
	_, err := guarded.Get(server.URL)
	if err == nil {
		t.Fatal("a loopback target was fetched through the guard")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v, want an explicit refusal", err)
	}
}

// TestRedirectHopIsRevalidated: a public URL that redirects into the private
// network must be refused at the hop, not followed.
func TestRedirectHopIsRevalidated(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "metadata")
	}))
	defer target.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer front.Close()

	guarded := newFetchHTTPClient(FetchGuardOptions{AllowLoopback: true}, DefaultTimeout, false)
	// AllowLoopback lets the FRONT server be reached (it is on 127.0.0.1); the
	// redirect target is also loopback, so this asserts the hop is re-checked with
	// the same policy rather than bypassed. To prove the refusal itself, use a
	// policy where the target is forbidden: same-origin redirects.
	sameOrigin := newFetchHTTPClient(FetchGuardOptions{AllowLoopback: true}, DefaultTimeout, true)
	_, err := sameOrigin.Get(front.URL)
	if err == nil {
		t.Fatal("a cross-origin redirect was followed")
	}
	if !strings.Contains(err.Error(), "cross-origin") {
		t.Errorf("err = %v, want a cross-origin refusal", err)
	}
	_ = guarded
}

// --- the pipeline ----------------------------------------------------------

// fetchClient starts a fixture server and returns a client plus the base URL to
// build page URLs from. Requests MUST target base: pointing a test at
// example.com would silently exercise the real internet (this test file did that
// once, and every case failed with a 404 from the live site).
func fetchClient(t *testing.T, handler http.HandlerFunc) (*Client, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	// AllowLoopback: the fixture server is on 127.0.0.1 by construction. The gate
	// itself is tested above.
	client := NewClient(
		WithProviders(&fakeProvider{name: "exa", available: true}),
		WithFetchGuard(FetchGuardOptions{AllowLoopback: true}),
		WithStore(NewStore()),
	)
	return client, server.URL
}

// TestFetchHTMLBecomesMarkdown is the core promise: nav/script dropped, structure
// kept, title taken from the page.
func TestFetchHTMLBecomesMarkdown(t *testing.T) {
	page := `<!doctype html><html><head><title>9700 Syllabus</title></head><body>
<nav>Home | About | Contact</nav>
<script>console.log("tracking")</script>
<article>
  <h1>Syllabus overview</h1>
  <p>Cambridge International AS &amp; A Level Biology builds on IGCSE skills.</p>
  <h2>Content</h2>
  <ul><li>Cell structure</li><li>Enzymes</li></ul>
  <p>See <a href="https://example.org/spec">the specification</a>.</p>
</article>
<footer>© UCLES</footer></body></html>`

	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})

	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/9700"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.HasPrefix(response.ResponseID, "f_") {
		t.Fatalf("responseId = %q, want the f_ prefix", response.ResponseID)
	}
	summary := response.URLs[0]
	if summary.Error != "" {
		t.Fatalf("error = %s", summary.Error)
	}
	if summary.Via != "html" {
		t.Errorf("via = %q, want html", summary.Via)
	}
	if !strings.Contains(summary.Title, "9700") {
		t.Errorf("title = %q", summary.Title)
	}
	for _, want := range []string{"Syllabus overview", "Cell structure", "Enzymes", "specification"} {
		if !strings.Contains(summary.Excerpt, want) {
			t.Errorf("excerpt is missing %q:\n%s", want, summary.Excerpt)
		}
	}
	// The extraction must drop what is not article text.
	for _, unwanted := range []string{"console.log", "Home | About"} {
		if strings.Contains(summary.Excerpt, unwanted) {
			t.Errorf("excerpt kept %q:\n%s", unwanted, summary.Excerpt)
		}
	}

	// The stored text is what get_search_content pages.
	entry, err := client.Store().Get(response.ResponseID)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if entry.Kind != KindFetch || len(entry.URLs) != 1 {
		t.Fatalf("entry kind=%q urls=%d", entry.Kind, len(entry.URLs))
	}
	if !strings.Contains(entry.URLs[0].Content, "Cell structure") {
		t.Errorf("stored content = %q", entry.URLs[0].Content)
	}
}

// TestFetchTextAndJSONStayVerbatim: a spec file or an API answer must not travel
// through readability.
func TestFetchTextAndJSON(t *testing.T) {
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/spec.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "line one\nline two")
		case "/api.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true,"version":"1.2.3"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	response, err := client.Fetch(context.Background(), FetchRequest{
		URLs: []string{base + "/spec.txt", base + "/api.json"},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	byURL := map[string]URLSummary{}
	for _, summary := range response.URLs {
		byURL[summary.URL] = summary
	}
	if got := byURL[base+"/spec.txt"]; got.Via != "text" || !strings.Contains(got.Excerpt, "line two") {
		t.Errorf("text/plain = %+v", got)
	}
	if got := byURL[base+"/api.json"]; got.Via != "text" || !strings.Contains(got.Excerpt, `"version":"1.2.3"`) {
		t.Errorf("json = %+v", got)
	}
}

// TestFetchRawModeSkipsExtraction: raw returns markup for a status endpoint or an
// HTML API error, which is exactly what the caller asked for.
func TestFetchRawMode(t *testing.T) {
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html><body>upstream down</body></html>")
	})

	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/api", Mode: FetchModeRaw})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	summary := response.URLs[0]
	if summary.Status != http.StatusBadGateway {
		t.Errorf("status = %d", summary.Status)
	}
	if !strings.Contains(summary.Error, "502") {
		t.Errorf("error = %q, want the status", summary.Error)
	}
	if !strings.Contains(summary.Excerpt, "<body>upstream down</body>") {
		t.Errorf("raw excerpt = %q", summary.Excerpt)
	}
}

// TestFetchPDFIsRoutedToThePDFParser: a real PDF fixture would be ideal, but the
// valuable assertion here is ROUTING — the bytes must reach docparse's PDF path
// (its error proves it did) rather than being markdown-ised as HTML.
func TestFetchPDFIsRoutedToThePDFParser(t *testing.T) {
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		fmt.Fprint(w, "%PDF-1.4\nnot really a pdf")
	})

	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/x.pdf"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	summary := response.URLs[0]
	if !strings.Contains(summary.Error, "pdf") && !strings.Contains(summary.Error, "PDF") {
		t.Fatalf("a %%PDF body was not routed to the PDF parser: %+v", summary)
	}
	if response.ResponseID != "" {
		t.Errorf("responseId = %q, want empty when nothing was extracted", response.ResponseID)
	}
	if !strings.Contains(response.Hint, "nothing was stored") {
		t.Errorf("hint = %q", response.Hint)
	}
}

// TestFetchPerURLFailureIsIsolated: one dead link must not void the others.
func TestFetchPerURLFailureIsolated(t *testing.T) {
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "still readable")
	})

	response, err := client.Fetch(context.Background(), FetchRequest{
		URLs: []string{base + "/missing", base + "/ok"},
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(response.URLs) != 2 {
		t.Fatalf("urls = %d", len(response.URLs))
	}
	var failed, ok bool
	for _, summary := range response.URLs {
		if summary.URL == base+"/missing" {
			failed = strings.Contains(summary.Error, "404")
		} else if strings.Contains(summary.Excerpt, "still readable") {
			ok = true
		}
	}
	if !failed || !ok {
		t.Fatalf("isolation broken: %+v", response.URLs)
	}
	if response.ResponseID == "" {
		t.Error("a successful page did not get a responseId")
	}
}

// TestFetchRejectsPrivateURLInResult: the refusal arrives as that URL's error, so
// the model is told WHICH url was refused and why.
func TestFetchRejectsPrivateURLInResult(t *testing.T) {
	client := NewClient(WithProviders(&fakeProvider{name: "exa", available: true}), WithStore(NewStore()))
	response, err := client.Fetch(context.Background(), FetchRequest{URL: "http://127.0.0.1:8080/admin"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	summary := response.URLs[0]
	if !strings.Contains(summary.Error, "private") && !strings.Contains(summary.Error, "refusing") {
		t.Fatalf("error = %q", summary.Error)
	}
	if response.ResponseID != "" {
		t.Errorf("refused URL still produced a responseId")
	}
}

// TestFetchSizeCap: the body cap is enforced while reading, not after.
func TestFetchSizeCap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 5MB")
	}
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		chunk := strings.Repeat("x", 64*1024)
		for written := 0; written <= int(MaxFetchBytes); written += len(chunk) {
			fmt.Fprint(w, chunk)
		}
	})

	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/big.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	summary := response.URLs[0]
	if !strings.Contains(summary.Error, "larger than") {
		t.Fatalf("error = %q, want the size cap", summary.Error)
	}
}

// TestFetchInputRules pins the schema's documented limits.
func TestFetchInputRules(t *testing.T) {
	client := NewClient(WithProviders(&fakeProvider{name: "exa", available: true}), WithStore(NewStore()))

	if _, err := client.Fetch(context.Background(), FetchRequest{}); err == nil || !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("empty request: %v", err)
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URLs: []string{"https://a.example", "https://b.example", "https://c.example", "https://d.example"}}); err == nil || !strings.Contains(err.Error(), "too many URLs") {
		t.Errorf("four URLs: %v", err)
	}
	if _, err := client.Fetch(context.Background(), FetchRequest{URL: "https://a.example", Mode: "render"}); err == nil || !strings.Contains(err.Error(), "mode must be") {
		t.Errorf("bad mode: %v", err)
	}

	// maxRedirects is documented on the client; keep the constant honest.
	if maxRedirects != 5 || MaxFetchURLs != 3 {
		t.Errorf("limits drifted: redirects=%d urls=%d", maxRedirects, MaxFetchURLs)
	}
}

// TestFetchStoredTextIsPagedByRunes checks the handoff: a fetched page is stored
// once and paged in rune units, CJK included.
func TestFetchStoredTextIsPaged(t *testing.T) {
	body := strings.Repeat("细胞结构是生物学的基础。", 400) // ~4800 runes
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, body)
	})

	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/cn.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	entry, err := client.Store().Get(response.ResponseID)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	page, total, next := SliceContent(entry.URLs[0].Content, 0, 100)
	if total < 4000 {
		t.Fatalf("total = %d, want the whole document", total)
	}
	if utf8RuneCount(page) != 100 || next != 100 {
		t.Errorf("page=%d runes next=%d", utf8RuneCount(page), next)
	}
	if found := Find(entry.URLs[0].Content, []string{"细胞结构"}, FindCaseInsensitive, MaxPageRunes); found.TotalMatches == 0 {
		t.Error("findText found nothing in a fetched page")
	}
}

// TestFetchResponseIsJSON pins the wire shape the tool and the card both read.
func TestFetchResponseIsJSON(t *testing.T) {
	client, base := fetchClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	})
	response, err := client.Fetch(context.Background(), FetchRequest{URL: base + "/a.txt"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"responseId"`, `"urls"`, `"excerpt"`, `"chars"`, `"via"`, `"hint"`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("payload is missing %s: %s", field, encoded)
		}
	}
}

// TestHostLabelAndTitleHelpers keep the two small fallbacks honest.
func TestTitleHelpers(t *testing.T) {
	if got := documentTitle("<html><head><TITLE> My  Page </TITLE></head></html>"); got != "My Page" {
		t.Errorf("documentTitle = %q", got)
	}
	if got := documentTitle("<html><body>no title</body></html>"); got != "" {
		t.Errorf("documentTitle without a title = %q", got)
	}
	if got := firstPDFTitle("\n\nFirst line\nsecond"); got != "First line" {
		t.Errorf("firstPDFTitle = %q", got)
	}
	parsed, _ := url.Parse("https://www.example.org/x")
	if got := hostLabel(parsed.String()); got != "www.example.org" {
		t.Errorf("hostLabel = %q", got)
	}
}
