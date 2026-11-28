package websearch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/go-shiori/go-readability"
	"golang.org/x/net/html/charset"

	"github.com/liup215/cobot/docparse"
)

// fetch_content: give it a URL, get the page's TEXT.
//
// Division of labour with the search tools:
//
//	web_search          finds candidates (titles, URLs, snippet heads)
//	fetch_content       reads ONE page in full (this file)
//	get_search_content  reads back either, in pages or by findText
//
// The third tool is what makes the second one affordable: a fetched page can be
// hundreds of kilobytes, so the response carries a short excerpt and the full text
// goes into the same store get_search_content already reads (KindFetch).
//
// Fetch modes:
//
//	readable (default)  HTML → readability → markdown; PDF/Office → text
//	raw                 the body as text, no extraction (APIs, .txt, JSON)
//
// Deliberately absent (each is its own capability layer, not a flag):
// rendered/JS pages (needs a browser), cookie auth, hosted unlockers, video.

// Fetch modes accepted by the tool schema.
const (
	FetchModeReadable = "readable"
	FetchModeRaw      = "raw"
)

// FetchRequest mirrors the fetch_content schema.
type FetchRequest struct {
	URL  string   `json:"url"`
	URLs []string `json:"urls"`
	Mode string   `json:"mode"`
}

// URLSummary is one URL's entry in the model-facing payload.
type URLSummary struct {
	URL      string `json:"url"`
	Title    string `json:"title,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Status   int    `json:"status,omitempty"`
	// Via names the pipeline ("html", "pdf", "document", "text", "raw").
	Via string `json:"via,omitempty"`
	// Chars is the length of the STORED text in runes.
	Chars int `json:"chars"`
	// Excerpt is the head of the stored text (bounded); the rest is paged.
	Excerpt string `json:"excerpt,omitempty"`
	// Error is per URL: one bad URL never discards another one's text.
	Error string `json:"error"`
}

// FetchResponse is the fetch_content tool result.
type FetchResponse struct {
	// ResponseID is empty when every URL failed — there is nothing to page.
	ResponseID string       `json:"responseId"`
	URLs       []URLSummary `json:"urls"`
	Hint       string       `json:"hint"`
}

// Fetch runs the request: validate, fetch (up to three concurrently), extract,
// store, and return bounded summaries.
func (c *Client) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	mode, err := normalizeFetchMode(req.Mode)
	if err != nil {
		return nil, err
	}
	targets, err := c.normalizeFetchURLs(req)
	if err != nil {
		return nil, err
	}

	stored := make([]StoredURL, len(targets))
	sem := make(chan struct{}, c.concurrency)
	done := make(chan struct{}, len(targets))
	for i, target := range targets {
		go func(i int, target string) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			stored[i] = c.fetchOne(ctx, target, mode)
		}(i, target)
	}
	for range targets {
		<-done
	}

	// Cancellation is a real failure, not a page of errors.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	summaries := make([]URLSummary, 0, len(stored))
	readable := make([]StoredURL, 0, len(stored))
	for _, doc := range stored {
		summary := URLSummary{
			URL:      doc.URL,
			Title:    doc.Title,
			MIMEType: doc.MIMEType,
			Status:   doc.Status,
			Via:      doc.Via,
			Chars:    utf8RuneCount(doc.Content),
			Error:    doc.Error,
		}
		// Content decides what is stored, NOT the absence of an error: raw mode
		// deliberately keeps a non-2xx body (an API's 404 answer is often the answer
		// the caller wanted), and that body must be pageable even though the status
		// is reported as an error.
		if doc.Content != "" {
			excerpt, _ := truncateRunes(doc.Content, c.fetchExcerptRunes)
			summary.Excerpt = excerpt
			readable = append(readable, doc)
		}
		summaries = append(summaries, summary)
	}

	response := &FetchResponse{URLs: summaries}
	if len(readable) == 0 {
		// Nothing was extracted, so there is no id to hand out. Saying so beats
		// returning an id whose content is empty.
		response.Hint = "No page text was extracted, so nothing was stored. Fix the URL (or the page) and fetch again — do not retry the same URL unchanged."
		return response, nil
	}

	response.ResponseID = c.store.PutFetch(stored)
	response.Hint = fmt.Sprintf(
		`Full text of %d page(s) is stored as responseId %q. Read it with get_search_content({"responseId": %q, "urlIndex": 0, "limit": %d}); pass findText to locate a specific fact without paging.`,
		len(readable), response.ResponseID, response.ResponseID, DefaultPageRunes,
	)
	return response, nil
}

// normalizeFetchMode validates the mode ("" = readable).
func normalizeFetchMode(value string) (string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(value)); mode {
	case "", FetchModeReadable:
		return FetchModeReadable, nil
	case FetchModeRaw:
		return FetchModeRaw, nil
	default:
		return "", fmt.Errorf("mode must be readable or raw (got %q)", value)
	}
}

// normalizeFetchURLs collects url/urls, de-duplicates and caps the count.
func (c *Client) normalizeFetchURLs(req FetchRequest) ([]string, error) {
	raw := make([]string, 0, 1+len(req.URLs))
	if s := strings.TrimSpace(req.URL); s != "" {
		raw = append(raw, s)
	}
	for _, u := range req.URLs {
		if s := strings.TrimSpace(u); s != "" {
			raw = append(raw, s)
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("a URL is required (pass url or a non-empty urls list)")
	}
	seen := make(map[string]bool, len(raw))
	targets := make([]string, 0, len(raw))
	for _, u := range raw {
		if seen[u] {
			continue
		}
		seen[u] = true
		targets = append(targets, u)
	}
	if len(targets) > MaxFetchURLs {
		return nil, fmt.Errorf("too many URLs: %d (max %d per call — fetch the ones you will actually read)", len(targets), MaxFetchURLs)
	}
	return targets, nil
}

// fetchOne fetches and extracts a single URL. Every failure is reported as
// StoredURL.Error rather than an error, so one dead link cannot void the others.
func (c *Client) fetchOne(ctx context.Context, rawURL, mode string) StoredURL {
	doc := StoredURL{URL: rawURL}

	parsed, err := validateFetchURL(rawURL, c.guard)
	if err != nil {
		doc.Error = err.Error()
		return doc
	}

	client := c.fetchClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		doc.Error = fmt.Sprintf("build request: %v", err)
		return doc
	}
	// Identify honestly, and ask for HTML first: a page that serves a different
	// representation to a bot is a page we should not pretend to have read.
	req.Header.Set("User-Agent", "cobot/1.0 (+web fetch)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf,text/plain;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		doc.Error = describeFetchError(err)
		return doc
	}
	defer resp.Body.Close()

	doc.Status = resp.StatusCode
	doc.MIMEType = resp.Header.Get("Content-Type")

	body, tooBig, err := readBounded(resp.Body, MaxFetchBytes)
	if err != nil {
		doc.Error = fmt.Sprintf("read body: %v", err)
		return doc
	}
	if tooBig {
		doc.Error = fmt.Sprintf("response is larger than %d MB — not fetched", MaxFetchBytes>>20)
		return doc
	}

	// Non-2xx: report the status, keep the body only for raw mode (an API error
	// body is often the answer the caller wants).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if mode == FetchModeRaw {
			text, _ := decodeText(body, doc.MIMEType)
			doc.Content = text
			doc.Via = "raw"
		}
		doc.Error = fmt.Sprintf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		return doc
	}

	if mode == FetchModeRaw {
		text, err := decodeText(body, doc.MIMEType)
		if err != nil {
			doc.Error = err.Error()
			return doc
		}
		doc.Content = capStoredText(text)
		doc.Via = "raw"
		doc.Title = hostLabel(parsed.String())
		return doc
	}

	return c.extractDoc(parsed, doc, body)
}

// extractDoc dispatches on content type. Order matters: magic bytes beat headers
// (a mislabelled PDF is common), then explicit types, then a sniff.
func (c *Client) extractDoc(parsed *url.URL, doc StoredURL, body []byte) StoredURL {
	ctype := strings.ToLower(doc.MIMEType)

	switch {
	case bytes.HasPrefix(body, []byte("%PDF")) || strings.Contains(ctype, "application/pdf"):
		text, err := extractPDF(body)
		if err != nil {
			doc.Error = err.Error()
			return doc
		}
		doc.Content = capStoredText(text)
		doc.Via = "pdf"
		doc.Title = firstPDFTitle(text)
		return doc

	case docparse.NeedsExtraction(body):
		// Word/Excel/PowerPoint/ODF/EPUB: docparse already owns these formats
		// (the read tool uses it), so fetching a .docx and reading it locally
		// produce the same text.
		text, err := extractDocument(parsed.Path, body)
		if err != nil {
			doc.Error = err.Error()
			return doc
		}
		doc.Content = capStoredText(text)
		doc.Via = "document"
		doc.Title = hostLabel(parsed.String())
		return doc

	case strings.HasPrefix(ctype, "image/"):
		doc.Error = "the URL is an image, not text — fetch the page that shows it, or attach the image to the conversation"
		return doc

	case isHTML(ctype, body):
		return extractHTML(parsed, doc, body)

	case strings.HasPrefix(ctype, "text/") ||
		strings.Contains(ctype, "json") || strings.Contains(ctype, "xml") ||
		strings.Contains(ctype, "csv") || strings.Contains(ctype, "javascript"):
		text, err := decodeText(body, doc.MIMEType)
		if err != nil {
			doc.Error = err.Error()
			return doc
		}
		doc.Content = capStoredText(text)
		doc.Via = "text"
		doc.Title = hostLabel(parsed.String())
		return doc
	}

	doc.Error = fmt.Sprintf("cannot read %q as text (content type %q) — only HTML, PDF, office documents and text formats are supported", parsed.Host, doc.MIMEType)
	return doc
}

// extractHTML is the readable pipeline: decode → readability → markdown, with two
// fallbacks so a page that readability rejects still yields usable text.
//
// Why markdown and not plain text: headings, lists and links carry meaning in a
// syllabus page, and the model quotes them back. Why the fallbacks: readability
// returns "no article" for plenty of legitimate pages (docs indexes, tables of
// contents), and answering "could not read it" for those would be a lie about the
// page rather than about our parser.
func extractHTML(parsed *url.URL, doc StoredURL, body []byte) StoredURL {
	decoded, err := decodeText(body, doc.MIMEType)
	if err != nil {
		doc.Error = err.Error()
		return doc
	}

	doc.Via = "html"
	doc.Title = documentTitle(decoded)
	if doc.Title == "" {
		doc.Title = hostLabel(parsed.String())
	}

	if article, err := readability.FromReader(strings.NewReader(decoded), parsed); err == nil {
		if title := strings.TrimSpace(article.Title); title != "" {
			doc.Title = title
		}
		if markdown, convErr := htmltomarkdown.ConvertString(article.Content); convErr == nil {
			if text := strings.TrimSpace(markdown); text != "" {
				doc.Content = capStoredText(text)
				return doc
			}
		}
	}

	// Fallback 1: the whole document as markdown (keeps lists/tables/links).
	if markdown, err := htmltomarkdown.ConvertString(decoded); err == nil {
		if text := collapseMarkdown(markdown); text != "" {
			doc.Content = capStoredText(text)
			return doc
		}
	}

	// Fallback 2: tag stripping (docparse), which never returns markup.
	if text, err := docparse.ExtractText(parsed.Path, body); err == nil {
		if strings.TrimSpace(text) != "" {
			doc.Via = "html-text"
			doc.Content = capStoredText(text)
			return doc
		}
	}

	doc.Error = "the page yielded no readable text (it may be script-rendered or a paywall)"
	return doc
}

// extractPDF writes the bytes to a temp file and lets docparse do the work —
// its PDF parser is file-based, and it already carries the format knowledge the
// read tool uses.
func extractPDF(body []byte) (string, error) {
	file, err := os.CreateTemp("", "cobot-fetch-*.pdf")
	if err != nil {
		return "", fmt.Errorf("pdf: create temp file: %w", err)
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(body); err != nil {
		file.Close()
		return "", fmt.Errorf("pdf: write temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("pdf: close temp file: %w", err)
	}

	text, err := docparse.ParseDocument(path)
	if err != nil {
		return "", fmt.Errorf("pdf: %w", err)
	}
	return text, nil
}

// extractDocument is extractPDF for the other office/epub containers.
func extractDocument(name string, body []byte) (string, error) {
	suffix := filepath.Ext(name)
	if suffix == "" {
		suffix = ".bin"
	}
	file, err := os.CreateTemp("", "cobot-fetch-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(body); err != nil {
		file.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close temp file: %w", err)
	}
	text, err := docparse.ParseDocument(path)
	if err != nil {
		return "", err
	}
	return text, nil
}

// readBounded reads at most limit bytes and reports whether the body was longer.
func readBounded(body io.Reader, limit int64) ([]byte, bool, error) {
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		return nil, true, nil
	}
	return buf, false, nil
}

// decodeText converts a body to UTF-8 using the declared charset, the BOM or a
// sniff — CJK pages declare GBK/Shift_JIS often enough that assuming UTF-8 is not
// an option.
func decodeText(body []byte, contentType string) (string, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return "", fmt.Errorf("decode text: %w", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("decode text: %w", err)
	}
	return string(decoded), nil
}

// isHTML prefers the header but sniffs when it is missing or generic.
func isHTML(contentType string, body []byte) bool {
	if strings.Contains(contentType, "html") {
		return true
	}
	if contentType != "" && !strings.HasPrefix(contentType, "text/") {
		return false
	}
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	lower := strings.ToLower(string(head))
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<body")
}

// documentTitle pulls <title> without a full parse (readability overwrites this
// with a better value when it succeeds).
func documentTitle(htmlText string) string {
	lower := strings.ToLower(htmlText)
	start := strings.Index(lower, "<title")
	if start < 0 {
		return ""
	}
	open := strings.Index(lower[start:], ">")
	if open < 0 {
		return ""
	}
	open += start + 1
	end := strings.Index(lower[open:], "</title")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(collapse(htmlText[open : open+end]))
}

// collapseMarkdown removes the blank-line noise an empty conversion leaves behind.
func collapseMarkdown(markdown string) string {
	lines := strings.Split(markdown, "\n")
	kept := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// capStoredText bounds one stored document. A model reading 200k runes is reading
// nothing useful; the cap also keeps the store's byte budget meaningful.
func capStoredText(text string) string {
	text = strings.TrimSpace(text)
	if cut, tooLong := truncateRunes(text, MaxStoredTextRunes); tooLong {
		return cut + "\n\n[truncated: the page is longer than the storage cap]"
	}
	return text
}

// firstPDFTitle takes the first non-empty line as a title — PDFs have no reliable
// metadata we can trust, and the first line of a syllabus is its name.
func firstPDFTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			if cut, tooLong := truncateRunes(trimmed, 120); tooLong {
				return cut
			}
			return trimmed
		}
	}
	return ""
}

// describeFetchError turns transport failures into an instruction. "connection
// refused" tells a model nothing; "unreachable from here" tells it to stop.
func describeFetchError(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "refusing"):
		return message // our own guard, already explicit
	case strings.Contains(message, "no such host"):
		return fmt.Sprintf("host not found (%s) — check the URL", message)
	case strings.Contains(message, "timeout") || strings.Contains(message, "deadline exceeded"):
		return fmt.Sprintf("timed out (%s) — the site may be slow or blocked from this machine", message)
	case strings.Contains(message, "connection refused"), strings.Contains(message, "connectex"):
		return fmt.Sprintf("connection refused (%s) — the site may block this machine", message)
	case strings.Contains(message, "certificate"), strings.Contains(message, "tls"):
		return fmt.Sprintf("TLS failed (%s) — the site's certificate could not be verified", message)
	}
	return message
}
