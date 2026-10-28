package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// scriptedDeltaFetcher answers each delta call from a script and records the
// token every call carried.
type scriptedDeltaFetcher struct {
	mu     stdsync.Mutex // guards tokens
	answer func(call int, token string) (*graph.DeltaPage, error)
	tokens []string
}

func (f *scriptedDeltaFetcher) Delta(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
	f.mu.Lock()
	f.tokens = append(f.tokens, token)
	call := len(f.tokens)
	f.mu.Unlock()

	return f.answer(call, token)
}

func (f *scriptedDeltaFetcher) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.tokens...)
}

func cursorTestItem(id string) graph.Item {
	return graph.Item{
		ID: id, Name: id + ".txt", ParentID: "root", ParentPathKnown: true,
		DriveID: driveid.New(engineTestDriveID), QuickXorHash: "h-" + id, Size: 1,
	}
}

func graphStatusError(status int, sentinel error) error {
	return &graph.GraphError{StatusCode: status, Code: http.StatusText(status), Err: sentinel}
}

// Validates: R-2.15.2
func TestFullDelta_RepeatedNextLinkIsCursorUnusable(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(call int, _ string) (*graph.DeltaPage, error) {
		// Page 2 links back to itself: Graph would be followed for up to
		// 10,000 pages with every item buffered.
		return &graph.DeltaPage{Items: []graph.Item{cursorTestItem(fmt.Sprintf("i%d", call))}, NextLink: "page-2"}, nil
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, errDeltaExpired, "a looping change list takes the expired-token recovery")
	assert.Equal(t, []string{"saved-token", "page-2"}, fetcher.seen(), "the loop is noticed after two fetches")
}

// Validates: R-2.15.2
func TestFullDelta_RepeatedPageItemsIsCursorUnusable(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(call int, _ string) (*graph.DeltaPage, error) {
		return &graph.DeltaPage{
			Items:    []graph.Item{cursorTestItem("same-a"), cursorTestItem("same-b")},
			NextLink: fmt.Sprintf("page-%d", call+1),
		}, nil
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, errDeltaExpired)
	assert.Len(t, fetcher.seen(), 2)
}

// Validates: R-2.15.2
func TestFullDelta_EmptyPageWithNextLinkContinues(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(call int, _ string) (*graph.DeltaPage, error) {
		switch call {
		case 1, 2:
			// Empty pages are not a loop: Graph sends them while it catches up.
			return &graph.DeltaPage{NextLink: fmt.Sprintf("page-%d", call+1)}, nil
		default:
			return &graph.DeltaPage{Items: []graph.Item{cursorTestItem("x")}, DeltaLink: "done"}, nil
		}
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))

	events, token, err := obs.FullDelta(t.Context(), "saved-token")

	require.NoError(t, err)
	assert.Equal(t, "done", token)
	assert.Len(t, events, 1)
}

// Validates: R-2.15.2
func TestFullDelta_SavedTokenRejectedTwiceIsCursorUnusable(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		rejected error
		unusable bool
	}{
		{name: "400", rejected: graphStatusError(http.StatusBadRequest, graph.ErrBadRequest), unusable: true},
		{name: "404", rejected: graphStatusError(http.StatusNotFound, graph.ErrNotFound), unusable: true},
		{name: "401 is sign-in, not the cursor", rejected: graphStatusError(http.StatusUnauthorized, graph.ErrUnauthorized)},
		{name: "403 is permission, not the cursor", rejected: graphStatusError(http.StatusForbidden, graph.ErrForbidden)},
		{name: "429 is throttling, not the cursor", rejected: graphStatusError(http.StatusTooManyRequests, graph.ErrThrottled)},
		{name: "5xx is transient", rejected: graphStatusError(http.StatusInternalServerError, graph.ErrServerError)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := &scriptedDeltaFetcher{answer: func(int, string) (*graph.DeltaPage, error) {
				return nil, tt.rejected
			}}
			obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))

			_, _, err := obs.FullDelta(t.Context(), "saved-token")

			require.Error(t, err)
			if !tt.unusable {
				assert.NotErrorIs(t, err, errDeltaExpired)
				assert.Len(t, fetcher.seen(), 1, "not retried as a cursor problem")
				return
			}
			require.ErrorIs(t, err, errDeltaExpired, "a token OneDrive keeps refusing is re-enumerated")
			assert.Equal(t, []string{"saved-token", "saved-token"}, fetcher.seen())
		})
	}
}

// Validates: R-2.15.2
func TestFullDelta_OneRejectionOfASavedTokenIsRetried(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(call int, _ string) (*graph.DeltaPage, error) {
		if call == 1 {
			return nil, graphStatusError(http.StatusBadRequest, graph.ErrBadRequest)
		}
		return &graph.DeltaPage{DeltaLink: "next-token"}, nil
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))

	_, token, err := obs.FullDelta(t.Context(), "saved-token")

	require.NoError(t, err)
	assert.Equal(t, "next-token", token)
}

// Port of rclone's TestChangeNotifyRetriesAndKeepsToken: a transient failure
// is retried with the same token, which is neither dropped nor advanced.
//
// Validates: R-2.15.2
func TestWatch_TransientErrorRetriesAndKeepsToken(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(call int, _ string) (*graph.DeltaPage, error) {
		if call <= 2 {
			return nil, graphStatusError(http.StatusServiceUnavailable, graph.ErrServerError)
		}
		return &graph.DeltaPage{DeltaLink: "kept-token"}, nil
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sleeps := 0
	obs.clock = &observerTestClock{syncClock: realClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps < 3 {
			return nil
		}
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	obs.SetDeltaExpiredRecovery(func(context.Context, resyncKind) (remoteObservationBatch, error) {
		return remoteObservationBatch{}, errors.New("a transient failure must not reset the cursor")
	})
	batches := make(chan remoteObservationBatch, 4)
	_ = startTestRemoteBatchDrain(ctx, batches, nil)

	require.NoError(t, obs.Watch(ctx, "kept-token", batches, time.Hour, nil, nil))

	assert.Equal(t, []string{"kept-token", "kept-token", "kept-token"}, fetcher.seen())
	assert.Equal(t, "kept-token", obs.CurrentDeltaToken())
}

// Validates: R-2.15.2
func TestWatch_SustainedServerErrorsOnTheCursorResetIt(t *testing.T) {
	t.Parallel()

	fetcher := &scriptedDeltaFetcher{answer: func(int, string) (*graph.DeltaPage, error) {
		return nil, graphStatusError(http.StatusInternalServerError, graph.ErrServerError)
	}}
	obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := newManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	obs.clock = &observerTestClock{syncClock: clock, sleep: func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		return nil
	}}
	recovered := make(chan struct{})
	obs.SetDeltaExpiredRecovery(func(context.Context, resyncKind) (remoteObservationBatch, error) {
		close(recovered)
		cancel()
		return remoteObservationBatch{}, context.Canceled
	})
	batches := make(chan remoteObservationBatch, 4)
	_ = startTestRemoteBatchDrain(ctx, batches, nil)

	require.NoError(t, obs.Watch(ctx, "stuck-token", batches, time.Minute, nil, nil))

	select {
	case <-recovered:
	default:
		require.Fail(t, "hours of 5xx on one token end in a re-enumeration")
	}
	assert.GreaterOrEqual(t, clock.Now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), cursorServerErrorResetAfter)
}
