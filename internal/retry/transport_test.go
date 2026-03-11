package retry_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// testPolicy is a fast retry policy for tests: 3 attempts, tiny delays, no jitter.
var testPolicy = retry.Policy{
	MaxAttempts: 3,
	Base:        1 * time.Millisecond,
	Max:         10 * time.Millisecond,
	Multiplier:  2.0,
	Jitter:      0.0,
}

// noopSleep returns immediately, eliminating real delays in tests.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
}

// roundTripFunc adapts a function to http.RoundTripper for test injection.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// makeResponse builds an *http.Response with the given status and optional headers.
func makeResponse(status int, headers map[string]string) *http.Response {
	resp := &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
	for k, v := range headers {
		resp.Header.Set(k, v)
	}

	return resp
}

// Validates: R-6.8.8, R-6.8.6
func TestRetryTransport_NetworkRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n < 3 {
			return nil, io.ErrUnexpectedEOF // network error
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(3), attempts.Load(), "should have retried twice then succeeded")
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_NetworkRetryExhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)

		return nil, io.ErrUnexpectedEOF
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	assert.Error(t, err)
	assert.Nil(t, resp)
	// MaxAttempts=3: initial + 3 retries = 4 total attempts
	assert.Equal(t, int32(4), attempts.Load())
}

// Validates: R-6.8.8
func TestRetryTransport_429RetryAfter(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	var sleepDurations []time.Duration
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n == 1 {
			return makeResponse(429, map[string]string{"Retry-After": "5"}), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep: func(_ context.Context, d time.Duration) error {
			sleepDurations = append(sleepDurations, d)

			return nil
		},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load())
	// 429 Retry-After should be honored instead of computed backoff.
	require.Len(t, sleepDurations, 1)
	assert.Equal(t, 5*time.Second, sleepDurations[0])
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_503Retry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n == 1 {
			return makeResponse(503, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load())
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_400NoRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)

		return makeResponse(400, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode, "non-retryable status should be returned as-is")
	assert.Equal(t, int32(1), attempts.Load(), "should not retry 400")
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_BodyRewind(t *testing.T) {
	t.Parallel()

	var bodies []string
	var attempts atomic.Int32

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		body, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(body))

		if n == 1 {
			return makeResponse(503, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	// Use bytes.Reader which implements io.Seeker — RetryTransport should rewind it.
	body := bytes.NewReader([]byte("hello world"))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com/upload", body)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load())
	// Both attempts should have received the full body.
	require.Len(t, bodies, 2)
	assert.Equal(t, "hello world", bodies[0])
	assert.Equal(t, "hello world", bodies[1])
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_ThrottleCoordination(t *testing.T) {
	t.Parallel()

	// First request gets 429 with Retry-After=2.
	// Second request (different goroutine) should wait for the throttle deadline.
	var sleepCalls []time.Duration

	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep: func(_ context.Context, d time.Duration) error {
			sleepCalls = append(sleepCalls, d)

			return nil
		},
	}

	// Simulate setting a throttle deadline.
	rt.SetThrottleDeadline(time.Now().Add(2 * time.Second))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	// Should have waited for throttle before making the request.
	require.Len(t, sleepCalls, 1)
	assert.True(t, sleepCalls[0] > 0 && sleepCalls[0] <= 2*time.Second,
		"throttle wait should be between 0 and 2s, got %v", sleepCalls[0])
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_MaxAttemptsExhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)

		return makeResponse(500, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode, "exhausted retries should return last response")
	// MaxAttempts=3: initial + 3 retries = 4 total attempts
	assert.Equal(t, int32(4), attempts.Load())
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return makeResponse(503, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel() // cancel during sleep

			return ctx.Err()
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	_, err = rt.RoundTrip(req)
	assert.Error(t, err)
}

// Validates: R-6.8.8
func TestRetryTransport_429SetsThrottleDeadline(t *testing.T) {
	t.Parallel()

	callNum := 0
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		callNum++
		if callNum == 1 {
			return makeResponse(429, map[string]string{"Retry-After": "10"}), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	// The throttle deadline should be set after seeing 429 with Retry-After.
	deadline := rt.ThrottleDeadline()
	assert.False(t, deadline.IsZero(), "throttle deadline should be set after 429")
}

// Validates: R-6.8.8
func TestRetryTransport_RetryAfterHeader_503(t *testing.T) {
	t.Parallel()

	var sleepDurations []time.Duration
	callNum := 0

	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		callNum++
		if callNum == 1 {
			return makeResponse(503, map[string]string{"Retry-After": "3"}), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep: func(_ context.Context, d time.Duration) error {
			sleepDurations = append(sleepDurations, d)

			return nil
		},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	require.Len(t, sleepDurations, 1)
	assert.Equal(t, 3*time.Second, sleepDurations[0], "503 Retry-After should be honored")
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_NoRetryOn401(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)

		return makeResponse(401, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 401, resp.StatusCode)
	assert.Equal(t, int32(1), attempts.Load(), "401 should not be retried at transport level")
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_SectionReaderBody(t *testing.T) {
	t.Parallel()

	// io.SectionReader implements io.Seeker (Go 1.17+). Verify body rewind works
	// with the type used by upload chunk requests.
	data := []byte("chunk data for upload")
	var bodies []string
	callNum := 0

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		callNum++
		b, _ := io.ReadAll(req.Body)
		bodies = append(bodies, string(b))

		if callNum == 1 {
			return makeResponse(500, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	baseReader := bytes.NewReader(data)
	reader := io.NewSectionReader(baseReader, 0, int64(len(data)))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com/upload", reader)
	require.NoError(t, err)

	// http.NewRequest doesn't set GetBody for io.SectionReader, so callers
	// must set it manually for retry support (same as production upload code).
	req.GetBody = func() (io.ReadCloser, error) {
		sr := io.NewSectionReader(baseReader, 0, int64(len(data)))

		return io.NopCloser(sr), nil
	}

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	require.Len(t, bodies, 2)
	assert.Equal(t, "chunk data for upload", bodies[0])
	assert.Equal(t, "chunk data for upload", bodies[1])
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_509BandwidthRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n == 1 {
			return makeResponse(509, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load())
	resp.Body.Close()
}

// Validates: R-6.8.8
func TestRetryTransport_RetryAfterParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		header      string
		expectDelay time.Duration
	}{
		{name: "valid integer", header: "5", expectDelay: 5 * time.Second},
		{name: "zero", header: "0", expectDelay: 0},
		{name: "negative", header: "-1", expectDelay: 0},
		{name: "non-numeric", header: "abc", expectDelay: 0},
		{name: "empty", header: "", expectDelay: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var sleepDurations []time.Duration
			callNum := 0

			inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				callNum++
				if callNum == 1 {
					hdrs := map[string]string{}
					if tc.header != "" {
						hdrs["Retry-After"] = tc.header
					}

					return makeResponse(429, hdrs), nil
				}

				return makeResponse(200, nil), nil
			})

			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy,
				Logger: slog.Default(),
				Sleep: func(_ context.Context, d time.Duration) error {
					sleepDurations = append(sleepDurations, d)

					return nil
				},
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			resp.Body.Close()

			if tc.expectDelay > 0 {
				require.Len(t, sleepDurations, 1)
				assert.Equal(t, tc.expectDelay, sleepDurations[0])
			} else {
				// Falls back to policy-computed delay.
				require.Len(t, sleepDurations, 1)
				assert.True(t, sleepDurations[0] > 0, "should use policy delay when Retry-After invalid")
			}
		})
	}
}

// Validates: R-6.8.8
func TestRetryTransport_StripeRetryCount(t *testing.T) {
	t.Parallel()

	// Verify the X-Retry-Count header is set on retried requests so servers
	// can distinguish retries from fresh requests.
	var retryHeaders []string
	callNum := 0

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		callNum++
		retryHeaders = append(retryHeaders, req.Header.Get("X-Retry-Count"))

		if callNum <= 2 {
			return makeResponse(500, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy,
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	resp.Body.Close()

	require.Len(t, retryHeaders, 3)
	assert.Equal(t, "", retryHeaders[0], "first attempt has no retry count header")
	assert.Equal(t, "1", retryHeaders[1])
	assert.Equal(t, "2", retryHeaders[2])
}
