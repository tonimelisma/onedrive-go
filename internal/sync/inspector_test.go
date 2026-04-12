package sync

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
		(drive_id, item_id, path, parent_id, item_type, hash, observed_at)
		VALUES
		(?, 'item-3', '/pending.txt', 'root', 'file', 'hash-new', 1),
		(?, 'item-4', '/synced.txt', 'root', 'file', '', 1)`,
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
	assert.Equal(t, 3, snapshot.RemoteDriftItems)
	assert.Equal(t, 1, snapshot.Issues.ConflictCount())
	assert.Equal(t, 1, snapshot.Issues.ActionableCount())
	assert.Equal(t, 0, snapshot.Issues.RemoteBlockedCount())
	assert.Equal(t, 0, snapshot.Issues.AuthRequiredCount())
	assert.Equal(t, 2, snapshot.Issues.VisibleTotal())
	assert.Equal(t, 1, snapshot.Issues.RetryingCount())
	assert.Equal(t, []IssueGroupCount{
		{Key: SummaryConflictUnresolved, Count: 1, ScopeKind: "file"},
		{Key: SummarySyncFailure, Count: 1, ScopeKind: "file"},
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

	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		DriveID:       driveid.New(testDriveID),
		ActionType:    ActionRemoteDelete,
		Path:          "/delete-me.txt",
		ItemID:        "item-delete",
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
	}}))
	require.NoError(t, store.ApproveHeldDeletes(ctx))

	pendingResult, err := store.RequestConflictResolution(ctx, "conflict-pending", ResolutionKeepLocal)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, pendingResult.Status)

	resolvingResult, err := store.RequestConflictResolution(ctx, "conflict-resolving", ResolutionKeepRemote)
	require.NoError(t, err)
	assert.Equal(t, ConflictRequestQueued, resolvingResult.Status)
	_, ok, err := store.ClaimConflictResolution(ctx, "conflict-resolving")
	require.NoError(t, err)
	require.True(t, ok)

	failedResult, err := store.RequestConflictResolution(ctx, "conflict-failed", ResolutionKeepBoth)
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
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	hasAuthScope, err := inspector.HasScopeBlock(ctx, SKAuthAccount())
	require.NoError(t, err)
	assert.True(t, hasAuthScope)

	hasServiceScope, err := inspector.HasScopeBlock(ctx, SKService())
	require.NoError(t, err)
	assert.False(t, hasServiceScope)
}

// Validates: R-2.4.4, R-2.4.5
// Validates: R-2.3.7, R-2.3.10, R-2.10.22
func TestInspector_ReadGroupedIssueProjection(t *testing.T) {
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

	require.NoError(t, store.UpsertShortcut(ctx, &Shortcut{
		ItemID:       "shortcut-1",
		RemoteDrive:  "Shared",
		RemoteItem:   "Docs",
		LocalPath:    "Team Docs",
		Observation:  ObservationDelta,
		DiscoveredAt: 1,
	}))

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, scope_key, failure_count, next_retry_at, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, '', 1, NULL, 1, 1),
		('/blocked/a.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 1),
		('/blocked/b.txt', ?, 'upload', 'upload', 'held', 'transient', ?, 'perm:remote:Shared/Docs', 1, NULL, 1, 2),
		('/retry.txt', ?, 'upload', 'upload', 'item', 'transient', '', '', 4, ?, 1, 1)`,
		testDriveID, IssueInvalidFilename,
		testDriveID, IssueSharedFolderBlocked,
		testDriveID, IssueSharedFolderBlocked,
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
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	current, err := inspector.readGroupedIssueProjection(ctx, false)
	require.NoError(t, err)
	assert.Len(t, current.Conflicts, 1)
	assert.Equal(t, "/conflict.txt", current.Conflicts[0].Path)
	assert.Len(t, current.Groups, 3)
	assert.Equal(t, SummarySharedFolderWritesBlocked, current.Groups[0].SummaryKey)
	assert.Equal(t, "Shared/Docs", current.Groups[0].ScopeLabel)
	assert.Equal(t, []string{"/blocked/a.txt", "/blocked/b.txt"}, current.Groups[0].Paths)
	assert.Len(t, current.HeldDeletes, 2)
	assert.Len(t, current.PendingRetries, 1)
	assert.Equal(t, 1, current.PendingRetries[0].Count)

	history, err := inspector.readGroupedIssueProjection(ctx, true)
	require.NoError(t, err)
	assert.Len(t, history.Conflicts, 2)
}

// Validates: R-2.3.3, R-2.3.4, R-2.3.6
func TestInspector_ReadDriveStatusSnapshot(t *testing.T) {
	t.Parallel()

	dbPath, ctx := seedDriveStatusSnapshotFixture(t)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	snapshot, err := inspector.ReadDriveStatusSnapshot(ctx, true)
	require.NoError(t, err)

	assert.Equal(t, "2026-04-10T12:00:00Z", snapshot.SyncMetadata["last_sync_time"])
	assert.Equal(t, 2, snapshot.RemoteDriftItems)
	require.Len(t, snapshot.IssueGroups, 1)
	assert.Equal(t, SummaryInvalidFilename, snapshot.IssueGroups[0].SummaryKey)

	assert.ElementsMatch(t, []DeleteSafetySnapshot{
		{Path: "/held-delete.txt", State: HeldDeleteStateHeld, LastSeenAt: 10},
		{Path: "/approved-delete.txt", State: HeldDeleteStateApproved, LastSeenAt: 20, ApprovedAt: 20},
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
	assert.Equal(t, ResolutionKeepLocal, queuedConflict.RequestedResolution)
	assert.Equal(t, ConflictStateQueued, queuedConflict.RequestState)
	assert.Equal(t, assert.AnError.Error(), queuedConflict.LastRequestError)
	assert.NotZero(t, queuedConflict.LastRequestedAt)

	require.Len(t, snapshot.ConflictHistory, 1)
	assert.Equal(t, "/resolved.txt", snapshot.ConflictHistory[0].Path)
	assert.Equal(t, ResolutionKeepBoth, snapshot.ConflictHistory[0].Resolution)
	assert.Equal(t, int64(30), snapshot.ConflictHistory[0].ResolvedAt)
	assert.Equal(t, ResolvedByUser, snapshot.ConflictHistory[0].ResolvedBy)
}

func seedDriveStatusSnapshotFixture(t *testing.T) (string, context.Context) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	ctx := t.Context()
	seedDriveStatusMetadata(t, store, ctx)
	seedDriveStatusFailures(t, store, ctx)
	seedDriveStatusDeleteSafety(t, store, ctx)
	seedDriveStatusConflicts(t, store, ctx)

	return dbPath, ctx
}

func seedDriveStatusMetadata(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO sync_metadata (key, value) VALUES
		('last_sync_time', '2026-04-10T12:00:00Z'),
		('last_sync_duration_ms', '2000')`)
	require.NoError(t, err)
}

func seedDriveStatusFailures(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	_, err := store.DB().ExecContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, parent_id, item_type, hash, observed_at)
		VALUES
		(?, 'pending-item', '/pending.txt', 'root', 'file', 'hash-new', 1),
		(?, 'synced-item', '/synced.txt', 'root', 'file', '', 1)`,
		testDriveID,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO sync_failures
		(path, drive_id, direction, action_type, failure_role, category, issue_type, failure_count, first_seen_at, last_seen_at)
		VALUES
		('/invalid:name.txt', ?, 'upload', 'upload', 'item', 'actionable', ?, 1, 1, 1)`,
		testDriveID,
		IssueInvalidFilename,
	)
	require.NoError(t, err)
}

func seedDriveStatusDeleteSafety(t *testing.T, store *SyncStore, ctx context.Context) {
	t.Helper()

	require.NoError(t, store.UpsertHeldDeletes(ctx, []HeldDeleteRecord{
		{
			DriveID:       driveid.New(testDriveID),
			ActionType:    ActionRemoteDelete,
			Path:          "/held-delete.txt",
			ItemID:        "held-item",
			State:         HeldDeleteStateHeld,
			HeldAt:        1,
			LastPlannedAt: 10,
		},
		{
			DriveID:       driveid.New(testDriveID),
			ActionType:    ActionRemoteDelete,
			Path:          "/approved-delete.txt",
			ItemID:        "approved-item",
			State:         HeldDeleteStateApproved,
			HeldAt:        2,
			ApprovedAt:    20,
			LastPlannedAt: 20,
		},
	}))
}

func seedDriveStatusConflicts(t *testing.T, store *SyncStore, ctx context.Context) {
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

	request, err := store.RequestConflictResolution(ctx, "conflict-queued", ResolutionKeepLocal)
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
func TestInspector_ReadStatusSnapshot_StaysConsistentWithDriveStatusSnapshot(t *testing.T) {
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
		testDriveID, IssueSharedFolderBlocked,
		testDriveID, IssueSharedFolderBlocked,
		testDriveID, IssueInvalidFilename,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, IssueUnauthorized)
	require.NoError(t, err)

	inspector, err := OpenInspector(dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, inspector.Close())
	})

	driveStatus, err := inspector.ReadDriveStatusSnapshot(ctx, false)
	require.NoError(t, err)

	status := inspector.ReadStatusSnapshot(ctx)
	assert.Equal(t, len(driveStatus.Conflicts)+1+1+1, status.Issues.VisibleTotal())
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
		testDriveID, IssueSharedFolderBlocked,
		testDriveID, IssueSharedFolderBlocked,
		testDriveID, IssueInvalidFilename,
		testDriveID,
	)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES ('auth:account', ?, 'none', 1, 0, 0, 0, 0)`, IssueUnauthorized)
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
		{Key: SummaryConflictUnresolved, Count: 1, ScopeKind: "file"},
		{Key: SummarySharedFolderWritesBlocked, Count: 1, ScopeKind: "shortcut", Scope: "Shared/Docs"},
		{Key: SummaryInvalidFilename, Count: 1, ScopeKind: "file"},
		{Key: SummaryAuthenticationRequired, Count: 1, ScopeKind: "account", Scope: "your OneDrive account authorization"},
	}, snapshot.Issues.Groups)
}
