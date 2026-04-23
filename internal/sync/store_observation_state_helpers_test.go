package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestRemoteRefreshIntervalForMode_CoversDegradedAndDefault(t *testing.T) {
	t.Parallel()

	assert.Equal(t, remoteRefreshEnumerateInterval, remoteRefreshIntervalForMode(remoteObservationModeEnumerate))
	assert.Equal(t, fullRemoteRefreshInterval, remoteRefreshIntervalForMode(remoteObservationModeDelta))
	assert.Equal(t, fullRemoteRefreshInterval, remoteRefreshIntervalForMode("unexpected"))
}

func TestMountDriveIDForRead_UsesFallbackAndCache(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	got, err := store.mountDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	fallback := driveid.New(testDriveID)
	got, err = store.mountDriveIDForRead(ctx, fallback)
	require.NoError(t, err)
	assert.Equal(t, fallback, got)

	got, err = store.mountDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, fallback, got)
}

func TestMountDriveIDForRead_ReadsPersistedValueFromDB(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, store.CommitObservationCursor(ctx, driveID, "token-from-db"))

	reopened, err := NewSyncStore(ctx, syncStorePathForStoreScopeTest(t, store), newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	got, err := reopened.mountDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, driveID, got)
	assert.Equal(t, driveID, reopened.mountDriveID())
}

func TestEnsureMatchingMountDriveID_RejectsMismatch(t *testing.T) {
	t.Parallel()

	err := ensureMatchingMountDriveID(driveid.New("attempted"), driveid.New("configured"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state DB drive mismatch")
}

func TestCommitObservationCursor_RejectsMismatchedDriveID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.CommitObservationCursor(ctx, driveid.New("drive-a"), "token-a"))

	err := store.CommitObservationCursor(ctx, driveid.New("drive-b"), "token-b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state DB drive mismatch")
}

func TestMarkFullRemoteRefresh_UpdatesNextDeadlineForLatestMode(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	first := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)
	second := first.Add(10 * time.Minute)

	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, first, remoteObservationModeEnumerate))
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, second, remoteObservationModeDelta))

	state := readObservationStateForTest(t, store, ctx)
	assert.Equal(t, testObservationState(driveID, "", second.Add(fullRemoteRefreshInterval).UnixNano()), state)
}

func TestClearObservationCursor_PreservesRefreshSchedules(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	remoteAt := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)

	require.NoError(t, store.CommitObservationCursor(ctx, driveID, "token-before-clear"))
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, remoteAt, remoteObservationModeEnumerate))
	require.NoError(t, store.ClearObservationCursor(ctx))

	state := readObservationStateForTest(t, store, ctx)
	assert.Equal(t, testObservationState(driveID, "", remoteAt.Add(remoteRefreshEnumerateInterval).UnixNano()), state)
}

func TestClampFullRemoteRefreshDeadline_ReturnsWhetherDeadlineMovedEarlier(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	now := time.Date(2026, 4, 20, 15, 0, 0, 0, time.UTC)

	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, now, remoteObservationModeDelta))

	changed, err := store.ClampFullRemoteRefreshDeadline(ctx, driveID, now.Add(remoteRefreshEnumerateInterval))
	require.NoError(t, err)
	assert.True(t, changed)

	changed, err = store.ClampFullRemoteRefreshDeadline(ctx, driveID, now.Add(2*remoteRefreshEnumerateInterval))
	require.NoError(t, err)
	assert.False(t, changed)
}
