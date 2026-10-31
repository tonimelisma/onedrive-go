package retry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	retryExhaustedLogMessage        = "request failed after all retries"
	networkRetryExhaustedLogMessage = "OneDrive still unreachable, giving up"
)

// testPolicy is a fast retry policy for tests: 4 total attempts (the first
// plus 3 retries), tiny delays, no jitter.
func testPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts: 4,
		Base:        1 * time.Millisecond,
		Max:         10 * time.Millisecond,
		Multiplier:  2.0,
		Jitter:      0.0,
	}
}

// noopSleep returns immediately, eliminating real delays in tests.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
}

func closeTestResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return
	}

	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, int32(3), attempts.Load(), "should have retried twice then succeeded")
	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	closeTestResponse(t, resp)
	require.Error(t, err)
	assert.Nil(t, resp)
	// MaxAttempts=4 counts every attempt: initial + 3 retries.
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
		Policy: testPolicy(),
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
	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 400, resp.StatusCode, "non-retryable status should be returned as-is")
	assert.Equal(t, int32(1), attempts.Load(), "should not retry 400")
	require.NoError(t, resp.Body.Close())
}

// Validates: R-6.8.8
func TestRetryTransport_UnsafeMethodsDoNotRetryGenericTransientStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
	}{
		{name: "Post", method: http.MethodPost},
		{name: "Patch", method: http.MethodPatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				attempts.Add(1)

				return makeResponse(http.StatusServiceUnavailable, nil), nil
			})
			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy(),
				Logger: slog.Default(),
				Sleep:  noopSleep,
			}

			req, err := http.NewRequestWithContext(t.Context(), tt.method, "http://example.com/mutate", http.NoBody)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, int32(1), attempts.Load())
			require.NoError(t, resp.Body.Close())
		})
	}
}

// Validates: R-6.8.8
func TestRetryTransport_UnsafeMethodsDoNotRetryGenericNetworkError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts.Add(1)

		return nil, io.ErrUnexpectedEOF
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.com/mutate", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	closeTestResponse(t, resp)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.Equal(t, int32(1), attempts.Load())
}

// Validates: R-6.8.21
func TestRetryTransport_ReplaySafePostIsRetriedWithItsBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		respond func() (*http.Response, error)
	}{
		{name: "transient status", respond: func() (*http.Response, error) {
			return makeResponse(http.StatusServiceUnavailable, nil), nil
		}},
		{name: "network error", respond: func() (*http.Response, error) {
			return nil, io.ErrUnexpectedEOF
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			var bodies []string
			inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				bodies = append(bodies, string(body))
				if attempts.Add(1) == 1 {
					return tt.respond()
				}

				return makeResponse(http.StatusCreated, nil), nil
			})
			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy(),
				Logger: slog.Default(),
				Sleep:  noopSleep,
			}

			req, err := http.NewRequestWithContext(
				retry.WithReplaySafe(t.Context()), http.MethodPost, "http://example.com/children", strings.NewReader(`{"name":"a"}`),
			)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusCreated, resp.StatusCode)
			assert.Equal(t, int32(2), attempts.Load())
			assert.Equal(t, []string{`{"name":"a"}`, `{"name":"a"}`}, bodies)
			require.NoError(t, resp.Body.Close())
		})
	}
}

// Validates: R-6.8.8
func TestRetryTransport_BodyRewind(t *testing.T) {
	t.Parallel()

	var bodies []string
	var attempts atomic.Int32

	inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		bodies = append(bodies, string(body))

		if n == 1 {
			return makeResponse(503, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
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
	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
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
	require.NoError(t, resp.Body.Close())
}

// Validates: R-6.8.8
func TestRetryTransport_SharedThrottleGateAcrossTransports(t *testing.T) {
	t.Parallel()

	sharedGate := &retry.ThrottleGate{}
	sharedGate.SetDeadline(time.Now().Add(2 * time.Second))

	var sleepCalls []time.Duration
	newTransport := func() *retry.RetryTransport {
		return &retry.RetryTransport{
			Inner:        roundTripFunc(func(_ *http.Request) (*http.Response, error) { return makeResponse(200, nil), nil }),
			Policy:       testPolicy(),
			Logger:       slog.Default(),
			ThrottleGate: sharedGate,
			Sleep: func(_ context.Context, d time.Duration) error {
				sleepCalls = append(sleepCalls, d)

				return nil
			},
		}
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := newTransport().RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, sleepCalls, 1)
	assert.True(t, sleepCalls[0] > 0 && sleepCalls[0] <= 2*time.Second,
		"shared throttle gate should delay a second transport, got %v", sleepCalls[0])
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 500, resp.StatusCode, "exhausted retries should return last response")
	// MaxAttempts=4 counts every attempt: initial + 3 retries.
	assert.Equal(t, int32(4), attempts.Load())
	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel() // cancel during sleep

			return ctx.Err()
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	closeTestResponse(t, resp)
	assert.Error(t, err)
}

// Validates: R-6.8.8
func TestRetryTransport_DeadlineExceededWithoutCanceledContextRetries(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	rt := &retry.RetryTransport{
		Inner: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, context.DeadlineExceeded
			}

			return makeResponse(200, nil), nil
		}),
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, int32(2), attempts.Load(), "deadline exceeded without canceled request context should retry")
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

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
		Policy: testPolicy(),
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
	require.NoError(t, resp.Body.Close())
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	assert.Equal(t, 401, resp.StatusCode)
	assert.Equal(t, int32(1), attempts.Load(), "401 should not be retried at transport level")
	require.NoError(t, resp.Body.Close())
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
		b, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		bodies = append(bodies, string(b))

		if callNum == 1 {
			return makeResponse(500, nil), nil
		}

		return makeResponse(200, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
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
	require.NoError(t, resp.Body.Close())
}

// Validates: R-6.8.8
func TestRetryTransport_RetryableStatusCodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "503Retry", statusCode: http.StatusServiceUnavailable},
		{name: "509BandwidthRetry", statusCode: 509},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			inner := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					return makeResponse(tt.statusCode, nil), nil
				}

				return makeResponse(200, nil), nil
			})

			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy(),
				Logger: slog.Default(),
				Sleep:  noopSleep,
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			assert.Equal(t, 200, resp.StatusCode)
			assert.Equal(t, int32(2), attempts.Load())
			closeTestResponse(t, resp)
		})
	}
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
				Policy: testPolicy(),
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
			require.NoError(t, resp.Body.Close())

			if tc.expectDelay > 0 {
				require.Len(t, sleepDurations, 1)
				assert.Equal(t, tc.expectDelay, sleepDurations[0])
			} else {
				// Falls back to policy-computed delay.
				require.Len(t, sleepDurations, 1)
				assert.Positive(t, sleepDurations[0], "should use policy delay when Retry-After invalid")
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
		Policy: testPolicy(),
		Logger: slog.Default(),
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com/test", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, retryHeaders, 3)
	assert.Empty(t, retryHeaders[0], "first attempt has no retry count header")
	assert.Equal(t, "1", retryHeaders[1])
	assert.Equal(t, "2", retryHeaders[2])
}

// Validates: R-6.6.8
//
// The exhausted network error is returned and the command prints it once, so
// the transport announces the outage while it is still retrying and logs the
// final give-up at Debug only.
func TestRetryTransport_NetworkError_ExhaustionAnnouncesOnceAndDoesNotRepeatTheError(t *testing.T) {
	t.Parallel()

	// Use a log handler that captures log records to verify the WARN log.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	var attempts atomic.Int32
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, assert.AnError
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: logger,
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	closeTestResponse(t, resp)
	require.Error(t, err)

	// All retries exhausted (1 initial + 3 retries = 4 attempts).
	assert.Equal(t, int32(4), attempts.Load())

	warns := warnMessages(t, logBuf.Bytes())
	assert.Equal(t, []string{"can't reach OneDrive, retrying"}, warns,
		"a sustained outage is announced once, while it is still retrying, and the give-up is not repeated at Warn")
	assert.NotContains(t, logBuf.String(), networkRetryExhaustedLogMessage,
		"the give-up record is Debug, below this logger's level")
}

// Validates: R-6.6.8
//
// The exhausted response is returned and becomes the caller's error, so the
// transport does not also warn about it.
func TestRetryTransport_HTTPError_ExhaustionDoesNotWarn(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	var attempts atomic.Int32
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: logger,
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	// All retries exhausted (1 initial + 3 retries = 4 attempts).
	assert.Equal(t, int32(4), attempts.Load())

	assert.Empty(t, warnMessages(t, logBuf.Bytes()))
	assert.NotContains(t, logBuf.String(), retryExhaustedLogMessage,
		"the exhausted-retries record is Debug, below this logger's level")
}

// Validates: R-6.6.8
func TestRetryTransport_AttemptsLogAtDebug(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var attempts atomic.Int32
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n == 1 {
			return nil, assert.AnError
		}

		return makeResponse(http.StatusOK, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: logger,
		Sleep:  noopSleep,
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Contains(t, logBuf.String(), `"level":"DEBUG"`)
	assert.NotContains(t, logBuf.String(), `"level":"WARN"`)
}

// Validates: R-6.6.8
func TestRetryTransport_UsesRedactedLogTargetFromContext(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	opaqueSessionURL := "https://uploads.contoso.sharepoint.com/session/opaque-upload-id"
	var attempts atomic.Int32
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		if n == 1 {
			return nil, assert.AnError
		}

		return makeResponse(http.StatusOK, nil), nil
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: logger,
		Sleep:  noopSleep,
	}

	ctx := retry.WithLogTarget(t.Context(), "preauth:upload chunk")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, opaqueSessionURL, http.NoBody)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, int32(2), attempts.Load())
	assert.NotContains(t, logBuf.String(), opaqueSessionURL)
	assert.Contains(t, logBuf.String(), "preauth:upload chunk")
	assert.NotContains(t, logBuf.String(), `"url":"`+opaqueSessionURL+`"`)
}

// warnMessages returns the msg of every WARN-or-higher record in JSON log output.
func warnMessages(t *testing.T, logOutput []byte) []string {
	t.Helper()

	var messages []string
	for _, line := range bytes.Split(bytes.TrimSpace(logOutput), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		require.NoError(t, json.Unmarshal(line, &record))
		if record.Level == slog.LevelWarn.String() || record.Level == slog.LevelError.String() {
			messages = append(messages, record.Msg)
		}
	}

	return messages
}

// Validates: R-6.6.8
//
// Concurrent workers in one outage all fail at once. Each used to print its
// own "can't reach OneDrive" warning; the shared notice makes it one line per
// outage, even across separate transports.
func TestRetryTransport_OutageIsAnnouncedOncePerProcessAcrossConcurrentRequests(t *testing.T) {
	t.Parallel()

	var logBuf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, io.ErrUnexpectedEOF
	})

	notices := &retry.Notices{}
	newTransport := func() *retry.RetryTransport {
		return &retry.RetryTransport{
			Inner:   inner,
			Policy:  testPolicy(),
			Logger:  logger,
			Sleep:   noopSleep,
			Notices: notices,
		}
	}
	transports := []*retry.RetryTransport{newTransport(), newTransport()}

	const workers = 8

	var (
		ready sync.WaitGroup
		done  sync.WaitGroup
	)
	start := make(chan struct{})

	for i := range workers {
		ready.Add(1)
		done.Add(1)

		go func(rt *retry.RetryTransport) {
			defer done.Done()

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
			assert.NoError(t, err)

			ready.Done()
			<-start

			resp, rtErr := rt.RoundTrip(req)
			if resp != nil {
				assert.NoError(t, resp.Body.Close())
			}
			assert.Error(t, rtErr)
		}(transports[i%len(transports)])
	}

	ready.Wait()
	close(start)
	done.Wait()

	assert.Equal(t, []string{"can't reach OneDrive, retrying"}, warnMessages(t, logBuf.Bytes()),
		"one outage, one announcement, however many requests hit it")
}

// Validates: R-6.6.8
//
// The notice re-arms once OneDrive answers again, so a later, separate outage
// is announced too.
func TestRetryTransport_OutageNoticeRearmsAfterAResponse(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	var online atomic.Bool
	inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if online.Load() {
			return makeResponse(http.StatusOK, nil), nil
		}

		return nil, io.ErrUnexpectedEOF
	})

	rt := &retry.RetryTransport{
		Inner:  inner,
		Policy: testPolicy(),
		Logger: logger,
		Sleep:  noopSleep,
	}

	roundTrip := func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
		require.NoError(t, err)

		resp, rtErr := rt.RoundTrip(req)
		if rtErr == nil {
			require.NoError(t, resp.Body.Close())
		}
	}

	roundTrip()
	roundTrip()
	assert.Len(t, warnMessages(t, logBuf.Bytes()), 1, "a continuing outage stays announced once")

	online.Store(true)
	roundTrip()
	online.Store(false)
	roundTrip()

	assert.Equal(t, []string{"can't reach OneDrive, retrying", "can't reach OneDrive, retrying"},
		warnMessages(t, logBuf.Bytes()), "a new outage after OneDrive answered is announced again")
}

// Validates: R-6.6.8
//
// A Retry-After long enough for a person to notice the silence is announced
// once per throttling episode; short waits stay quiet.
func TestRetryTransport_LongRateLimitWaitIsAnnouncedOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		retryAfter string
		wantWarns  []string
	}{
		{name: "short wait stays quiet", retryAfter: "2", wantWarns: nil},
		{
			name:       "long wait is announced",
			retryAfter: "120",
			wantWarns:  []string{"OneDrive is limiting requests, waiting before retrying"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

			var attempts atomic.Int32
			inner := roundTripFunc(func(*http.Request) (*http.Response, error) {
				if attempts.Add(1) <= 2 {
					return makeResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": tt.retryAfter}), nil
				}

				return makeResponse(http.StatusOK, nil), nil
			})

			var slept []time.Duration
			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy(),
				Logger: logger,
				Sleep: func(_ context.Context, d time.Duration) error {
					slept = append(slept, d)
					return nil
				},
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			assert.Equal(t, tt.wantWarns, warnMessages(t, logBuf.Bytes()),
				"two throttled attempts in one episode produce at most one line")
			assert.Len(t, slept, 2)
		})
	}
}

// Validates: R-6.6.8
func TestHumanDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   time.Duration
		want string
	}{
		{in: 0, want: "1 s"},
		{in: 400 * time.Millisecond, want: "1 s"},
		{in: 2100 * time.Millisecond, want: "2 s"},
		{in: 59 * time.Second, want: "59 s"},
		{in: 2 * time.Minute, want: "2 min"},
		{in: 90 * time.Second, want: "1 min 30 s"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, retry.HumanDuration(tt.in), tt.in.String())
	}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger
// shared by several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log buffer: %w", err)
	}

	return n, nil
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]byte(nil), b.buf.Bytes()...)
}

// onceReader is a body http.NewRequest cannot replay: no GetBody, no Seek.
type onceReader struct{ io.Reader }

// Validates: R-6.8.19
func TestRetryTransport_NonReplayableBodyIsNotRetried(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		respond func() (*http.Response, error)
	}{
		{name: "RetryableStatus", respond: func() (*http.Response, error) { return makeResponse(503, nil), nil }},
		{name: "NetworkError", respond: func() (*http.Response, error) { return nil, io.ErrUnexpectedEOF }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var bodies []string
			inner := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				bodies = append(bodies, string(body))

				return tt.respond()
			})

			rt := &retry.RetryTransport{
				Inner:  inner,
				Policy: testPolicy(),
				Logger: slog.Default(),
				Sleep:  noopSleep,
			}

			body := &onceReader{Reader: bytes.NewReader([]byte("file content"))}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://example.com/upload", body)
			require.NoError(t, err)
			require.Nil(t, req.GetBody)

			resp, err := rt.RoundTrip(req)
			closeTestResponse(t, resp)

			// One attempt only: a retry would have sent zero bytes as the
			// file's new content.
			assert.Equal(t, []string{"file content"}, bodies)
			if resp != nil {
				assert.Equal(t, 503, resp.StatusCode)
			} else {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			}
		})
	}
}
