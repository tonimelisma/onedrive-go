package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func TestRemoteRefreshIntervalForMode_CoversDegradedAndDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, RemoteRefreshEnumerateInterval, remoteRefreshIntervalForMode(syncdomain.RemoteObservationModeEnumerate))
	assert.Equal(t, syncdomain.FullRemoteRefreshInterval, remoteRefreshIntervalForMode(syncdomain.RemoteObservationModeDelta))
	assert.Equal(t, syncdomain.FullRemoteRefreshInterval, remoteRefreshIntervalForMode("unexpected"))
}

func TestContentDriveIDForRead_UsesFallbackAndCache(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	got, err := store.contentDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	fallback := driveid.New(syncfixture.TestDriveID)
	got, err = store.contentDriveIDForRead(ctx, fallback)
	require.NoError(t, err)
	assert.Equal(t, fallback, got)

	got, err = store.contentDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, fallback, got)
}

func TestContentDriveIDForRead_ReadsPersistedValueFromDB(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)

	require.NoError(t, store.CommitObservationCursor(ctx, driveID, "token-from-db"))

	reopened, err := NewSyncStore(ctx, syncStorePathForStoreScopeTest(t, store), syncfixture.NewTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	got, err := reopened.contentDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, driveID, got)
	assert.Equal(t, driveID, reopened.contentDriveID())
}

func TestEnsureMatchingContentDriveID_RejectsMismatch(t *testing.T) {
	t.Parallel()

	err := ensureMatchingContentDriveID(driveid.New("attempted"), driveid.New("configured"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to a different OneDrive drive")
}

func TestCommitObservationCursor_RejectsMismatchedDriveID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.CommitObservationCursor(ctx, driveid.New("drive-a"), "token-a"))

	err := store.CommitObservationCursor(ctx, driveid.New("drive-b"), "token-b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to a different OneDrive drive")
}

func TestMarkFullRemoteRefresh_UpdatesNextDeadlineForLatestMode(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	first := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)
	second := first.Add(10 * time.Minute)

	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, first, syncdomain.RemoteObservationModeEnumerate))
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, second, syncdomain.RemoteObservationModeDelta))

	state := readObservationStateForTest(t, store, ctx)
	assert.Equal(t, testObservationState(driveID, "", second.Add(syncdomain.FullRemoteRefreshInterval).UnixNano()), state)
}

func TestClearObservationCursor_PreservesRefreshSchedules(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	remoteAt := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)

	require.NoError(t, store.CommitObservationCursor(ctx, driveID, "token-before-clear"))
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, remoteAt, syncdomain.RemoteObservationModeEnumerate))
	require.NoError(t, store.ClearObservationCursor(ctx))

	state := readObservationStateForTest(t, store, ctx)
	assert.Equal(t, testObservationState(driveID, "", remoteAt.Add(RemoteRefreshEnumerateInterval).UnixNano()), state)
}

func TestClampFullRemoteRefreshDeadline_ReturnsWhetherDeadlineMovedEarlier(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	now := time.Date(2026, 4, 20, 15, 0, 0, 0, time.UTC)

	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, now, syncdomain.RemoteObservationModeDelta))

	changed, err := store.ClampFullRemoteRefreshDeadline(ctx, driveID, now.Add(RemoteRefreshEnumerateInterval))
	require.NoError(t, err)
	assert.True(t, changed)

	changed, err = store.ClampFullRemoteRefreshDeadline(ctx, driveID, now.Add(2*RemoteRefreshEnumerateInterval))
	require.NoError(t, err)
	assert.False(t, changed)
}
