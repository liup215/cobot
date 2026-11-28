package websearch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// SSRF gate for fetch_content.
//
// Why this exists at all: web_search only ever talks to endpoints cobot itself
// configures, but fetch_content takes a URL FROM THE MODEL. Without a gate, "read
// this page" is a request the model can point at 127.0.0.1:8080, at the router's
// admin page on 192.168.1.1, or at a cloud metadata service on 169.254.169.254 —
// and the answer would be rendered back into the transcript as if it were public
// web content. That is the whole attack: the tool becomes a blind proxy into the
// host's private network.
//
// The guard has two layers on purpose:
//
//  1. validateFetchURL rejects a bad URL up front, so the error names the URL
//     rather than a dialing failure;
//  2. the transport's Dialer.Control re-checks the RESOLVED IP for every
//     connection. Layer 1 alone is defeated by a DNS name that resolves to
//     127.0.0.1 (or rebinds between the check and the dial), which is exactly the
//     trick this class of bug is known for.
//
// Redirects are re-validated hop by hop by the same validator: a public URL that
// 302s to http://169.254.169.254/ must be refused at the second hop, not followed.

// MaxFetchURLs bounds one fetch call. Three is pi's number: enough to compare
// three sources, few enough that one tool result cannot become a crawl.
const MaxFetchURLs = 3

// maxRedirects is the hop limit; beyond it the fetch fails rather than chases.
const maxRedirects = 5

// ErrBlockedAddress is returned for a URL that points into the host's private
// network. Exported so a host can tell "you are not allowed" from "it failed".
var ErrBlockedAddress = errors.New("refusing to fetch a private or local address")

// FetchGuardOptions tunes the gate. The zero value is the safe one.
type FetchGuardOptions struct {
	// AllowLoopback permits 127.0.0.0/8 and ::1. Only for tests and for a host
	// that deliberately serves content from its own machine — never for a
	// user-facing default, which is why it is opt-in.
	AllowLoopback bool
}

// validateFetchURL checks scheme, host and literal-IP cases up front.
func validateFetchURL(raw string, opts FetchGuardOptions) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("a URL is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", trimmed, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		// file:, data:, ftp:, gopher: … are not "pages" and several of them read
		// the local disk (or worse) without any network at all.
		return nil, fmt.Errorf("only http and https URLs can be fetched (got %q)", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("invalid URL %q: no host", trimmed)
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		if !opts.AllowLoopback {
			return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, host)
		}
		return parsed, nil
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedIP(ip, opts) {
		return nil, fmt.Errorf("%w: %s", ErrBlockedAddress, host)
	}
	return parsed, nil
}

// isBlockedIP is the address policy. It runs on the RESOLVED address, so it also
// covers names.
func isBlockedIP(ip net.IP, opts FetchGuardOptions) bool {
	if ip == nil {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4.IsLoopback():
			return !opts.AllowLoopback
		case ip4.IsUnspecified(), ip4.IsMulticast():
			return true
		case ip4.IsPrivate(): // 10/8, 172.16/12, 192.168/16
			return true
		case ip4.IsLinkLocalUnicast(): // 169.254/16, incl. 169.254.169.254
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64/10 carrier NAT
			return true
		case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 198.18/15 benchmarking
			return true
		case ip4[0] == 0: // "this network"
			return true
		}
		return false
	}
	// IPv6
	switch {
	case ip.IsLoopback():
		return !opts.AllowLoopback
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsLinkLocalUnicast():
		return true
	case ip.IsPrivate(): // fc00::/7 unique local
		return true
	}
	return false
}

// newFetchHTTPClient builds the guarded client. EVERY connection — first hop and
// redirects alike — passes through Control with the resolved address, so a host
// that resolves to a private IP is refused at dial time.
func newFetchHTTPClient(opts FetchGuardOptions, timeout time.Duration, sameHostOnly bool) *http.Client {
	if timeout <= 0 {
		timeout = FetchTimeout
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 15 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("websearch: unparsable dial address %q", address)
			}
			ip := net.ParseIP(strings.Trim(host, "[]"))
			if ip == nil {
				// Not an IP: the transport resolved a name we cannot inspect, so
				// the safe answer is "no".
				return fmt.Errorf("websearch: refusing to dial unresolved host %q", host)
			}
			if isBlockedIP(ip, opts) {
				return fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
			}
			return nil
		},
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if _, err := validateFetchURL(req.URL.String(), opts); err != nil {
				return err
			}
			if sameHostOnly && len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("refusing a cross-origin redirect to %s", req.URL.Host)
			}
			return nil
		},
	}
}

// syscallRawConn placeholder removed: Dialer.Control takes syscall.RawConn.
