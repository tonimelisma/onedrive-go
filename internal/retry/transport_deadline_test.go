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

const (
	// deadlineTestBudget is the request deadline of the tests below: longer
	// than the test takes, shorter than every wait the server asks for.
	deadlineTestBudget = 5 * time.Second
	// deadlineTestLongWait is a Retry-After, backoff or throttle wait that
	// ends after deadlineTestBudget.
	deadlineTestLongWait = 2 * time.Minute
	// deadlineTestRetryAfter is deadlineTestLongWait as a Retry-After value.
	deadlineTestRetryAfter = "120"
	// outageNoticeMultiplier makes the backoff after a second network
	// failure 16 s: longer than deadlineTestBudget, while the first, 1 s,
	// fits.
	outageNoticeMultiplier = 16
)

// longBackoffPolicy retries, but its first backoff already outlasts
// deadlineTestBudget.
func longBackoffPolicy() retry.Policy {
	return retry.Policy{
		MaxAttempts: 4,
		Base:        deadlineTestLongWait,
		Max:         deadlineTestLongWait,
		Multiplier:  2.0,
		Jitter:      0.0,
	}
}

// answer is what a test's inner transport answers every attempt with.
type answer func() (*http.Response, error)

func rateLimitedPastTheDeadline() (*http.Response, error) {
	return makeResponse(http.StatusTooManyRequests, map[string]string{"Retry-After": deadlineTestRetryAfter}), nil
}

func serverError() (*http.Response, error) {
	return makeResponse(http.StatusServiceUnavailable, nil), nil
}

func networkError() (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

func answeredOK() (*http.Response, error) {
	return makeResponse(http.StatusOK, nil), nil
}

// deadlineTransport counts the attempts it sends and the waits it starts,
// and never blocks in a wait.
type deadlineTransport struct {
	rt       *retry.RetryTransport
	attempts atomic.Int32
	waits    atomic.Int32
}

func newDeadlineTransport(policy retry.Policy, logger *slog.Logger, respond answer) *deadlineTransport {
	dt := &deadlineTransport{}
	dt.rt = &retry.RetryTransport{
		Inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
			dt.attempts.Add(1)
			return respond()
		}),
		Policy: policy,
		Logger: logger,
		Sleep: func(context.Context, time.Duration) error {
			dt.waits.Add(1)
			return nil
		},
	}

	return dt
}

// throttleUntil gives the transport a shared throttle gate holding until, as
// an earlier 429 of another transport in its scope would have.
func (dt *deadlineTransport) throttleUntil(until time.Time) {
	gate := &retry.ThrottleGate{}
	gate.SetDeadline(until)
	dt.rt.ThrottleGate = gate
}

// deadlineRequest is a GET whose context ends deadlineTestBudget from now.
func deadlineRequest(t *testing.T) *http.Request {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), deadlineTestBudget)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/me", http.NoBody)
	require.NoError(t, err)

	return req
}

// Validates: R-6.8.8, R-6.8.28
//
// A wait that would end after the request's deadline cannot be followed by an
// attempt, so the transport returns what it has at once: the 429 or 503
// OneDrive sent, the network error, or, for a throttle wait an earlier 429
// set, an unsent ThrottledError. Waiting would only have turned each into the
// deadline's error, later.
func TestRetryTransport_AWaitPastTheDeadlineReturnsTheAnswerAtOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		respond      answer
		policy       retry.Policy
		throttled    bool
		wantStatus   int
		wantErr      error
		wantAttempts int32
	}{
		{
			name:         "429 with a Retry-After past the deadline",
			respond:      rateLimitedPastTheDeadline,
			policy:       testPolicy(),
			wantStatus:   http.StatusTooManyRequests,
			wantAttempts: 1,
		},
		{
			name:         "503 whose backoff ends past the deadline",
			respond:      serverError,
			policy:       longBackoffPolicy(),
			wantStatus:   http.StatusServiceUnavailable,
			wantAttempts: 1,
		},
		{
			name:         "network error whose backoff ends past the deadline",
			respond:      networkError,
			policy:       longBackoffPolicy(),
			wantErr:      io.ErrUnexpectedEOF,
			wantAttempts: 1,
		},
		{
			name:         "throttle wait past the deadline",
			respond:      answeredOK,
			policy:       testPolicy(),
			throttled:    true,
			wantAttempts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dt := newDeadlineTransport(tt.policy, slog.New(slog.DiscardHandler), tt.respond)
			throttleUntil := time.Now().Add(deadlineTestLongWait)
			if tt.throttled {
				dt.throttleUntil(throttleUntil)
			}

			resp, err := dt.rt.RoundTrip(deadlineRequest(t))
			defer closeTestResponse(t, resp)

			assert.Equal(t, tt.wantAttempts, dt.attempts.Load())
			assert.Zero(t, dt.waits.Load(), "no wait may start that ends after the deadline")

			switch {
			case tt.throttled:
				var throttled *retry.ThrottledError
				require.ErrorAs(t, err, &throttled, "a throttle wait past the deadline answers, unsent")
				assert.Equal(t, throttleUntil, throttled.Until)
				assert.Nil(t, resp)
			case tt.wantErr != nil:
				require.ErrorIs(t, err, tt.wantErr, "the network error, not the deadline's")
				require.NotErrorIs(t, err, context.DeadlineExceeded)
				assert.Nil(t, resp)
			default:
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Equal(t, tt.wantStatus, resp.StatusCode, "the answer OneDrive gave, not the deadline's error")
			}
		})
	}
}

// Validates: R-6.6.8, R-6.8.8
//
// A wait the transport does not start is not announced: neither the
// rate-limit notice nor the outage notice, which belong to waits a person
// would sit through.
func TestRetryTransport_AWaitPastTheDeadlineIsNotAnnounced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		respond      answer
		policy       retry.Policy
		gate         bool
		wantAttempts int32
	}{
		{name: "rate limit", respond: rateLimitedPastTheDeadline, policy: testPolicy(), wantAttempts: 1},
		{name: "server error", respond: serverError, policy: longBackoffPolicy(), wantAttempts: 1},
		{name: "throttle gate", respond: answeredOK, policy: testPolicy(), gate: true, wantAttempts: 0},
		{
			// The wait after the first failure (1 s) fits and is silent, as
			// a single blip always is. The second failure is the one that
			// announces an outage, but its wait (16 s) would pass the
			// deadline, so there is no wait to announce.
			name:    "network error",
			respond: networkError,
			policy: retry.Policy{
				MaxAttempts: 6, Base: time.Second, Max: deadlineTestLongWait, Multiplier: outageNoticeMultiplier,
			},
			wantAttempts: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logBuf bytes.Buffer
			dt := newDeadlineTransport(tt.policy, slog.New(slog.NewJSONHandler(&logBuf, nil)), tt.respond)
			if tt.gate {
				dt.throttleUntil(time.Now().Add(deadlineTestLongWait))
			}

			resp, err := dt.rt.RoundTrip(deadlineRequest(t))
			closeTestResponse(t, resp)
			require.NotErrorIs(t, err, context.DeadlineExceeded)

			assert.Equal(t, tt.wantAttempts, dt.attempts.Load())
			assert.Empty(t, warnMessages(t, logBuf.Bytes()), "a wait not taken is not announced")
		})
	}
}

// Validates: R-6.8.8
//
// A request without a deadline, or with one the wait ends before, waits and
// retries as it always has.
func TestRetryTransport_NoDeadlineWaitsAsBefore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ctx  func(*testing.T) context.Context
	}{
		{
			name: "no deadline",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				return t.Context()
			},
		},
		{
			name: "a deadline after the wait",
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 2*deadlineTestLongWait)
				t.Cleanup(cancel)
				return ctx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			var slept []time.Duration
			rt := &retry.RetryTransport{
				Inner: roundTripFunc(func(*http.Request) (*http.Response, error) {
					if attempts.Add(1) == 1 {
						return rateLimitedPastTheDeadline()
					}
					return answeredOK()
				}),
				Policy: testPolicy(),
				Logger: slog.New(slog.DiscardHandler),
				Sleep: func(_ context.Context, d time.Duration) error {
					slept = append(slept, d)
					return nil
				},
			}

			req, err := http.NewRequestWithContext(tt.ctx(t), http.MethodGet, "https://example.com/me", http.NoBody)
			require.NoError(t, err)

			resp, err := rt.RoundTrip(req)
			require.NoError(t, err)
			defer closeTestResponse(t, resp)

			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, int32(2), attempts.Load())
			assert.Equal(t, []time.Duration{deadlineTestLongWait}, slept, "the Retry-After is waited out")
		})
	}
}
