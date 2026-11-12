package devtool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ghFailure is an error the way ExecRunner reports a failed gh command: the
// command, gh's stderr, then the exit status.
func ghFailure(stderr string) error {
	return errors.New("gh [run view 1]: " + stderr + "\n: exit status 1")
}

// githubBadGateway is gh's report of a 502 from the REST API.
func githubBadGateway() error {
	return ghFailure("HTTP 502: Bad Gateway (https://api.github.com/repos/o/r/actions/jobs/1/logs)")
}

// recordedSleeps collects the backoffs a ghClient waited out, returning at
// once.
type recordedSleeps struct {
	waits []time.Duration
}

func (r *recordedSleeps) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("recorded sleep: %w", err)
	}
	r.waits = append(r.waits, d)

	return nil
}

// githubBackoffs is the backoff before each repeat of a request that kept
// failing: the first wait, then each doubled.
func githubBackoffs() []time.Duration {
	waits := make([]time.Duration, 0, githubAttempts-1)
	wait := githubFirstBackoff
	for range githubAttempts - 1 {
		waits = append(waits, wait)
		wait *= 2
	}

	return waits
}

// Validates: R-6.10.16, R-6.10.23
//
// GitHub answers a share of requests with a 5xx or drops the connection, and
// such a failure says nothing about the request. It is repeated with backoff,
// at most githubAttempts times in all; any other failure, a 404 or a rejected
// query, is final at once.
func TestGHClient_RetriesOnlyTransientFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		failures  []error
		wantCalls int
		wantErr   string
	}{
		{
			name:      "a server error, then the answer",
			failures:  []error{ghFailure("HTTP 503: Service Unavailable (https://api.github.com/graphql)")},
			wantCalls: 2,
		},
		{
			name:      "gh api's spelling of a server error",
			failures:  []error{ghFailure("gh: Bad Gateway (HTTP 502)")},
			wantCalls: 2,
		},
		{
			name:      "a reset connection",
			failures:  []error{ghFailure(`Get "https://api.github.com/repos/o/r": read tcp: connection reset by peer`)},
			wantCalls: 2,
		},
		{
			name:      "a connection closed before the answer",
			failures:  []error{ghFailure(`Post "https://api.github.com/graphql": EOF`)},
			wantCalls: 2,
		},
		{
			name:      "not found is final",
			failures:  []error{ghFailure("HTTP 404: Not Found (https://api.github.com/repos/o/r/actions/runs/1)")},
			wantCalls: 1,
			wantErr:   "nightly run view 1: gh [run view 1]: HTTP 404",
		},
		{
			name:      "a server error on every attempt",
			failures:  slices.Repeat([]error{githubBadGateway()}, githubAttempts),
			wantCalls: githubAttempts,
			wantErr:   fmt.Sprintf("nightly run view 1: GitHub answered HTTP 502 on all %d attempts", githubAttempts),
		},
		{
			name:      "a dropped connection on every attempt",
			failures:  slices.Repeat([]error{ghFailure("unexpected EOF")}, githubAttempts),
			wantCalls: githubAttempts,
			wantErr:   fmt.Sprintf("nightly run view 1: GitHub dropped the connection on all %d attempts", githubAttempts),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			runner := &fakeRunner{outputFunc: func(_ string, _ []string, name string, args ...string) ([]byte, error) {
				assert.Equal(t, githubCLI, name)
				assert.Equal(t, []string{"run", "view", "1"}, args)
				calls++
				if calls <= len(tc.failures) {
					return nil, tc.failures[calls-1]
				}

				return []byte("answer"), nil
			}}
			sleeps := &recordedSleeps{}

			out, err := newGHClient(runner, sleeps.sleep).output(t.Context(), "/repo", "nightly run view 1", "run", "view", "1")

			assert.Equal(t, tc.wantCalls, calls)
			assert.Equal(t, githubBackoffs()[:tc.wantCalls-1], append([]time.Duration{}, sleeps.waits...))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "answer", string(out))
		})
	}
}

// Validates: R-6.10.16, R-6.10.23
func TestGHClient_ContextEndingDuringTheBackoffEndsTheCall(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) {
		calls++
		cancel()

		return nil, githubBadGateway()
	}}

	_, err := newGHClient(runner, (&recordedSleeps{}).sleep).output(ctx, "/repo", "nightly run list", "run", "list")

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, calls)
}

// Validates: R-6.10.16, R-6.10.23
//
// A request that never reached GitHub says as little about the request as a
// dropped one: the runner's network or DNS failed. The texts are Go's
// net/http errors as gh reports them.
func TestGHClient_RetriesARequestThatNeverReachedGitHub(t *testing.T) {
	t.Parallel()

	failures := []string{
		`Post "https://api.github.com/graphql": dial tcp 140.82.112.6:443: i/o timeout`,
		`Get "https://api.github.com/repos/o/r/actions/runs/1": net/http: TLS handshake timeout`,
		`Get "https://api.github.com/repos/o/r": dial tcp 140.82.112.6:443: connect: connection refused`,
		`Get "https://api.github.com/repos/o/r": dial tcp: lookup api.github.com: no such host`,
		`Post "https://api.github.com/graphql": dial tcp: lookup api.github.com: Temporary failure in name resolution`,
		`Get "https://api.github.com/repos/o/r": dial tcp 140.82.112.6:443: connect: network is unreachable`,
	}
	for _, failure := range failures {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			calls := 0
			runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					return nil, ghFailure(failure)
				}

				return []byte("answer"), nil
			}}
			sleeps := &recordedSleeps{}

			out, err := newGHClient(runner, sleeps.sleep).output(t.Context(), "/repo", "dod gh pr view 7", "pr", "view", "7")

			require.NoError(t, err)
			assert.Equal(t, "answer", string(out))
			assert.Equal(t, 2, calls)
			assert.Equal(t, githubBackoffs()[:1], sleeps.waits)
		})
	}

	runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) {
		return nil, ghFailure(failures[0])
	}}
	_, err := newGHClient(runner, (&recordedSleeps{}).sleep).output(t.Context(), "/repo", "dod gh pr view 7", "pr", "view", "7")
	require.ErrorContains(t, err, fmt.Sprintf("dod gh pr view 7: GitHub could not be reached on all %d attempts", githubAttempts))
}

// Validates: R-6.10.16, R-6.10.23
//
// A rate limit is GitHub answering about the rate of requests, and a repeat
// within seconds only deepens it: the request fails at once and says so.
func TestGHClient_RateLimitsAreFinal(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{
		"HTTP 403: You have exceeded a secondary rate limit. Please wait a few minutes before you try again. (https://api.github.com/graphql)",
		"HTTP 429: Too Many Requests (https://api.github.com/repos/o/r/actions/runs)",
		"gh: API rate limit exceeded for user ID 1. (HTTP 403)",
	} {
		calls := 0
		runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) {
			calls++

			return nil, ghFailure(failure)
		}}
		sleeps := &recordedSleeps{}

		_, err := newGHClient(runner, sleeps.sleep).output(t.Context(), "/repo", "nightly run list", "run", "list")

		require.ErrorContains(t, err, "nightly run list: ")
		assert.Equal(t, 1, calls, failure)
		assert.Empty(t, sleeps.waits, failure)
	}
}

// Validates: R-6.10.16
//
// GitHub can carry out a rerun and still answer the request with a 5xx. The
// repeat is then refused because the run is already running; that refusal
// means the first attempt worked. The same refusal to a first attempt is a
// plain failure: nothing this wait sent started the run.
func TestGHClient_RefusalAfterA5xxMeansTheRequestTookEffect(t *testing.T) {
	t.Parallel()

	const alreadyRunning = "HTTP 403: This workflow is already running (https://api.github.com/repos/o/r/actions/runs/1/rerun-failed-jobs)"
	answers := []error{githubBadGateway(), ghFailure(alreadyRunning)}
	calls := 0
	runner := &fakeRunner{outputFunc: func(string, []string, string, ...string) ([]byte, error) {
		calls++

		return nil, answers[min(calls, len(answers))-1]
	}}

	done, err := newGHClient(runner, (&recordedSleeps{}).sleep).outputUnlessDone(
		t.Context(), "/repo", "dod rerun run 1", isRerunAlreadyRunning, "run", "rerun", "1", "--failed")
	require.NoError(t, err)
	assert.True(t, done, "the refusal of the repeat reports that the first attempt took effect")
	assert.Equal(t, 2, calls)

	calls = 1
	_, err = newGHClient(runner, (&recordedSleeps{}).sleep).outputUnlessDone(
		t.Context(), "/repo", "dod rerun run 1", isRerunAlreadyRunning, "run", "rerun", "1", "--failed")
	require.ErrorContains(t, err, "already running")
}
