package graph

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopSleep is a sleep function that returns immediately, for fast tests.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
}

// failingSeeker is an io.ReadSeeker where Read succeeds but Seek always fails.
// Used to test the rewindBody error path directly.
type failingSeeker struct {
	data []byte
}

func (f *failingSeeker) Read(p []byte) (int, error) {
	return copy(p, f.data), io.EOF
}

func (f *failingSeeker) Seek(_ int64, _ int) (int64, error) {
	return 0, errors.New("seek failed")
}

// failOnSecondSeeker is an io.ReadSeeker where the first Seek succeeds but
// subsequent Seeks fail. Used to test the rewindBody failure on retry in doRetry.
type failOnSecondSeeker struct {
	data      []byte
	seekCount atomic.Int32
}

func (f *failOnSecondSeeker) Read(p []byte) (int, error) {
	return copy(p, f.data), io.EOF
}

func (f *failOnSecondSeeker) Seek(_ int64, _ int) (int64, error) {
	n := f.seekCount.Add(1)
	if n > 1 {
		return 0, errors.New("seek failed on retry")
	}

	return 0, nil
}

// staticToken is a test TokenSource that returns a fixed token.
type staticToken string

func (t staticToken) Token() (string, error) {
	return string(t), nil
}

// failingToken is a test TokenSource that always returns an error.
type failingToken struct{}

func (failingToken) Token() (string, error) {
	return "", errors.New("token error")
}

// newTestClient creates a Client pointing at the given httptest server
// with instant retry sleeps for fast tests.
func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	c := NewClient(url, http.DefaultClient, staticToken("test-token"), slog.Default(), "test-agent")
	c.sleepFunc = noopSleep

	return c
}

func TestDo_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodGet, "/me", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, `{"value":"ok"}`, string(body))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDo_ErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		sentinel error
	}{
		{"bad request", http.StatusBadRequest, ErrBadRequest},
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, ErrForbidden},
		{"not found", http.StatusNotFound, ErrNotFound},
		{"conflict", http.StatusConflict, ErrConflict},
		{"gone", http.StatusGone, ErrGone},
		{"locked", http.StatusLocked, ErrLocked},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("request-id", "test-req-id")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":"something"}`))
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			_, err := client.Do(context.Background(), http.MethodGet, "/test", nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.sentinel)

			var graphErr *GraphError
			require.ErrorAs(t, err, &graphErr)
			assert.Equal(t, tt.status, graphErr.StatusCode)
			assert.Equal(t, "test-req-id", graphErr.RequestID)
		})
	}
}

func TestDo_RetryOn5xx(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodGet, "/retry", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

func TestDo_RetryOn429WithRetryAfter(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodGet, "/throttle", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDo_MaxRetriesExhausted(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Do(context.Background(), http.MethodGet, "/fail", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrServerError)

	// 1 initial + 5 retries = 6 total attempts.
	assert.Equal(t, int32(6), calls.Load())
}

func TestDo_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Do(context.Background(), http.MethodGet, "/missing", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	// No retries for non-retryable 4xx.
	assert.Equal(t, int32(1), calls.Load())
}

func TestDo_AuthorizationHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, http.DefaultClient, staticToken("my-secret-token"), slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	resp, err := client.Do(context.Background(), http.MethodGet, "/auth", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDo_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestClient(t, srv.URL)
	_, err := client.Do(ctx, http.MethodGet, "/cancel", nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGraphError_ErrorsIs(t *testing.T) {
	graphErr := &GraphError{
		StatusCode: http.StatusNotFound,
		RequestID:  "abc-123",
		Message:    "item not found",
		Err:        ErrNotFound,
	}

	assert.ErrorIs(t, graphErr, ErrNotFound)
	assert.True(t, !errors.Is(graphErr, ErrConflict))
}

func TestGraphError_Unwrap(t *testing.T) {
	graphErr := &GraphError{
		StatusCode: http.StatusForbidden,
		Message:    "access denied",
		Err:        ErrForbidden,
	}

	unwrapped := errors.Unwrap(graphErr)
	assert.Equal(t, ErrForbidden, unwrapped)
}

func TestDo_UserAgentHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodGet, "/ua", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
}

func TestDo_ContentTypeForBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodPost, "/create", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
}

func TestDo_TokenError(t *testing.T) {
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	_, err := client.Do(context.Background(), http.MethodGet, "/test", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token error")
}

func TestGraphError_ErrorString(t *testing.T) {
	t.Run("with request ID", func(t *testing.T) {
		graphErr := &GraphError{
			StatusCode: http.StatusNotFound,
			RequestID:  "req-123",
			Message:    "not found",
			Err:        ErrNotFound,
		}
		assert.Contains(t, graphErr.Error(), "404")
		assert.Contains(t, graphErr.Error(), "req-123")
	})

	t.Run("without request ID", func(t *testing.T) {
		graphErr := &GraphError{
			StatusCode: http.StatusNotFound,
			Message:    "not found",
			Err:        ErrNotFound,
		}
		assert.Contains(t, graphErr.Error(), "404")
		assert.NotContains(t, graphErr.Error(), "request-id")
	})
}

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		code     int
		expected error
	}{
		{http.StatusOK, nil},
		{http.StatusCreated, nil},
		{http.StatusNoContent, nil},
		{http.StatusBadRequest, ErrBadRequest},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrConflict},
		{http.StatusGone, ErrGone},
		{http.StatusTooManyRequests, ErrThrottled},
		{http.StatusLocked, ErrLocked},
		{http.StatusInternalServerError, ErrServerError},
		{http.StatusBadGateway, ErrServerError},
		{http.StatusServiceUnavailable, ErrServerError},
		{http.StatusGatewayTimeout, ErrServerError},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.code), func(t *testing.T) {
			assert.Equal(t, tt.expected, classifyStatus(tt.code))
		})
	}
}

func TestNewClient_Defaults(t *testing.T) {
	// Nil logger and httpClient should use defaults, not panic.
	c := NewClient("http://localhost", nil, staticToken("tok"), nil, "")
	assert.NotNil(t, c.httpClient)
	assert.NotNil(t, c.logger)
}

func TestNewClient_NilTokenSourcePanics(t *testing.T) {
	assert.Panics(t, func() {
		NewClient("http://localhost", nil, nil, nil, "")
	})
}

func TestTimeSleep_Completes(t *testing.T) {
	err := timeSleep(context.Background(), 10*time.Millisecond)
	assert.NoError(t, err)
}

func TestTimeSleep_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := timeSleep(ctx, time.Minute)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestCalcBackoff_MaxCap(t *testing.T) {
	c := NewClient("http://localhost", nil, staticToken("tok"), nil, "")

	// Attempt 10 produces 1s * 2^10 = 1024s which exceeds maxBackoff (60s).
	// Verify the result is capped near maxBackoff (±jitter).
	backoff := c.calcBackoff(10)
	assert.LessOrEqual(t, backoff, maxBackoff+maxBackoff/4)
	assert.GreaterOrEqual(t, backoff, maxBackoff-maxBackoff/4)
}

func TestDoWithHeaders_SendsExtraHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "deltashowremoteitemsaliasid", r.Header.Get("Prefer"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	headers := http.Header{"Prefer": {"deltashowremoteitemsaliasid"}}

	resp, err := client.DoWithHeaders(context.Background(), http.MethodGet, "/test", nil, headers)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDoWithHeaders_NilHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	resp, err := client.DoWithHeaders(context.Background(), http.MethodGet, "/test", nil, nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDoWithHeaders_RetriesWithHeaders(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the Prefer header is present on every attempt (including retries).
		assert.Equal(t, "deltashowremoteitemsaliasid", r.Header.Get("Prefer"))

		n := calls.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	headers := http.Header{"Prefer": {"deltashowremoteitemsaliasid"}}

	resp, err := client.DoWithHeaders(context.Background(), http.MethodGet, "/retry", nil, headers)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDo_RetryWithBody(t *testing.T) {
	// Verify that POST/PATCH bodies are fully readable on retry attempts.
	// Before the fix, the body io.Reader was consumed on the first attempt
	// and subsequent retries sent empty bodies.
	expectedBody := `{"name":"test-folder","folder":{}}`

	var calls atomic.Int32

	var capturedBodies []string

	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)

		mu.Lock()
		capturedBodies = append(capturedBodies, string(body))
		mu.Unlock()

		n := calls.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"created"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(
		context.Background(),
		http.MethodPost,
		"/create",
		bytes.NewReader([]byte(expectedBody)),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())

	// Both attempts must have received the full body.
	mu.Lock()
	defer mu.Unlock()

	require.Len(t, capturedBodies, 2)
	assert.Equal(t, expectedBody, capturedBodies[0], "first attempt body")
	assert.Equal(t, expectedBody, capturedBodies[1], "retry attempt body")
}

func TestIsRetryable(t *testing.T) {
	retryable := []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		509, // Bandwidth Limit Exceeded
	}

	for _, code := range retryable {
		assert.True(t, isRetryable(code), "expected %d to be retryable", code)
	}

	notRetryable := []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
	}

	for _, code := range notRetryable {
		assert.False(t, isRetryable(code), "expected %d to not be retryable", code)
	}
}

func TestRewindBody_SeekError(t *testing.T) {
	// Verify that rewindBody returns an error when Seek fails.
	fs := &failingSeeker{data: []byte("test data")}
	err := rewindBody(fs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rewinding request body for retry")
	assert.Contains(t, err.Error(), "seek failed")
}

func TestDoRetry_RewindBodyFailure(t *testing.T) {
	// The first rewind (before attempt 0) succeeds, the HTTP call gets a 500
	// (retryable), then the second rewind (before the retry) fails.
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)

	body := &failOnSecondSeeker{data: []byte(`{"key":"value"}`)}

	_, err := client.Do(context.Background(), http.MethodPost, "/test", body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rewinding request body for retry")

	// Only one HTTP call should have been made — the rewind failure prevents retry.
	assert.Equal(t, int32(1), calls.Load())
}

func TestRetryBackoff_MalformedRetryAfter(t *testing.T) {
	// Verify that a non-numeric Retry-After header falls back to exponential backoff
	// instead of crashing or using a zero duration.
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 1 {
			w.Header().Set("Retry-After", "not-a-number")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(context.Background(), http.MethodGet, "/throttle", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDoRetry_NetworkError_MaxRetries(t *testing.T) {
	// Point the client at an unreachable address and verify that all retries
	// are exhausted before returning an error.
	client := NewClient("http://127.0.0.1:1", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	_, err := client.Do(context.Background(), http.MethodGet, "/unreachable", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 5 retries")
}

// --- doPreAuthRetry tests ---

func TestDoPreAuthRetry_SuccessFirstTry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no Authorization header is sent.
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "test op", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/test", http.NoBody)
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("User-Agent", "test-agent")

		return req, nil
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDoPreAuthRetry_NetworkRetry(t *testing.T) {
	// Verify that network errors trigger retries. Use a factory that switches
	// from an unreachable address to a working server after the first attempt.
	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "net retry", func() (*http.Request, error) {
		n := attempts.Add(1)

		target := "http://127.0.0.1:1/unreachable"
		if n > 1 {
			target = srv.URL + "/ok"
		}

		return http.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), attempts.Load(), "should succeed on second attempt")
}

func TestDoPreAuthRetry_503Retry(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "503 retry", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/test", http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

func TestDoPreAuthRetry_429WithRetryAfter(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		if n <= 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuthRetry(context.Background(), "429 retry", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/test", http.NoBody)
	})
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDoPreAuthRetry_MaxRetriesExhausted(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuthRetry(context.Background(), "exhaust", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/fail", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrServerError)

	// 1 initial + 5 retries = 6 total attempts.
	assert.Equal(t, int32(6), calls.Load())
}

func TestDoPreAuthRetry_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuthRetry(ctx, "cancel test", func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/test", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDoPreAuthRetry_NonRetryable4xx(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("request-id", "test-req-id")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuthRetry(context.Background(), "404 test", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/missing", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "test-req-id", graphErr.RequestID)

	// No retries for non-retryable 4xx.
	assert.Equal(t, int32(1), calls.Load())
}

func TestDoPreAuthRetry_MakeReqError(t *testing.T) {
	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuthRetry(context.Background(), "bad factory", func() (*http.Request, error) {
		return nil, errors.New("factory failed")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory failed")
}

func TestDoPreAuthRetry_NetworkMaxRetries(t *testing.T) {
	client := NewClient("http://localhost", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")
	client.sleepFunc = noopSleep

	_, err := client.doPreAuthRetry(context.Background(), "net exhaust", func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/unreachable", http.NoBody)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 5 retries")
}

func TestDoPreAuthRetry_ContextCancelDuringHTTPBackoff(t *testing.T) {
	// Verify that context cancellation during the backoff sleep after a retryable
	// HTTP error (503) is detected and returned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := newTestClient(t, "http://unused")
	// Override sleepFunc to cancel context on first backoff.
	client.sleepFunc = func(_ context.Context, _ time.Duration) error {
		cancel()

		return context.Canceled
	}

	_, err := client.doPreAuthRetry(ctx, "cancel during backoff", func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/fail", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestDoPreAuthRetry_ContextCancelDuringNetworkBackoff(t *testing.T) {
	// Verify that context cancellation during the backoff sleep after a network
	// error is detected and returned.
	ctx, cancel := context.WithCancel(context.Background())

	client := NewClient("http://localhost", http.DefaultClient, staticToken("tok"), slog.Default(), "test-agent")
	client.sleepFunc = func(_ context.Context, _ time.Duration) error {
		cancel()

		return context.Canceled
	}

	_, err := client.doPreAuthRetry(ctx, "cancel during net backoff", func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/unreachable", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}
