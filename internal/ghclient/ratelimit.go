package ghclient

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	// defaultMaxRateLimitWait caps how long a single request will block waiting
	// for a rate limit to reset before giving up and surfacing the error (which
	// the runner then treats as transient and re-checks later).
	defaultMaxRateLimitWait = 15 * time.Minute

	// defaultRateLimitRetries bounds the number of waits per request as a safety
	// net against a server that keeps reporting a limit.
	defaultRateLimitRetries = 3
)

// rateLimitTransport waits out GitHub primary (X-RateLimit-Reset) and secondary
// (Retry-After) rate limits and retries the request, so a temporary limit does
// not bubble up as a failure. Waits longer than maxWait are not taken.
type rateLimitTransport struct {
	base    http.RoundTripper
	maxWait time.Duration
	retries int
	wait    func(ctx context.Context, d time.Duration) error
}

func newRateLimitTransport(base http.RoundTripper, maxWait time.Duration, retries int) *rateLimitTransport {
	return &rateLimitTransport{
		base:    base,
		maxWait: maxWait,
		retries: retries,
		wait:    waitFor,
	}
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return resp, err
		}

		delay, limited := rateLimitWait(resp)
		if !limited || delay > t.maxWait || attempt >= t.retries {
			return resp, nil
		}

		// A retry replays the body, so it must be rewindable; bail out otherwise.
		if req.Body != nil {
			if req.GetBody == nil {
				return resp, nil
			}
			body, gerr := req.GetBody()
			if gerr != nil {
				return resp, nil
			}
			req.Body = body
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if werr := t.wait(req.Context(), delay); werr != nil {
			return nil, werr
		}
	}
}

// rateLimitWait returns how long to wait before retrying when resp reports a
// rate limit, and whether it reported one at all.
func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}

	// Secondary (abuse) rate limit: honour Retry-After (seconds).
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
	}

	// Primary rate limit: exhausted quota plus a reset epoch.
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			if epoch, err := strconv.ParseInt(reset, 10, 64); err == nil {
				delay := time.Until(time.Unix(epoch, 0))
				if delay < 0 {
					delay = 0
				}
				return delay, true
			}
		}
	}

	return 0, false
}

func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
