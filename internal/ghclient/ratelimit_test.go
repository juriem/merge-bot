package ghclient

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, headers map[string]string) *http.Response {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}

	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func Test_rateLimitWait(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(42*time.Second).Unix(), 10)

	cases := []struct {
		name    string
		resp    *http.Response
		limited bool
	}{
		{"ok", resp(http.StatusOK, nil), false},
		{"retry-after", resp(http.StatusForbidden, map[string]string{"Retry-After": "5"}), true},
		{"too-many", resp(http.StatusTooManyRequests, map[string]string{"Retry-After": "1"}), true},
		{"primary reset", resp(http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}), true},
		{"403 without limit headers", resp(http.StatusForbidden, nil), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, limited := rateLimitWait(c.resp)
			if limited != c.limited {
				t.Fatalf("limited = %v, want %v", limited, c.limited)
			}
		})
	}
}

func Test_rateLimitTransport_WaitsThenRetries(t *testing.T) {
	// Arrange: first call is rate-limited, second succeeds.
	var calls int
	var waited []time.Duration
	tr := &rateLimitTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return resp(http.StatusForbidden, map[string]string{"Retry-After": "7"}), nil
			}
			return resp(http.StatusOK, nil), nil
		}),
		maxWait: time.Hour,
		retries: 3,
		wait: func(_ context.Context, d time.Duration) error {
			waited = append(waited, d)
			return nil
		},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)

	// Act
	got, err := tr.RoundTrip(req)

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("base called %d times, want 2", calls)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Fatalf("waits = %v, want [7s]", waited)
	}
}

func Test_rateLimitTransport_RetriesZeroDisablesWaiting(t *testing.T) {
	// Arrange
	var calls int
	waited := false
	tr := &rateLimitTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return resp(http.StatusForbidden, map[string]string{"Retry-After": "1"}), nil
		}),
		maxWait: time.Hour,
		retries: 0,
		wait: func(context.Context, time.Duration) error {
			waited = true
			return nil
		},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)

	// Act
	_, err := tr.RoundTrip(req)

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 || waited {
		t.Fatalf("expected a single call and no wait, calls=%d waited=%v", calls, waited)
	}
}

func Test_rateLimitTransport_DoesNotWaitBeyondMax(t *testing.T) {
	// Arrange: limit asks for longer than maxWait, so no retry/wait happens.
	var calls int
	waited := false
	tr := &rateLimitTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return resp(http.StatusForbidden, map[string]string{"Retry-After": "600"}), nil
		}),
		maxWait: time.Minute,
		retries: 3,
		wait: func(context.Context, time.Duration) error {
			waited = true
			return nil
		},
	}

	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/x", nil)

	// Act
	got, err := tr.RoundTrip(req)

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 surfaced to caller", got.StatusCode)
	}
	if calls != 1 || waited {
		t.Fatalf("expected a single call and no wait, calls=%d waited=%v", calls, waited)
	}
}
