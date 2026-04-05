package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func closeClientTestResponse(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return
	}

	require.NoError(t, resp.Body.Close())
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
func testRetryPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts: 5,
		Base:        1 * time.Millisecond,
		Max:         10 * time.Millisecond,
		Multiplier:  2.0,
		Jitter:      0.0,
	}
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

	client := MustNewClient(normalizeTestBaseURL(url), retryHTTPClient(http.DefaultClient, testRetryPolicy()), staticToken("test-token"), slog.Default(), "test-agent")
	client.uploadURLValidator = allowTestUploadURL
	client.copyMonitorValidator = allowTestCopyMonitorURL
	client.socketIOValidator = allowTestSocketIONotificationURL

	return client
}

// newNoRetryTestClient creates a Client with no retry — for testing single-request behavior.
func newNoRetryTestClient(t *testing.T, url string) *Client {
	t.Helper()

	client := MustNewClient(normalizeTestBaseURL(url), http.DefaultClient, staticToken("test-token"), slog.Default(), "test-agent")
	client.uploadURLValidator = allowTestUploadURL
	client.copyMonitorValidator = allowTestCopyMonitorURL
	client.socketIOValidator = allowTestSocketIONotificationURL

	return client
}

func normalizeTestBaseURL(raw string) string {
	if raw == "http://unused" {
		return "http://localhost"
	}

	return raw
}

func allowTestUploadURL(parsed *url.URL) error {
	if parsed == nil {
		return fmt.Errorf("graph: upload URL is nil")
	}

	if isLoopbackHostname(parsed.Hostname()) && (parsed.Scheme == deltaHTTPPrefix || parsed.Scheme == httpsScheme) {
		return nil
	}

	return validateUploadURL(parsed)
}

func allowTestCopyMonitorURL(parsed *url.URL) error {
	if parsed == nil {
		return fmt.Errorf("graph: copy monitor URL is nil")
	}

	if isLoopbackHostname(parsed.Hostname()) && (parsed.Scheme == deltaHTTPPrefix || parsed.Scheme == httpsScheme) {
		return nil
	}

	return validateCopyMonitorURL(parsed)
}

func allowTestSocketIONotificationURL(parsed *url.URL) error {
	if parsed == nil {
		return fmt.Errorf("graph: socket.io notification URL is nil")
	}

	if isLoopbackHostname(parsed.Hostname()) && (parsed.Scheme == deltaHTTPPrefix || parsed.Scheme == httpsScheme) {
		return nil
	}

	return validateSocketIONotificationURL(parsed)
}

func writeClientTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

func TestDo_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		writeClientTestBody(t, w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"value":"ok"}`, string(body))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func assertValidatedPreAuthURL(
	t *testing.T,
	name string,
	validate func(*Client, string) (string, error),
	setValidator func(*Client, func(*url.URL) error),
	allowedURL string,
	rejectedURL string,
	errorPrefix string,
) {
	t.Helper()

	client := &Client{}

	validated, err := validate(client, allowedURL)
	require.NoError(t, err)
	assert.Equal(t, allowedURL, validated)

	_, err = validate(client, rejectedURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), errorPrefix)

	custom := &Client{}
	setValidator(custom, func(parsed *url.URL) error {
		require.NotNil(t, parsed)
		assert.Equal(t, "example.com", parsed.Hostname())

		return nil
	})

	validated, err = validate(custom, rejectedURL)
	require.NoError(t, err, name)
	assert.Equal(t, rejectedURL, validated)
}

func TestValidatedUploadURL(t *testing.T) {
	t.Parallel()

	assertValidatedPreAuthURL(
		t,
		"upload",
		func(client *Client, raw string) (string, error) {
			return client.validatedUploadURL(UploadURL(raw))
		},
		func(client *Client, validator func(*url.URL) error) {
			client.uploadURLValidator = validator
		},
		"https://contoso.sharepoint.com/upload",
		"https://example.com/upload",
		"validating upload URL",
	)
}

func TestValidatedCopyMonitorURL(t *testing.T) {
	t.Parallel()

	assertValidatedPreAuthURL(
		t,
		"copy-monitor",
		func(client *Client, raw string) (string, error) {
			return client.validatedCopyMonitorURL(raw)
		},
		func(client *Client, validator func(*url.URL) error) {
			client.copyMonitorValidator = validator
		},
		"https://graph.microsoft.com/v1.0/operations/copy",
		"https://example.com/v1.0/operations/copy",
		"validating copy monitor URL",
	)
}

func TestValidatedCopyMonitorURL_AllowsPersonalCopyMonitorHost(t *testing.T) {
	t.Parallel()

	client := &Client{}

	validated, err := client.validatedCopyMonitorURL("https://my.microsoftpersonalcontent.com/personal/operations/copy")
	require.NoError(t, err)
	assert.Equal(t, "https://my.microsoftpersonalcontent.com/personal/operations/copy", validated)
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
				writeClientTestBody(t, w, `{"error":"something"}`)
			}))
			defer srv.Close()

			// Use no-retry client — these are non-retryable codes.
			client := newNoRetryTestClient(t, srv.URL)
			resp, err := client.do(t.Context(), http.MethodGet, "/test", nil)
			closeClientTestResponse(t, resp)
			require.Error(t, err)
			require.ErrorIs(t, err, tt.sentinel)

			var graphErr *GraphError
			require.ErrorAs(t, err, &graphErr)
			assert.Equal(t, tt.status, graphErr.StatusCode)
			assert.Equal(t, "test-req-id", graphErr.RequestID)
		})
	}
}

func TestDo_ParsesStructuredGraphErrors(t *testing.T) {
	t.Run("top level and nested codes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("request-id", "test-req-id")
			w.WriteHeader(http.StatusBadRequest)
			writeClientTestBody(t, w, `{
				"error": {
					"code": "badRequest",
					"message": "outer message",
					"innerError": {
						"code": "invalidRequest",
						"innerError": {
							"code": "deepCode"
						}
					}
				}
			}`)
		}))
		defer srv.Close()

		client := newNoRetryTestClient(t, srv.URL)
		resp, err := client.do(t.Context(), http.MethodGet, "/test", nil)
		closeClientTestResponse(t, resp)
		require.Error(t, err)

		var graphErr *GraphError
		require.ErrorAs(t, err, &graphErr)
		assert.Equal(t, http.StatusBadRequest, graphErr.StatusCode)
		assert.Equal(t, "badRequest", graphErr.Code)
		assert.Equal(t, []string{"invalidRequest", "deepCode"}, graphErr.InnerCodes)
		assert.Equal(t, "deepCode", graphErr.MostSpecificCode())
		assert.True(t, graphErr.HasCode("badRequest"))
		assert.True(t, graphErr.HasCode("invalidRequest"))
		assert.True(t, graphErr.HasCode("deepCode"))
		assert.False(t, graphErr.HasCode("itemNotFound"))
		assert.Equal(t, "outer message", graphErr.Message)
		assert.Contains(t, graphErr.RawBody, `"deepCode"`)
	})

	t.Run("malformed and non-json responses fall back to raw body", func(t *testing.T) {
		tests := []struct {
			name      string
			body      string
			expectRaw bool
		}{
			{name: "non-json", body: `plain text error body`, expectRaw: true},
			{name: "json without graph envelope", body: `{"error":"something"}`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("request-id", "test-req-id")
					w.WriteHeader(http.StatusBadRequest)
					writeClientTestBody(t, w, tt.body)
				}))
				defer srv.Close()

				client := newNoRetryTestClient(t, srv.URL)
				resp, err := client.do(t.Context(), http.MethodGet, "/test", nil)
				closeClientTestResponse(t, resp)
				require.Error(t, err)

				var graphErr *GraphError
				require.ErrorAs(t, err, &graphErr)
				assert.Empty(t, graphErr.Code)
				assert.Empty(t, graphErr.InnerCodes)
				if tt.expectRaw {
					assert.Equal(t, tt.body, graphErr.Message)
					assert.Equal(t, tt.body, graphErr.RawBody)
					return
				}

				assert.JSONEq(t, tt.body, graphErr.Message)
				assert.JSONEq(t, tt.body, graphErr.RawBody)
			})
		}
	})
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
		writeClientTestBody(t, w, `ok`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.do(t.Context(), http.MethodGet, "/retry", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int32(3), calls.Load())
}

// Validates: R-6.8.1
func TestDo_RetryAfterHandling(t *testing.T) {
	tests := []struct {
		name         string
		retryAfter   string
		wantAttempts int32
	}{
		{
			name:         "numeric Retry-After",
			retryAfter:   "1",
			wantAttempts: 2,
		},
		{
			name:         "malformed Retry-After falls back to backoff",
			retryAfter:   "not-a-number",
			wantAttempts: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n := calls.Add(1)
				if n <= 1 {
					w.Header().Set("Retry-After", tt.retryAfter)
					w.WriteHeader(http.StatusTooManyRequests)

					return
				}

				w.WriteHeader(http.StatusOK)
				writeClientTestBody(t, w, `ok`)
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			resp, err := client.do(t.Context(), http.MethodGet, "/throttle", nil)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tt.wantAttempts, calls.Load())
		})
	}
}

func TestDo_RetryPolicyDecisions(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		path         string
		wantErr      error
		wantAttempts int32
	}{
		{
			name:         "retryable 5xx exhausts retries",
			status:       http.StatusServiceUnavailable,
			path:         "/fail",
			wantErr:      ErrServerError,
			wantAttempts: 6,
		},
		{
			name:         "non-retryable 4xx does not retry",
			status:       http.StatusNotFound,
			path:         "/missing",
			wantErr:      ErrNotFound,
			wantAttempts: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			resp, err := client.do(t.Context(), http.MethodGet, tt.path, nil)
			closeClientTestResponse(t, resp)
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, tt.wantAttempts, calls.Load())
		})
	}
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

	client := MustNewClient(srv.URL, http.DefaultClient, staticToken("my-secret-token"), slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/auth", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDo_DebugLogsNeverExposeBearerToken(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := MustNewClient(srv.URL, http.DefaultClient, staticToken("my-secret-token"), logger, "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/auth", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NotContains(t, logBuf.String(), "my-secret-token")
}

func TestDo_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	client := newNoRetryTestClient(t, srv.URL)
	resp, err := client.do(ctx, http.MethodGet, "/cancel", nil)
	closeClientTestResponse(t, resp)
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

	require.ErrorIs(t, graphErr, ErrNotFound)
	assert.NotErrorIs(t, graphErr, ErrConflict)
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
	resp, err := client.do(t.Context(), http.MethodGet, "/ua", nil)
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
	resp, err := client.do(t.Context(), http.MethodPost, "/create", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
}

func TestDo_TokenError(t *testing.T) {
	client := MustNewClient("http://localhost", http.DefaultClient, failingToken{}, slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/test", nil)
	closeClientTestResponse(t, resp)
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
	c, err := NewClient("http://localhost", nil, staticToken("tok"), nil, "")
	require.NoError(t, err)
	assert.NotNil(t, c.httpClient)
	assert.NotNil(t, c.logger)
}

func TestNewClient_NilTokenSourceReturnsError(t *testing.T) {
	c, err := NewClient("http://localhost", nil, nil, nil, "")
	require.Error(t, err)
	assert.Nil(t, c)
	assert.Contains(t, err.Error(), "token source")
}

func TestDoWithHeaders_SendsExtraHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "deltashowremoteitemsaliasid", r.Header.Get("Prefer"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		writeClientTestBody(t, w, `{"value":"ok"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	headers := http.Header{"Prefer": {"deltashowremoteitemsaliasid"}}

	resp, err := client.doGetWithHeaders(t.Context(), "/test", headers)
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

	resp, err := client.doGetWithHeaders(t.Context(), "/test", nil)
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
		writeClientTestBody(t, w, `ok`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	headers := http.Header{"Prefer": {"deltashowremoteitemsaliasid"}}

	resp, err := client.doGetWithHeaders(t.Context(), "/retry", headers)
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
		if !assert.NoError(t, readErr) {
			return
		}
		assert.Equal(t, expectedBody, string(body))

		n := calls.Add(1)
		if n <= 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.WriteHeader(http.StatusOK)
		writeClientTestBody(t, w, `{"id":"created"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	resp, err := client.do(
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

func TestDo_NetworkError_MaxRetries(t *testing.T) {
	// Point the client at an unreachable address and verify that all retries
	// are exhausted before returning an error.
	client := MustNewClient("http://127.0.0.1:1", retryHTTPClient(http.DefaultClient, testRetryPolicy()), staticToken("tok"), slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/unreachable", nil)
	closeClientTestResponse(t, resp)
	require.Error(t, err)
}

// --- doPreAuth tests ---

func TestDoPreAuth_SuccessFirstTry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify no Authorization header is sent.
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "test-agent", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusOK)
		writeClientTestBody(t, w, `ok`)
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuth(t.Context(), "test op", func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/test", http.NoBody)
		if reqErr != nil {
			return nil, fmt.Errorf("build pre-auth request: %w", reqErr)
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

	resp, err := client.doPreAuth(t.Context(), "404 test", func() (*http.Request, error) {
		return http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/missing", http.NoBody)
	})
	closeClientTestResponse(t, resp)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotFound)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, "test-req-id", graphErr.RequestID)

	// No retries for non-retryable 4xx.
	assert.Equal(t, int32(1), calls.Load())
}

func TestDoPreAuth_MakeReqError(t *testing.T) {
	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuth(t.Context(), "bad factory", func() (*http.Request, error) {
		return nil, errors.New("factory failed")
	})
	closeClientTestResponse(t, resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory failed")
}

func TestDo_ErrorBodyCappedAt64KiB(t *testing.T) {
	// Verify that error response bodies are capped at maxErrBodySize (64 KiB)
	// to prevent OOM from malicious/buggy servers (B-314).
	bigBody := strings.Repeat("X", 128*1024) // 128 KiB — twice the cap

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeClientTestBody(t, w, bigBody)
	}))
	defer srv.Close()

	client := newNoRetryTestClient(t, srv.URL)

	resp, err := client.do(t.Context(), http.MethodGet, "/big-error", nil)
	closeClientTestResponse(t, resp)
	require.Error(t, err)

	var ge *GraphError
	require.ErrorAs(t, err, &ge)
	assert.LessOrEqual(t, len(ge.Message), maxErrBodySize, "error body should be capped at 64 KiB")
	assert.Len(t, ge.Message, maxErrBodySize, "error body should be exactly 64 KiB (truncated)")
}

func TestDoPreAuth_ErrorBodyCappedAt64KiB(t *testing.T) {
	// Same test but for the pre-auth path (B-314).
	bigBody := strings.Repeat("Y", 128*1024)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeClientTestBody(t, w, bigBody)
	}))
	defer srv.Close()

	client := newTestClient(t, "http://unused")

	resp, err := client.doPreAuth(t.Context(), "big error", func() (*http.Request, error) {
		return http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/big", http.NoBody)
	})
	closeClientTestResponse(t, resp)
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
			writeClientTestBody(t, w, `{"error":"server error"}`)
		}))
		defer srv.Close()

		twoRetryPolicy := retry.Policy{
			MaxAttempts: 2,
			Base:        1 * time.Millisecond,
			Max:         10 * time.Millisecond,
			Multiplier:  2.0,
			Jitter:      0.0,
		}

		client := MustNewClient(srv.URL, retryHTTPClient(http.DefaultClient, twoRetryPolicy), staticToken("tok"), slog.Default(), "test-agent")

		resp, err := client.do(t.Context(), http.MethodGet, "/fail", nil)
		closeClientTestResponse(t, resp)
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
			writeClientTestBody(t, w, `{"error":"server error"}`)
		}))
		defer srv.Close()

		client := newNoRetryTestClient(t, srv.URL)

		resp, err := client.do(t.Context(), http.MethodGet, "/fail", nil)
		closeClientTestResponse(t, resp)
		require.Error(t, err)
		assert.Equal(t, int32(1), attempts.Load(), "no-retry client should make exactly 1 attempt")
	})
}

// Validates: R-6.8.6
func TestTerminalError_RetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		path           string
		status         int
		retryAfter     string
		body           string
		wantRetryAfter time.Duration
	}{
		{
			name:           "429 with Retry-After populates RetryAfter",
			path:           "/throttle",
			status:         http.StatusTooManyRequests,
			retryAfter:     "42",
			body:           `{"error":"throttled"}`,
			wantRetryAfter: 42 * time.Second,
		},
		{
			name:           "503 with Retry-After populates RetryAfter",
			path:           "/unavail",
			status:         http.StatusServiceUnavailable,
			retryAfter:     "10",
			body:           `{"error":"service unavailable"}`,
			wantRetryAfter: 10 * time.Second,
		},
		{
			name:           "500 without Retry-After has zero RetryAfter",
			path:           "/fail",
			status:         http.StatusInternalServerError,
			body:           `{"error":"server error"}`,
			wantRetryAfter: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
				writeClientTestBody(t, w, tt.body)
			}))
			defer srv.Close()

			client := newNoRetryTestClient(t, srv.URL)
			resp, err := client.do(t.Context(), http.MethodGet, tt.path, nil)
			closeClientTestResponse(t, resp)
			require.Error(t, err)

			var graphErr *GraphError
			require.ErrorAs(t, err, &graphErr)
			assert.Equal(t, tt.wantRetryAfter, graphErr.RetryAfter)
		})
	}
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
			writeClientTestBody(t, w, `{"value":"ok"}`)

			return
		}

		// Old token → 401.
		w.WriteHeader(http.StatusUnauthorized)
		writeClientTestBody(t, w, `{"error":"expired"}`)
	}))
	defer srv.Close()

	ts := &switchingToken{oldTok: "expired-token", newTok: "refreshed-token"}
	client := MustNewClient(srv.URL, http.DefaultClient, ts, slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
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
		writeClientTestBody(t, w, `{"error":"invalid"}`)
	}))
	defer srv.Close()

	// Static token — second Token() call returns the same value, so no refresh.
	client := MustNewClient(srv.URL, http.DefaultClient, staticToken("same-token"), slog.Default(), "test-agent")

	resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.Error(t, err)

	var graphErr *GraphError
	require.ErrorAs(t, err, &graphErr)
	assert.Equal(t, http.StatusUnauthorized, graphErr.StatusCode)
}
