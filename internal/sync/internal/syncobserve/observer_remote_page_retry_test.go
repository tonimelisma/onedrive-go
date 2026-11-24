package syncobserve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const pageRetrySecondPage = "page-2"

// pageRetryFixture is a three-page change list whose second page answers
// with pageErr until it has done so failures times.
func pageRetryFixture(t *testing.T, pageErr error, failures int) (*RemoteObserver, *syncfixture.ScriptedDeltaFetcher, *[]time.Duration) {
	t.Helper()

	failed := 0
	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, token string) (*graph.DeltaPage, error) {
		switch {
		case call == 1:
			return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem("one")}, NextLink: pageRetrySecondPage}, nil
		case token == pageRetrySecondPage && failed < failures:
			failed++
			return nil, pageErr
		case token == pageRetrySecondPage:
			return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem("two")}, NextLink: "page-3"}, nil
		default:
			return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem("three")}, DeltaLink: "done"}, nil
		}
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})
	slept := &[]time.Duration{}
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}}

	return obs, fetcher, slept
}

// Validates: R-2.15.5
//
// One 504 on page 2 of a change list used to fail the whole one-shot run
// (fault round of 2026-11-03); the page is retried after a short backoff.
func TestFullDelta_RetriesAServerErrorOnAPage(t *testing.T) {
	t.Parallel()

	obs, fetcher, slept := pageRetryFixture(t, syncfixture.GraphStatusError(http.StatusGatewayTimeout, graph.ErrServerError), 1)

	events, token, err := obs.FullDelta(t.Context(), "saved-token")

	require.NoError(t, err)
	assert.Equal(t, "done", token)
	assert.Len(t, events, 3, "every page's items arrive")
	assert.Equal(t, []string{"saved-token", pageRetrySecondPage, pageRetrySecondPage, "page-3"}, fetcher.Seen(), "page 2 is fetched again, not the whole list")
	assert.Equal(t, []time.Duration{2 * time.Second}, *slept)
}

// Validates: R-2.15.5
func TestFullDelta_WaitsAShortRetryAfterOnAPage(t *testing.T) {
	t.Parallel()

	throttled := &graph.GraphError{StatusCode: http.StatusTooManyRequests, Code: "activityLimitReached", Err: graph.ErrThrottled, RetryAfter: 5 * time.Second}
	obs, fetcher, slept := pageRetryFixture(t, throttled, 1)

	_, token, err := obs.FullDelta(t.Context(), "saved-token")

	require.NoError(t, err)
	assert.Equal(t, "done", token)
	assert.Len(t, fetcher.Seen(), 4)
	assert.Equal(t, []time.Duration{5 * time.Second}, *slept, "the Retry-After is honored as given")
}

// Validates: R-2.15.5
func TestFullDelta_ALongRetryAfterOnAPageIsLeftToTheCaller(t *testing.T) {
	t.Parallel()

	throttled := &graph.GraphError{StatusCode: http.StatusTooManyRequests, Code: "activityLimitReached", Err: graph.ErrThrottled, RetryAfter: 10 * time.Minute}
	obs, fetcher, slept := pageRetryFixture(t, throttled, 1)

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, graph.ErrThrottled)
	assert.Len(t, fetcher.Seen(), 2, "no second try: the pause is the caller's to wait out or report")
	assert.Empty(t, *slept)
}

// Validates: R-2.15.5
func TestFullDelta_AClientErrorOnAPageIsNotRetried(t *testing.T) {
	t.Parallel()

	obs, fetcher, slept := pageRetryFixture(t, syncfixture.GraphStatusError(http.StatusForbidden, graph.ErrForbidden), 1)

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, graph.ErrForbidden)
	assert.Len(t, fetcher.Seen(), 2)
	assert.Empty(t, *slept)
}

// Validates: R-2.15.5
func TestFullDelta_GivesUpAPageAfterItsAttempts(t *testing.T) {
	t.Parallel()

	obs, fetcher, slept := pageRetryFixture(t, syncfixture.GraphStatusError(http.StatusServiceUnavailable, graph.ErrServerError), 10)

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, graph.ErrServerError)
	assert.Len(t, fetcher.Seen(), 1+4, "four attempts at page 2, then the error surfaces")
	assert.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}, *slept)
	assert.Contains(t, fmt.Sprint(err), "delta page 1", "pages are counted from zero")
}
