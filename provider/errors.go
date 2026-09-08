package provider

import (
	"errors"
	"fmt"
	"net/http"

	"strings"
	"time"
)

// HTTPError carries structured transport-level error information so callers
// (the agent loop's automatic retry) can distinguish retryable failures from
// permanent ones and honor server-provided Retry-After hints.
//
// Status semantics:
//   - 0: network-level failure (request never reached a server) — retryable
//   - -1: request marshaling/construction failure — not retryable
//   - 1xx-5xx: actual HTTP status — retryable per Retryable()
//
// The Error() text matches the plain fmt.Errorf messages providers used to
// return, so logs and user-facing output stay unchanged.
type HTTPError struct {
	Status  int
	Message string
	// RetryAfter is the delay requested by the server via the Retry-After
	// header (delta-seconds form); 0 if absent or unparseable.
	RetryAfter time.Duration
	// Err is the underlying error for network failures, if any.
	Err error
}

// Error implements the error interface with the server-provided message.
func (e *HTTPError) Error() string { return e.Message }

// Unwrap exposes the underlying network error for errors.Is/As chains.
func (e *HTTPError) Unwrap() error { return e.Err }

// Retryable reports whether a retry has a realistic chance of succeeding:
// network failures (status 0), request timeouts (408), rate limiting (429)
// and server-side errors (5xx). Client parameter errors (4xx other than
// 408/429) are permanent and must not be retried.
func (e *HTTPError) Retryable() bool {
	switch {
	case e.Status == 0:
		return true // network error
	case e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests:
		return true
	case e.Status >= 500 && e.Status <= 599:
		return true
	default:
		return false
	}
}

// AsHTTPError extracts an *HTTPError from an error chain, if present.
func AsHTTPError(err error) (*HTTPError, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return he, true
	}
	return nil, false
}

// IsRetryable reports whether err is worth retrying. Only *HTTPError values
// (or chains containing one) participate; any other error type is considered
// permanent.
func IsRetryable(err error) bool {
	if he, ok := AsHTTPError(err); ok {
		return he.Retryable()
	}
	return false
}

// parseRetryAfter parses the Retry-After header in delta-seconds form
// (e.g. "30"). HTTP-date form is not supported and yields 0.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconvParsePositiveInt(v)
	if err != nil {
		return 0
	}
	if secs > 300 { // cap absurd values at 5 minutes
		secs = 300
	}
	return time.Duration(secs) * time.Second
}

func strconvParsePositiveInt(s string) (int, error) {
	var n int
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0, fmt.Errorf("too large")
		}
	}
	return n, nil
}
