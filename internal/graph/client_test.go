package graph

import (
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
)

// noopSleep is a sleep function that returns immediately, for fast tests.
func noopSleep(_ context.Context, _ time.Duration) error {
	return nil
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

	c := NewClient(url, http.DefaultClient, staticToken("test-token"), slog.Default())
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

	client := NewClient(srv.URL, http.DefaultClient, staticToken("my-secret-token"), slog.Default())
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
		assert.Equal(t, userAgent, r.Header.Get("User-Agent"))
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
	client := NewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default())
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
	c := NewClient("http://localhost", nil, staticToken("tok"), nil)
	assert.NotNil(t, c.httpClient)
	assert.NotNil(t, c.logger)
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
