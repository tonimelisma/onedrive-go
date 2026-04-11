package syncstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-6.10.5
func TestInspector_ReadStatusSnapshot(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES
		('last_sync_time', '2026-04-02T10:00:00Z'),
		('last_sync_duration_ms', '1500'),
		('last_sync_error', 'network timeout')`)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO baseline
		(path, drive_id, item_id, parent_id, item_type, synced_at)
		VALUES ('/a.txt', ?, 'item-1', 'root', 'file', 1)`, testDriveID)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-2', '/conflict.txt', 'edit_edit', 1, 'unresolved')`, testDriveID)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
		VALUES
		(?, 'item-3', '/pending.txt', 'root', 'file', 'pending_download', 1),
		(?, 'item-4', '/synced.txt', 'root', 'file', 'synced', 1)`,
		testDriveID, testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/retry.txt', ?, 'upload', 'upload', 'item', 'transient', 4, 1, 1),
		('/actionable.txt', ?, 'upload', 'upload', 'item', 'actionable', 1, 1, 1)`,
		testDriveID, testDriveID,
	)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, "2026-04-02T10:00:00Z", snapshot.SyncMetadata["last_sync_time"])
	assert.Equal(t, "1500", snapshot.SyncMetadata["last_sync_duration_ms"])
	assert.Equal(t, "network timeout", snapshot.SyncMetadata["last_sync_error"])
	assert.Equal(t, 1, snapshot.BaselineEntryCount)
	assert.Equal(t, 1, snapshot.PendingSyncItems)
	assert.Equal(t, 1, snapshot.Issues.ConflictCount())
	assert.Equal(t, 1, snapshot.Issues.ActionableCount())
	assert.Equal(t, 0, snapshot.Issues.RemoteBlockedCount())
	assert.Equal(t, 0, snapshot.Issues.AuthRequiredCount())
	assert.Equal(t, 2, snapshot.Issues.VisibleTotal())
	assert.Equal(t, 1, snapshot.Issues.RetryingCount())
	assert.Equal(t, []IssueGroupCount{
		{Key: synctypes.SummaryConflictUnresolved, Count: 1, ScopeKind: "file"},
		{Key: synctypes.SummarySyncFailure, Count: 1, ScopeKind: "file"},
	}, snapshot.Issues.Groups)
}

// Validates: R-2.3.6, R-2.3.12
func TestInspector_ReadStatusSnapshot_DurableIntentCounts(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO conflicts
			(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
			('conflict-pending', ?, 'item-1', '/pending.txt', 'edit_edit', 1, 'unresolved'),
			('conflict-resolving', ?, 'item-2', '/resolving.txt', 'edit_edit', 2, 'unresolved'),
			('conflict-failed', ?, 'item-3', '/failed.txt', 'edit_edit', 3, 'unresolved')`,
		testDriveID,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	require.NoError(t, store.UpsertHeldDeletes(ctx, []synctypes.HeldDeleteRecord{{
		DriveID:       driveid.New(testDriveID),
		ActionType:    synctypes.ActionRemoteDelete,
		Path:          "/delete-me.txt",
		ItemID:        "item-delete",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	pendingResult, err := store.RequestConflictResolution(ctx, "conflict-pending", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, pendingResult.Status)

	resolvingResult, err := store.RequestConflictResolution(ctx, "conflict-resolving", synctypes.ResolutionKeepRemote)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, resolvingResult.Status)
	_, ok, err := store.ClaimConflictResolution(ctx, "conflict-resolving")
	require.NoError(t, err)
	require.True(t, ok)

	failedResult, err := store.RequestConflictResolution(ctx, "conflict-failed", synctypes.ResolutionKeepBoth)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, failedResult.Status)
	_, ok, err = store.ClaimConflictResolution(ctx, "conflict-failed")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkConflictResolutionFailed(ctx, "conflict-failed", assert.AnError))

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, 1, snapshot.DurableIntents.PendingHeldDeleteApprovals)
	assert.Equal(t, 2, snapshot.DurableIntents.PendingConflictRequests)
	assert.Equal(t, 1, snapshot.DurableIntents.ApplyingConflictRequests)
}

// Validates: R-2.10.47
func TestInspector_HasScopeBlock(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()
	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, synctypes.IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	hasAuthScope, err := inspector.HasScopeBlock(ctx, synctypes.SKAuthAccount())
	require.NoError(t, err)
	assert.True(t, hasAuthScope)

	hasServiceScope, err := inspector.HasScopeBlock(ctx, synctypes.SKService())
	require.NoError(t, err)
	assert.False(t, hasServiceScope)
}

// Validates: R-2.4.4, R-2.4.5
// Validates: R-2.3.7, R-2.3.10, R-2.10.22
func TestInspector_ReadIssuesSnapshot(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES
		('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved'),
		('c2', ?, 'item-2', '/resolved.txt', 'edit_edit', 2, 'keep_local')`,
		testDriveID, testDriveID,
	)
	require.NoError(t, err)

	require.NoError(t, store.UpsertShortcut(ctx, &synctypes.Shortcut{
		ItemID:       "shortcut-1",
		RemoteDrive:  "Shared",
		RemoteItem:   "Docs",
		LocalPath:    "Team Docs",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1,
	}))

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, next_retry_at, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, '', 1, NULL, 1, 1),
		('/blocked/a.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 1),
		('/blocked/b.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 2),
		('/retry.txt', ?, 'upload', 'upload', 'item', 'transient', '', '', 4, ?, 1, 1)`,
		testDriveID, synctypes.IssueInvalidFilename,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).UnixNano(),
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO held_deletes
		(drive_id, action_type, path, item_id, state, held_at, last_planned_at, last_error)
		VALUES
		(?, 'remote_delete', '/delete/a.txt', 'item-a', 'held', 1, 1, 'held'),
		(?, 'remote_delete', '/delete/b.txt', 'item-b', 'held', 1, 2, 'held')`,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, synctypes.IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	current, err := inspector.ReadIssuesSnapshot(ctx, false)
	require.NoError(t, err)
	assert.Len(t, current.Conflicts, 1)
	assert.Equal(t, "/conflict.txt", current.Conflicts[0].Path)
	assert.Len(t, current.Groups, 3)
	assert.Equal(t, synctypes.SummarySharedFolderWritesBlocked, current.Groups[0].SummaryKey)
	assert.Equal(t, "Shared/Docs", current.Groups[0].ScopeLabel)
	assert.Equal(t, []string{"/blocked/a.txt", "/blocked/b.txt"}, current.Groups[0].Paths)
	assert.Len(t, current.HeldDeletes, 2)
	assert.Len(t, current.PendingRetries, 1)
	assert.Equal(t, 1, current.PendingRetries[0].Count)

	history, err := inspector.ReadIssuesSnapshot(ctx, true)
	require.NoError(t, err)
	assert.Len(t, history.Conflicts, 2)
}

// Validates: R-2.3.3, R-2.3.4, R-2.3.6
func TestInspector_ReadDetailedStatusSnapshot(t *testing.T) {
	t.Parallel()

	dbPath, ctx := seedDetailedStatusSnapshotFixture(t)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot, err := inspector.ReadDetailedStatusSnapshot(ctx, true)
	require.NoError(t, err)

	assert.Equal(t, "2026-04-10T12:00:00Z", snapshot.SyncMetadata["last_sync_time"])
	assert.Equal(t, 1, snapshot.PendingSyncItems)
	require.Len(t, snapshot.IssueGroups, 1)
	assert.Equal(t, synctypes.SummaryInvalidFilename, snapshot.IssueGroups[0].SummaryKey)

	assert.ElementsMatch(t, []DeleteSafetySnapshot{
		{Path: "/held-delete.txt", State: synctypes.HeldDeleteStateHeld, LastSeenAt: 10},
		{Path: "/approved-delete.txt", State: synctypes.HeldDeleteStateApproved, LastSeenAt: 20, ApprovedAt: 20},
	}, snapshot.DeleteSafety)

	require.Len(t, snapshot.Conflicts, 2)
	needsChoice := findConflictStatusSnapshot(t, snapshot.Conflicts, "conflict-needs-choice")
	assert.Equal(t, "/needs-choice.txt", needsChoice.Path)
	assert.Equal(t, "edit_edit", needsChoice.ConflictType)
	assert.Equal(t, int64(1), needsChoice.DetectedAt)
	assert.Empty(t, needsChoice.RequestedResolution)
	assert.Empty(t, needsChoice.RequestState)
	assert.Empty(t, needsChoice.LastRequestError)

	queuedConflict := findConflictStatusSnapshot(t, snapshot.Conflicts, "conflict-queued")
	assert.Equal(t, "/queued.txt", queuedConflict.Path)
	assert.Equal(t, "edit_delete", queuedConflict.ConflictType)
	assert.Equal(t, int64(2), queuedConflict.DetectedAt)
	assert.Equal(t, synctypes.ResolutionKeepLocal, queuedConflict.RequestedResolution)
	assert.Equal(t, synctypes.ConflictStateQueued, queuedConflict.RequestState)
	assert.Equal(t, assert.AnError.Error(), queuedConflict.LastRequestError)
	assert.NotZero(t, queuedConflict.LastRequestedAt)

	require.Len(t, snapshot.ConflictHistory, 1)
	assert.Equal(t, "/resolved.txt", snapshot.ConflictHistory[0].Path)
	assert.Equal(t, synctypes.ResolutionKeepBoth, snapshot.ConflictHistory[0].Resolution)
	assert.Equal(t, int64(30), snapshot.ConflictHistory[0].ResolvedAt)
	assert.Equal(t, synctypes.ResolvedByUser, snapshot.ConflictHistory[0].ResolvedBy)
}

func seedDetailedStatusSnapshotFixture(t *testing.T) (string, context.Context) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()
	seedDetailedStatusMetadata(t, store, ctx)
	seedDetailedStatusFailures(t, store, ctx)
	seedDetailedStatusDeleteSafety(t, store, ctx)
	seedDetailedStatusConflicts(t, store, ctx)

	return dbPath, ctx
}

func seedDetailedStatusMetadata(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES
		('last_sync_time', '2026-04-10T12:00:00Z'),
		('last_sync_duration_ms', '2000')`)
	require.NoError(t, err)
}

func seedDetailedStatusFailures(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, sync_status, observed_at)
		VALUES
		(?, 'pending-item', '/pending.txt', 'root', 'file', 'pending_download', 1),
		(?, 'synced-item', '/synced.txt', 'root', 'file', 'synced', 1)`,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, 1, 1, 1)`,
		testDriveID,
		synctypes.IssueInvalidFilename,
	)
	require.NoError(t, err)
}

func seedDetailedStatusDeleteSafety(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	require.NoError(t, store.UpsertHeldDeletes(ctx, []synctypes.HeldDeleteRecord{
		{
			DriveID:       driveid.New(testDriveID),
			ActionType:    synctypes.ActionRemoteDelete,
			Path:          "/held-delete.txt",
			ItemID:        "held-item",
			State:         synctypes.HeldDeleteStateHeld,
			HeldAt:        1,
			LastPlannedAt: 10,
		},
		{
			DriveID:       driveid.New(testDriveID),
			ActionType:    synctypes.ActionRemoteDelete,
			Path:          "/approved-delete.txt",
			ItemID:        "approved-item",
			State:         synctypes.HeldDeleteStateApproved,
			HeldAt:        2,
			ApprovedAt:    20,
			LastPlannedAt: 20,
		},
	}))
}

func seedDetailedStatusConflicts(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution, resolved_at, resolved_by)
		VALUES
		('conflict-needs-choice', ?, 'item-1', '/needs-choice.txt', 'edit_edit', 1, 'unresolved', NULL, NULL),
		('conflict-queued', ?, 'item-2', '/queued.txt', 'edit_delete', 2, 'unresolved', NULL, NULL),
		('conflict-resolved', ?, 'item-3', '/resolved.txt', 'edit_edit', 3, 'keep_both', 30, 'user')`,
		testDriveID,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	request, err := store.RequestConflictResolution(ctx, "conflict-queued", synctypes.ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, request.Status)

	_, ok, err := store.ClaimConflictResolution(ctx, "conflict-queued")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkConflictResolutionFailed(ctx, "conflict-queued", assert.AnError))
}

func findConflictStatusSnapshot(
	t *testing.T,
	conflicts []ConflictStatusSnapshot,
	id string,
) ConflictStatusSnapshot {
	t.Helper()

	for _, conflict := range conflicts {
		if conflict.ID == id {
			return conflict
		}
	}

	require.FailNowf(t, "missing conflict snapshot", "id=%s", id)
	return ConflictStatusSnapshot{}
}

// Validates: R-2.14.3, R-2.10.47
func TestInspector_ReadStatusSnapshot_StaysConsistentWithIssuesSnapshot(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-1', '/conflict.txt', 'edit_edit', 1, 'unresolved')`,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, next_retry_at, first_seen_at, last_seen_at)
		VALUES
		('/blocked/a.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 1),
		('/blocked/b.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 2),
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, '', 1, NULL, 1, 1)`,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, synctypes.IssueInvalidFilename,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, synctypes.IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	issues, err := inspector.ReadIssuesSnapshot(ctx, false)
	require.NoError(t, err)

	status := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, len(issues.Conflicts)+1+1+1, status.Issues.VisibleTotal())
	assert.Equal(t, 1, status.Issues.ConflictCount())
	assert.Equal(t, 1, status.Issues.ActionableCount())
	assert.Equal(t, 1, status.Issues.RemoteBlockedCount())
	assert.Equal(t, 1, status.Issues.AuthRequiredCount())
}

// Validates: R-2.14.3, R-2.10.47
func TestInspector_ReadStatusSnapshot_IssueSummary(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()

	_, err = store.DB().ExecContext(ctx, `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('c1', ?, 'item-2', '/conflict.txt', 'edit_edit', 1, 'unresolved')`, testDriveID)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/blocked/a.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, 1, 1),
		('/blocked/b.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, 1, 1),
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, '', 1, 1, 1),
		('/retry.txt', ?, 'upload', 'upload', 'item', 'transient', '', '', 4, 1, 1)`,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, synctypes.IssueSharedFolderBlocked,
		testDriveID, synctypes.IssueInvalidFilename,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, synctypes.IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, 4, snapshot.Issues.VisibleTotal())
	assert.Equal(t, 1, snapshot.Issues.ConflictCount())
	assert.Equal(t, 1, snapshot.Issues.ActionableCount())
	assert.Equal(t, 1, snapshot.Issues.RemoteBlockedCount())
	assert.Equal(t, 1, snapshot.Issues.AuthRequiredCount())
	assert.Equal(t, 1, snapshot.Issues.RetryingCount())
	assert.ElementsMatch(t, []IssueGroupCount{
		{Key: synctypes.SummaryConflictUnresolved, Count: 1, ScopeKind: "file"},
		{Key: synctypes.SummarySharedFolderWritesBlocked, Count: 1, ScopeKind: "shortcut", Scope: "Shared/Docs"},
		{Key: synctypes.SummaryInvalidFilename, Count: 1, ScopeKind: "file"},
		{Key: synctypes.SummaryAuthenticationRequired, Count: 1, ScopeKind: "account", Scope: "your OneDrive account authorization"},
	}, snapshot.Issues.Groups)
}
