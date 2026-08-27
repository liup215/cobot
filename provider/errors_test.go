package provider

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestHTTPErrorRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{0, true},    // network failure
		{408, true},  // request timeout
		{429, true},  // rate limited
		{500, true},  // server error
		{502, true},  // bad gateway
		{503, true},  // service unavailable
		{529, true},  // overloaded (Anthropic-style)
		{400, false}, // bad request
		{401, false}, // unauthorized
		{403, false}, // forbidden
		{404, false}, // not found
		{422, false}, // unprocessable entity
	}
	for _, c := range cases {
		e := &HTTPError{Status: c.status, Message: fmt.Sprintf("status %d", c.status)}
		if got := e.Retryable(); got != c.want {
			t.Errorf("HTTPError{Status:%d}.Retryable() = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestHTTPErrorErrorText(t *testing.T) {
	e := &HTTPError{Status: 429, Message: "OpenAI API returned status 429: slow down"}
	if e.Error() != "OpenAI API returned status 429: slow down" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestAsHTTPErrorAndIsRetryable(t *testing.T) {
	he := &HTTPError{Status: 503, Message: "overloaded"}
	wrapped := fmt.Errorf("stream setup failed: %w", he)

	got, ok := AsHTTPError(wrapped)
	if !ok || got.Status != 503 {
		t.Fatalf("AsHTTPError(wrapped) = %v, %v", got, ok)
	}
	if !IsRetryable(wrapped) {
		t.Error("IsRetryable(wrapped) = false, want true")
	}
	plain := errors.New("some other failure")
	if IsRetryable(plain) {
		t.Error("IsRetryable(plain error) = true, want false (non-HTTP errors are not retried)")
	}
	if _, ok := AsHTTPError(plain); ok {
		t.Error("AsHTTPError(plain) should not match")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"abc", 0}, // HTTP-date form not supported
		{"Wed, 01 Jan 2026 00:00:00 GMT", 0},
		{"30", 30 * time.Second},
		{"2", 2 * time.Second},
		{"999999", 300 * time.Second}, // capped at 5 minutes
		{"-5", 0},
	}
	for _, c := range cases {
		if got := parseRetryAfter(c.in); got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
