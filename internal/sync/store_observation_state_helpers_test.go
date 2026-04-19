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

	assert.Equal(t, remoteRefreshDegradedInterval, remoteRefreshIntervalForMode(remoteRefreshModeDeltaDegraded))
	assert.Equal(t, fullRemoteReconcileInterval, remoteRefreshIntervalForMode(remoteRefreshModeDeltaHealthy))
	assert.Equal(t, fullRemoteReconcileInterval, remoteRefreshIntervalForMode("unexpected"))
}

func TestConfiguredDriveIDForRead_UsesFallbackAndCache(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	got, err := store.configuredDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.True(t, got.IsZero())

	fallback := driveid.New(testDriveID)
	got, err = store.configuredDriveIDForRead(ctx, fallback)
	require.NoError(t, err)
	assert.Equal(t, fallback, got)

	got, err = store.configuredDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, fallback, got)
}

func TestConfiguredDriveIDForRead_ReadsPersistedValueFromDB(t *testing.T) {
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

	got, err := reopened.configuredDriveIDForRead(ctx, driveid.ID{})
	require.NoError(t, err)
	assert.Equal(t, driveID, got)
	assert.Equal(t, driveID, reopened.configuredDriveID())
}

func TestEnsureMatchingConfiguredDriveID_RejectsMismatch(t *testing.T) {
	t.Parallel()

	err := ensureMatchingConfiguredDriveID(driveid.New("attempted"), driveid.New("configured"))
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

func TestMarkFullRemoteReconcile_PreservesCurrentRemoteMode(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	first := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)
	second := first.Add(10 * time.Minute)

	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, first, remoteRefreshModeDeltaDegraded))
	require.NoError(t, store.MarkFullRemoteReconcile(ctx, driveID, second))

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, remoteRefreshModeDeltaDegraded, state.RemoteRefreshMode)
	assert.Equal(t, second.UnixNano(), state.LastFullRemoteRefreshAt)
	assert.Equal(t, second.Add(remoteRefreshDegradedInterval).UnixNano(), state.NextFullRemoteRefreshAt)
	assert.Equal(t, second.UnixNano(), state.LastFullRemoteReconcileAt)
}

func TestClearObservationCursor_PreservesRefreshSchedules(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	remoteAt := time.Date(2026, 4, 19, 8, 0, 0, 0, time.UTC)
	localAt := remoteAt.Add(15 * time.Minute)

	require.NoError(t, store.CommitObservationCursor(ctx, driveID, "token-before-clear"))
	require.NoError(t, store.MarkFullRemoteRefresh(ctx, driveID, remoteAt, "unexpected-remote-mode"))
	require.NoError(t, store.MarkFullLocalRefresh(ctx, driveID, localAt, "unexpected-local-mode"))
	require.NoError(t, store.ClearObservationCursor(ctx))

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Empty(t, state.Cursor)
	assert.Equal(t, remoteRefreshModeDeltaHealthy, state.RemoteRefreshMode)
	assert.Equal(t, remoteAt.UnixNano(), state.LastFullRemoteRefreshAt)
	assert.Equal(t, remoteAt.Add(fullRemoteReconcileInterval).UnixNano(), state.NextFullRemoteRefreshAt)
	assert.Equal(t, localRefreshModeWatchHealthy, state.LocalRefreshMode)
	assert.Equal(t, localAt.UnixNano(), state.LastFullLocalRefreshAt)
	assert.Equal(t, localAt.Add(localFullScanInterval).UnixNano(), state.NextFullLocalRefreshAt)
}
