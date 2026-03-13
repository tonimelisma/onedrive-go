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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/retry"
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

// testRetryPolicy is a fast retry policy for tests.
var testRetryPolicy = retry.Policy{
	MaxAttempts: 5,
	Base:        1 * time.Millisecond,
	Max:         10 * time.Millisecond,
	Multiplier:  2.0,
	Jitter:      0.0,
}

// retryHTTPClient wraps an http.Client with a fast RetryTransport for tests.
// This replaces the old approach of injecting sleepFunc on the graph.Client.
func retryHTTPClient(inner *http.Client, policy retry.Policy) *http.Client {
	transport := inner.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	return &http.Client{
		Timeout: inner.Timeout,
		Transport: &retry.RetryTransport{
			Inner:  transport,
			Policy: policy,
			Logger: slog.Default(),
			Sleep:  noopSleep,
		},
	}
}

// newTestClient creates a Client with a RetryTransport for fast tests that need retry behavior.
func newTestClient(t *testing.T, url string) *Client {
	t.Helper()

	return NewClient(url, retryHTTPClient(http.DefaultClient, testRetryPolicy), staticToken("test-token"), slog.Default(), "test-agent")
}

// newNoRetryTestClient creates a Client with no retry — for testing single-request behavior.
func newNoRetryTestClient(t *testing.T, url string) *Client {
	t.Helper()

	return NewClient(url, http.DefaultClient, staticToken("test-token"), slog.Default(), "test-agent")
}

func TestDo_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.Do(t.Context(), http.MethodGet, "/me", nil)
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

			// Use no-retry client — these are non-retryable codes.
			client := newNoRetryTestClient(t, srv.URL)
			_, err := client.Do(t.Context(), http.MethodGet, "/test", nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.sentinel)

			var graphErr *GraphError
			require.ErrorAs(t, err, &graphErr)
			assert.Equal(t, tt.status, graphErr.StatusCode)
			assert.Equal(t, "test-req-id", graphErr.RequestID)
		})
	}
}

// Validates: R-6.8.2
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
	resp, err := client.Do(t.Context(), http.MethodGet, "/retry", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

// Validates: R-6.8.1
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
	resp, err := client.Do(t.Context(), http.MethodGet, "/throttle", nil)
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
	_, err := client.Do(t.Context(), http.MethodGet, "/fail", nil)
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
	_, err := client.Do(t.Context(), http.MethodGet, "/missing", nil)
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

	resp, err := client.Do(t.Context(), http.MethodGet, "/auth", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDo_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	client := newNoRetryTestClient(t, srv.URL)
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
	resp, err := client.Do(t.Context(), http.MethodGet, "/ua", nil)
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
	resp, err := client.Do(t.Context(), http.MethodPost, "/create", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
}

func TestDo_TokenError(t *testing.T) {
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")

	_, err := client.Do(t.Context(), http.MethodGet, "/test", nil)
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
		{http.StatusMethodNotAllowed, ErrMethodNotAllowed},
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

	resp, err := client.DoWithHeaders(t.Context(), http.MethodGet, "/test", nil, headers)
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

	resp, err := client.DoWithHeaders(t.Context(), http.MethodGet, "/test", nil, nil)
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

	resp, err := client.DoWithHeaders(t.Context(), http.MethodGet, "/retry", nil, headers)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDo_RetryWithBody(t *testing.T) {
	// Verify that POST/PATCH bodies are fully readable on retry attempts.
	expectedBody := `{"name":"test-folder","folder":{}}`

	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		assert.Equal(t, expectedBody, string(body))

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
		t.Context(),
		http.MethodPost,
		"/create",
		bytes.NewReader([]byte(expectedBody)),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

// TestIsRetryable was deleted: isRetryable moved to retry/transport.go.
// Coverage is provided by retry/transport_test.go.

func TestRewindBody_SeekError(t *testing.T) {
	// Verify that rewindBody returns an error when Seek fails.
	fs := &failingSeeker{data: []byte("test data")}
	err := rewindBody(fs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rewinding request body for retry")
	assert.Contains(t, err.Error(), "seek failed")
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
	resp, err := client.Do(t.Context(), http.MethodGet, "/throttle", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDo_NetworkError_MaxRetries(t *testing.T) {
	// Point the client at an unreachable address and verify that all retries
	// are exhausted before returning an error.
	client := NewClient("http://127.0.0.1:1", retryHTTPClient(http.DefaultClient, testRetryPolicy), staticToken("tok"), slog.Default(), "test-agent")

	_, err := client.Do(t.Context(), http.MethodGet, "/unreachable", nil)
	require.Error(t, err)
}

// --- doPreAuth tests ---

func TestDoPreAuth_SuccessFirstTry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no Authorization header is sent.
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuth(t.Context(), "test op", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/test", http.NoBody)
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

func TestDoPreAuth_NonRetryable4xx(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("request-id", "test-req-id")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuth(t.Context(), "404 test", func() (*http.Request, error) {
		return http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/missing", http.NoBody)
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "test-req-id", graphErr.RequestID)

	// No retries for non-retryable 4xx.
	assert.Equal(t, int32(1), calls.Load())
}

func TestDoPreAuth_MakeReqError(t *testing.T) {
	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuth(t.Context(), "bad factory", func() (*http.Request, error) {
		return nil, errors.New("factory failed")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory failed")
}

func TestDo_ErrorBodyCappedAt64KiB(t *testing.T) {
	// Verify that error response bodies are capped at maxErrBodySize (64 KiB)
	// to prevent OOM from malicious/buggy servers (B-314).
	bigBody := strings.Repeat("X", 128*1024) // 128 KiB — twice the cap

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	client := newNoRetryTestClient(t, srv.URL)

	_, err := client.Do(t.Context(), http.MethodGet, "/big-error", nil)
	require.Error(t, err)

	var ge *GraphError
	require.ErrorAs(t, err, &ge)
	assert.LessOrEqual(t, len(ge.Message), maxErrBodySize, "error body should be capped at 64 KiB")
	assert.Equal(t, maxErrBodySize, len(ge.Message), "error body should be exactly 64 KiB (truncated)")
}

func TestDoPreAuth_ErrorBodyCappedAt64KiB(t *testing.T) {
	// Same test but for the pre-auth path (B-314).
	bigBody := strings.Repeat("Y", 128*1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	_, err := client.doPreAuth(t.Context(), "big error", func() (*http.Request, error) {
		return http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/big", http.NoBody)
	})
	require.Error(t, err)

	var ge *GraphError
	require.ErrorAs(t, err, &ge)
	assert.LessOrEqual(t, len(ge.Message), maxErrBodySize)
}

// Validates: R-6.8.8
func TestDo_CustomPolicy_LimitsAttempts(t *testing.T) {
	t.Parallel()

	t.Run("MaxAttempts=2 retries twice after initial", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"server error"}`))
		}))
		defer srv.Close()

		twoRetryPolicy := retry.Policy{
			MaxAttempts: 2,
			Base:        1 * time.Millisecond,
			Max:         10 * time.Millisecond,
			Multiplier:  2.0,
			Jitter:      0.0,
		}

		client := NewClient(srv.URL, retryHTTPClient(http.DefaultClient, twoRetryPolicy), staticToken("tok"), slog.Default(), "test-agent")

		_, err := client.Do(t.Context(), http.MethodGet, "/fail", nil)
		require.Error(t, err)
		// MaxAttempts=2 means 2 retries after the initial attempt = 3 total server hits.
		assert.Equal(t, int32(3), attempts.Load(), "MaxAttempts=2 should make 1 initial + 2 retries = 3 total")
	})

	t.Run("NoRetryClient makes exactly 1 attempt", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"server error"}`))
		}))
		defer srv.Close()

		client := newNoRetryTestClient(t, srv.URL)

		_, err := client.Do(t.Context(), http.MethodGet, "/fail", nil)
		require.Error(t, err)
		assert.Equal(t, int32(1), attempts.Load(), "no-retry client should make exactly 1 attempt")
	})
}

// Validates: R-6.8.6
func TestTerminalError_RetryAfter(t *testing.T) {
	t.Parallel()

	t.Run("429 with Retry-After populates RetryAfter", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"throttled"}`))
		}))
		defer srv.Close()

		// No retry client — single attempt, hits terminal immediately.
		client := newNoRetryTestClient(t, srv.URL)

		_, err := client.Do(t.Context(), http.MethodGet, "/throttle", nil)
		require.Error(t, err)

		var graphErr *GraphError
		require.ErrorAs(t, err, &graphErr)
		assert.Equal(t, 42*time.Second, graphErr.RetryAfter)
	})

	t.Run("503 with Retry-After populates RetryAfter", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
		}))
		defer srv.Close()

		client := newNoRetryTestClient(t, srv.URL)

		_, err := client.Do(t.Context(), http.MethodGet, "/unavail", nil)
		require.Error(t, err)

		var graphErr *GraphError
		require.ErrorAs(t, err, &graphErr)
		assert.Equal(t, 10*time.Second, graphErr.RetryAfter)
	})

	t.Run("500 without Retry-After has zero RetryAfter", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"server error"}`))
		}))
		defer srv.Close()

		client := newNoRetryTestClient(t, srv.URL)

		_, err := client.Do(t.Context(), http.MethodGet, "/fail", nil)
		require.Error(t, err)

		var graphErr *GraphError
		require.ErrorAs(t, err, &graphErr)
		assert.Equal(t, time.Duration(0), graphErr.RetryAfter)
	})
}

// switchingToken returns different tokens on successive calls, simulating a
// token refresh. First call returns oldTok, subsequent calls return newTok.
type switchingToken struct {
	oldTok string
	newTok string
	calls  atomic.Int32
}

func (s *switchingToken) Token() (string, error) {
	n := s.calls.Add(1)
	if n == 1 {
		return s.oldTok, nil
	}

	return s.newTok, nil
}

// Validates: R-6.8.14
func TestDoOnce_401_RefreshSucceeds(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		auth := r.Header.Get("Authorization")

		if auth == "Bearer refreshed-token" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"value":"ok"}`))

			return
		}

		// Old token → 401.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"expired"}`))
	}))
	defer srv.Close()

	ts := &switchingToken{oldTok: "expired-token", newTok: "refreshed-token"}
	client := NewClient(srv.URL, http.DefaultClient, ts, slog.Default(), "test-agent")

	resp, err := client.Do(t.Context(), http.MethodGet, "/me", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// 2 server hits: 1st with old token (401), 2nd with refreshed token (200).
	assert.Equal(t, int32(2), attempts.Load())
}

// Validates: R-6.8.14
func TestDoOnce_401_SameToken_Returns401(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid"}`))
	}))
	defer srv.Close()

	// Static token — second Token() call returns the same value, so no refresh.
	client := NewClient(srv.URL, http.DefaultClient, staticToken("same-token"), slog.Default(), "test-agent")

	_, err := client.Do(t.Context(), http.MethodGet, "/me", nil)
	require.Error(t, err)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, http.StatusUnauthorized, graphErr.StatusCode)
}
