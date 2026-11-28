package websearch

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// collapse folds every whitespace run into one space and trims the ends.
//
// Providers send page text with newlines, tabs and runs of spaces. Snippets are
// displayed inline and counted against a rune budget, so the collapsed form is
// the only one worth storing: it keeps budgeting honest and stops a single
// snippet from looking like a multi-paragraph document it is not.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// truncateRunes returns the longest prefix of s of at most max runes, and
// whether anything was cut.
//
// Runes, not bytes: a 400-byte cap silently halves a Chinese snippet relative to
// an English one, and a byte cut can also split a multi-byte rune — the model
// would then read U+FFFD at every boundary. tools/ has the same rule for the
// same reason.
func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	if utf8RuneCount(s) <= max {
		return s, false
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i], true
		}
		count++
	}
	return s, false
}

// truncateRunesOrBytes is truncateRunes for callers that only want the prefix
// (error excerpts).
func truncateRunesOrBytes(s string, max int) string {
	out, _ := truncateRunes(s, max)
	return out
}

func utf8RuneCount(s string) int {
	count := 0
	for range s {
		count++
	}
	return count
}

// normalizeDomains cleans a domainFilter list into provider-ready entries.
//
// Accepted per entry:
//
//	example.com            include
//	-example.com           exclude
//	https://example.com/x  include example.com (scheme and path are dropped)
//
// An entry that is only "-" or only a scheme is an error: silently turning it
// into an empty filter would return everything while the caller believes it
// narrowed the search — the failure mode is invisible in the results.
func normalizeDomains(domains []string) ([]string, error) {
	if len(domains) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(domains))
	out := make([]string, 0, len(domains))
	for _, entry := range domains {
		raw := strings.TrimSpace(entry)
		if raw == "" {
			continue
		}
		exclude := strings.HasPrefix(raw, "-")
		host := strings.TrimPrefix(raw, "-")

		if strings.Contains(host, "://") {
			parsed, err := url.Parse(host)
			if err != nil || parsed.Hostname() == "" {
				return nil, fmt.Errorf("invalid domainFilter entry %q", entry)
			}
			host = parsed.Hostname()
		} else if cut, _, found := strings.Cut(host, "/"); found {
			host = cut
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if host == "" {
			return nil, fmt.Errorf("invalid domainFilter entry %q: a domain is required", entry)
		}

		key := host
		if exclude {
			key = "-" + host
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}
