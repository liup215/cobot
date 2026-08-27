package loop

import (
	"context"
	"time"

	"github.com/liup215/agentcore/provider"
)

// DefaultRetryDelays is the default exponential backoff schedule: each
// entry is the wait before retry attempt i+1 (1s, 2s, 4s, 8s — four
// retries total). A configured Retry-After hint overrides the delay for
// that attempt.
var DefaultRetryDelays = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// retryDelaysFor returns the effective retry schedule: the Options value
// when set, the DefaultRetryDelays otherwise. An empty non-nil slice
// disables retrying entirely.
func retryDelaysFor(configured []time.Duration) []time.Duration {
	if configured == nil {
		return DefaultRetryDelays
	}
	return configured
}

// createStreamWithRetry opens the LLM stream with automatic retries for
// transient connection failures (network errors, 408/429, 5xx; honoring
// Retry-After hints). 4xx parameter errors and mid-stream errors are NOT
// retried here — the former are permanent, the latter flow through the
// correction loop as usual.
//
// Each retry reports OnRetry(attempt, err, nextDelay) so callers can show
// progress. Context cancellation during a backoff wait aborts immediately.
func (a *BaseAgent) createStreamWithRetry(ctx context.Context, req *MessageRequest, callback StreamCallback) (<-chan StreamChunk, error) {
	delays := retryDelaysFor(a.retryDelays)

	streamChan, err := a.provider.CreateMessageStream(ctx, req)
	if err == nil {
		return streamChan, nil
	}

	maxRetries := len(delays)
	for attempt := 0; attempt < maxRetries && provider.IsRetryable(err); attempt++ {
		delay := delays[attempt]
		if he, ok := provider.AsHTTPError(err); ok && he.RetryAfter > delay {
			delay = he.RetryAfter // respect the server's hint when longer
		}
		logWarnf("Stream connection failed (attempt %d/%d): %v — retrying in %v",
			attempt+1, maxRetries, err, delay)
		if callback != nil {
			callback.OnRetry(attempt+1, err, delay)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		streamChan, err = a.provider.CreateMessageStream(ctx, req)
		if err == nil {
			return streamChan, nil
		}
	}
	return nil, err
}
