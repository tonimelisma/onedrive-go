package graphtransport

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// Validates: R-6.2.10, R-6.8.8
func TestBootstrapMetadataClient(t *testing.T) {
	t.Parallel()

	client := BootstrapMetadataClient(testLogger())

	require.NotNil(t, client)
	assert.Zero(t, client.Timeout, "bootstrap metadata client must not use http.Client.Timeout")

	rt, ok := client.Transport.(*retry.RetryTransport)
	require.True(t, ok, "bootstrap metadata client should use RetryTransport")

	perfRT, ok := rt.Inner.(perf.RoundTripper)
	require.True(t, ok, "bootstrap metadata inner transport should be perf.RoundTripper")

	inner, ok := perfRT.Inner.(*http.Transport)
	require.True(t, ok, "bootstrap metadata inner transport should be *http.Transport")
	assert.Equal(t, metadataResponseHeaderTimeout, inner.ResponseHeaderTimeout)
	assert.Nil(t, rt.ThrottleGate, "bootstrap metadata should not share throttle state")
}

// Validates: R-6.2.10
func TestSyncClientSet_NoRetryTransport(t *testing.T) {
	t.Parallel()

	clients := SyncClientSet()

	require.NotNil(t, clients.Meta)
	require.NotNil(t, clients.Transfer)
	assert.Zero(t, clients.Meta.Timeout, "sync metadata client must not use http.Client.Timeout")
	assert.Zero(t, clients.Transfer.Timeout, "sync transfer client must not use http.Client.Timeout")
	_, hasRetryTransport := clients.Meta.Transport.(*retry.RetryTransport)
	assert.False(t, hasRetryTransport, "sync metadata should not use RetryTransport")

	metaPerfRT, ok := clients.Meta.Transport.(perf.RoundTripper)
	require.True(t, ok, "sync metadata transport should be perf.RoundTripper")

	metaTransport, ok := metaPerfRT.Inner.(*http.Transport)
	require.True(t, ok, "sync metadata inner transport should be *http.Transport")
	assert.Equal(t, metadataResponseHeaderTimeout, metaTransport.ResponseHeaderTimeout)

	transferPerfRT, ok := clients.Transfer.Transport.(perf.RoundTripper)
	require.True(t, ok, "sync transfer transport should be perf.RoundTripper")

	transferTransport, ok := transferPerfRT.Inner.(*http.Transport)
	require.True(t, ok, "sync transfer inner transport should be *http.Transport")
	assert.Equal(t, transferResponseHeaderTimeout, transferTransport.ResponseHeaderTimeout)
}

// Validates: R-6.8.8
func TestInteractiveMetadataClient_TransportRetryDeadlineIsRetryable(t *testing.T) {
	t.Parallel()

	retryTransport, ok := InteractiveMetadataClient(testLogger(), &retry.ThrottleGate{}).Transport.(*retry.RetryTransport)
	require.True(t, ok)

	var attempts int
	retryTransport.Inner = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, context.DeadlineExceeded
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     make(http.Header),
		}, nil
	})
	retryTransport.Policy = retry.Policy{
		MaxAttempts: 1,
		Base:        time.Millisecond,
		Max:         time.Millisecond,
		Multiplier:  1,
		Jitter:      0,
	}
	retryTransport.Sleep = func(_ context.Context, _ time.Duration) error { return nil }

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)

	resp, err := retryTransport.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 2, attempts, "transport timeout should be retried while request context is still live")
	require.NoError(t, resp.Body.Close())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
