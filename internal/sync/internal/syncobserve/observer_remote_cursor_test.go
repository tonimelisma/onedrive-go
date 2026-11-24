package syncobserve

import (
	"context"
	"errors"
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

// Validates: R-2.15.2
func TestFullDelta_RepeatedNextLinkIsCursorUnusable(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
		// Page 2 links back to itself: Graph would be followed for up to
		// 10,000 pages with every item buffered.
		return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem(fmt.Sprintf("i%d", call))}, NextLink: "page-2"}, nil
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, syncdomain.ErrDeltaExpired, "a looping change list takes the expired-token recovery")
	assert.Equal(t, []string{"saved-token", "page-2"}, fetcher.Seen(), "the loop is noticed after two fetches")
}

// The loop guard keys a page by the path the graph client resolves its
// request and its next link to, so it sees a link back however the link is
// spelled: to the page of the saved token, to the first request of a window
// read without a token, and around a two-page cycle. Each is refused before
// the page is asked for again, and takes the expired-token recovery.
//
// Validates: R-2.15.2
func TestFullDelta_ALinkBackToAPageAskedForIsCursorUnusable(t *testing.T) {
	t.Parallel()

	const (
		initialPath = "/drives/d/root/delta"
		savedPath   = "/drives/d/root/delta?token=a"
		otherPath   = "/drives/d/root/delta?token=b"
		graphRoot   = "HTTPS://GRAPH.MICROSOFT.COM/v1.0"
	)
	for _, tc := range []struct {
		label     string
		saved     string
		pages     []graph.DeltaPage
		wantSeens int
	}{
		{
			label: "a link back to the saved token's page", saved: "https://graph.microsoft.com/v1.0" + savedPath,
			pages:     []graph.DeltaPage{{Path: savedPath, NextLink: graphRoot + savedPath, NextPath: savedPath}},
			wantSeens: 1,
		},
		{
			label:     "a self-link on the first request of a window without a token",
			pages:     []graph.DeltaPage{{Path: initialPath, NextLink: graphRoot + initialPath, NextPath: initialPath}},
			wantSeens: 1,
		},
		{
			label: "a two-page cycle", saved: "https://graph.microsoft.com/v1.0" + savedPath,
			pages: []graph.DeltaPage{
				{Path: savedPath, NextLink: graphRoot + otherPath, NextPath: otherPath},
				{Path: otherPath, NextLink: graphRoot + savedPath, NextPath: savedPath},
			},
			wantSeens: 2,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
				if call > len(tc.pages) {
					return nil, errors.New("a page linked back to was asked for again")
				}
				page := tc.pages[call-1]
				page.Items = []graph.Item{syncfixture.CursorTestItem(fmt.Sprintf("i%d", call))}

				return &page, nil
			}}
			obs := NewRemoteObserver(&RemoteObserverInputs{
				Fetcher:  fetcher,
				Baseline: syncfixture.EmptyBaseline(),
				DriveID:  driveid.New(syncfixture.EngineTestDriveID),
				Logger:   syncfixture.TestLogger(t),
			})

			_, _, err := obs.FullDelta(t.Context(), tc.saved)

			require.ErrorIs(t, err, syncdomain.ErrDeltaExpired, "a looping change list takes the expired-token recovery")
			assert.Len(t, fetcher.Seen(), tc.wantSeens, "no page is asked for twice")
		})
	}
}

// A saved token that is not a link, which the graph client refuses, is a
// cursor that cannot be continued: the drive is enumerated again, not read
// from the initial path as if it were the changes since the token.
//
// Validates: R-2.15.2
func TestFullDelta_ASavedTokenThatIsNotALinkIsCursorUnusable(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(int, string) (*graph.DeltaPage, error) {
		return nil, fmt.Errorf("graph: reading delta token: %w", graph.ErrDeltaTokenNotALink)
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})

	_, _, err := obs.FullDelta(t.Context(), "not-a-link")

	require.ErrorIs(t, err, syncdomain.ErrDeltaExpired)
	require.ErrorIs(t, err, graph.ErrDeltaTokenNotALink)
	assert.Equal(t, []string{"not-a-link"}, fetcher.Seen(), "refused once, not retried")
}

// Validates: R-2.15.2
func TestFullDelta_RepeatedPageItemsIsCursorUnusable(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
		return &graph.DeltaPage{
			Items:    []graph.Item{syncfixture.CursorTestItem("same-a"), syncfixture.CursorTestItem("same-b")},
			NextLink: fmt.Sprintf("page-%d", call+1),
		}, nil
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

	require.ErrorIs(t, err, syncdomain.ErrDeltaExpired)
	assert.Len(t, fetcher.Seen(), 2)
}

// Validates: R-2.15.2
func TestFullDelta_EmptyPageWithNextLinkContinues(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
		switch call {
		case 1, 2:
			// Empty pages are not a loop: Graph sends them while it catches up.
			return &graph.DeltaPage{NextLink: fmt.Sprintf("page-%d", call+1)}, nil
		default:
			return &graph.DeltaPage{Items: []graph.Item{syncfixture.CursorTestItem("x")}, DeltaLink: "done"}, nil
		}
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})

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
		{name: "400", rejected: syncfixture.GraphStatusError(http.StatusBadRequest, graph.ErrBadRequest), unusable: true},
		{name: "404", rejected: syncfixture.GraphStatusError(http.StatusNotFound, graph.ErrNotFound), unusable: true},
		{name: "401 is sign-in, not the cursor", rejected: syncfixture.GraphStatusError(http.StatusUnauthorized, graph.ErrUnauthorized)},
		{name: "403 is permission, not the cursor", rejected: syncfixture.GraphStatusError(http.StatusForbidden, graph.ErrForbidden)},
		{name: "429 is throttling, not the cursor", rejected: syncfixture.GraphStatusError(http.StatusTooManyRequests, graph.ErrThrottled)},
		{name: "5xx is transient", rejected: syncfixture.GraphStatusError(http.StatusInternalServerError, graph.ErrServerError)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(int, string) (*graph.DeltaPage, error) {
				return nil, tt.rejected
			}}
			obs := NewRemoteObserver(&RemoteObserverInputs{
				Fetcher:  fetcher,
				Baseline: syncfixture.EmptyBaseline(),
				DriveID:  driveid.New(syncfixture.EngineTestDriveID),
				Logger:   syncfixture.TestLogger(t),
			})
			obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: noopSleep}

			_, _, err := obs.FullDelta(t.Context(), "saved-token")

			require.Error(t, err)
			if !tt.unusable {
				require.NotErrorIs(t, err, syncdomain.ErrDeltaExpired)
				// A 429 or 5xx is tried again as a transient page fault
				// (R-2.15.5), always with the saved token: never as a
				// cursor problem that re-enumerates the drive.
				for _, token := range fetcher.Seen() {
					assert.Equal(t, "saved-token", token, "not retried as a cursor problem")
				}
				return
			}
			require.ErrorIs(t, err, syncdomain.ErrDeltaExpired, "a token OneDrive keeps refusing is re-enumerated")
			assert.Equal(t, []string{"saved-token", "saved-token"}, fetcher.Seen())
		})
	}
}

// Validates: R-2.15.2
func TestFullDelta_OneRejectionOfASavedTokenIsRetried(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
		if call == 1 {
			return nil, syncfixture.GraphStatusError(http.StatusBadRequest, graph.ErrBadRequest)
		}
		return &graph.DeltaPage{DeltaLink: "next-token"}, nil
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})

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

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(call int, _ string) (*graph.DeltaPage, error) {
		if call <= 2 {
			return nil, syncfixture.GraphStatusError(http.StatusServiceUnavailable, graph.ErrServerError)
		}
		return &graph.DeltaPage{DeltaLink: "kept-token"}, nil
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sleeps := 0
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(ctx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps < 3 {
			return nil
		}
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}}
	obs.recoverExpiredDelta = func(context.Context, syncdomain.ResyncKind) (RemoteObservationBatch, error) {
		return RemoteObservationBatch{}, errors.New("a transient failure must not reset the cursor")
	}
	batches := make(chan RemoteObservationBatch, 4)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)

	require.NoError(t, syncfixture.RunWithin(t, ctx, "the third sleep to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "kept-token", batches, time.Hour, nil, nil)
	}))

	assert.Equal(t, []string{"kept-token", "kept-token", "kept-token"}, fetcher.Seen())
	assert.Equal(t, "kept-token", obs.CurrentDeltaToken())
}

// Validates: R-2.15.2
func TestWatch_SustainedServerErrorsOnTheCursorResetIt(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(int, string) (*graph.DeltaPage, error) {
		return nil, syncfixture.GraphStatusError(http.StatusInternalServerError, graph.ErrServerError)
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := syncfixture.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	obs.clock = &observerTestClock{SyncClock: clock, sleep: func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		return nil
	}}
	recovered := make(chan struct{})
	obs.recoverExpiredDelta = func(context.Context, syncdomain.ResyncKind) (RemoteObservationBatch, error) {
		close(recovered)
		cancel()
		return RemoteObservationBatch{}, context.Canceled
	}
	batches := make(chan RemoteObservationBatch, 4)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)

	require.NoError(t, syncfixture.RunWithin(t, ctx, "the cursor reset's re-enumeration to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "stuck-token", batches, time.Minute, nil, nil)
	}))

	select {
	case <-recovered:
	default:
		require.Fail(t, "hours of 5xx on one token end in a re-enumeration")
	}
	assert.GreaterOrEqual(t, clock.Now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), cursorServerErrorResetAfter)
}

// A recovery that fails the same way every time (its own token-less
// enumeration loops, or every fresh token is refused) must back off and
// respect a minimum spacing, never re-enumerate the drive in a hot loop.
//
// Validates: R-2.15.2, R-2.15.3
func TestWatch_RepeatedlyFailingRecoveryBacksOff(t *testing.T) {
	t.Parallel()

	fetcher := &syncfixture.ScriptedDeltaFetcher{Answer: func(int, string) (*graph.DeltaPage, error) {
		return nil, syncfixture.DeltaGone("resyncChangesApplyDifferences")
	}}
	obs := NewRemoteObserver(&RemoteObserverInputs{
		Fetcher:  fetcher,
		Baseline: syncfixture.EmptyBaseline(),
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),
		Logger:   syncfixture.TestLogger(t),
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := syncfixture.NewManualClock(start)
	var slept time.Duration
	obs.clock = &observerTestClock{SyncClock: clock, sleep: func(_ context.Context, delay time.Duration) error {
		slept += delay
		clock.Advance(delay)
		return nil
	}}
	const attempts = 5
	var recoveredAt []time.Time
	obs.recoverExpiredDelta = func(context.Context, syncdomain.ResyncKind) (RemoteObservationBatch, error) {
		recoveredAt = append(recoveredAt, clock.Now())
		if len(recoveredAt) == attempts {
			cancel()
		}
		return RemoteObservationBatch{}, syncdomain.NewCursorUnusableError("a page links back to an earlier page", nil)
	}
	batches := make(chan RemoteObservationBatch, 4)
	_ = startTestRemoteBatchDrain(t, ctx, batches, nil)

	require.NoError(t, syncfixture.RunWithin(t, ctx, "the last recovery attempt to end the watch", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "expired", batches, time.Minute, nil, nil)
	}))

	require.Len(t, recoveredAt, attempts)
	for i := 1; i < attempts; i++ {
		assert.GreaterOrEqual(t, recoveredAt[i].Sub(recoveredAt[i-1]), expiredDeltaRecoveryMinInterval,
			"recovery %d ran too soon after the last one", i)
	}
	assert.Positive(t, slept, "a failed recovery sleeps")
	assert.Len(t, fetcher.Seen(), attempts, "the expired token is re-polled once per recovery, not in a loop")
}
