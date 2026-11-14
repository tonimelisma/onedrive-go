package graph

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// Validates: R-6.8.8
//
// A request the retrying transport returns unsent, because the throttle
// deadline an earlier 429 set outlasts the request's own deadline, is the
// throttle's answer: ErrThrottled, and not sent, so a create it stopped is
// known to have created nothing.
func TestDo_AThrottleWaitPastTheDeadlineIsNotSent(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	transport := &retry.RetryTransport{
		Inner:  http.DefaultTransport,
		Policy: testRetryPolicy(),
		Logger: slog.New(slog.DiscardHandler),
		Sleep:  noopSleep,
	}
	client := MustNewClient(srv.URL, &http.Client{Transport: transport}, staticToken("t"), slog.New(slog.DiscardHandler), "test-agent")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// The first request is answered with a 429 that asks for an hour, which
	// sets the transport's throttle deadline.
	resp, err := client.do(ctx, http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.ErrorIs(t, err, ErrThrottled)
	require.NotErrorIs(t, err, errRequestNotSent, "the first request was sent and answered")
	require.Equal(t, int32(1), requests.Load())

	resp, err = client.do(ctx, http.MethodGet, "/me", nil)
	closeClientTestResponse(t, resp)
	require.ErrorIs(t, err, ErrThrottled)
	require.ErrorIs(t, err, errRequestNotSent)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the throttle's answer, at once, not the deadline's")

	item, err := client.CreateFile(ctx, driveid.New("d"), "parent", "a.txt")
	require.ErrorIs(t, err, ErrThrottled)
	require.NotErrorIs(t, err, ErrCreateOutcomeUnknown, "a create that was never sent created nothing")
	assert.Nil(t, item)

	assert.Equal(t, int32(1), requests.Load(), "OneDrive was asked nothing after the 429")
}
