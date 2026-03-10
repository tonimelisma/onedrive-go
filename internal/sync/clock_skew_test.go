package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// B-319: Clock skew resilience tests
// ---------------------------------------------------------------------------

// TestClockSkew_BackwardJump_BaselineSyncedAt verifies that baseline commits
// work correctly even when the clock jumps backward.
func TestClockSkew_BackwardJump_BaselineSyncedAt(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Commit at t=2000.
	mgr.nowFunc = func() time.Time { return time.Unix(2000, 0) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: ItemTypeFile, RemoteHash: "h1", Size: 100,
	}))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := bl.GetByPath("file.txt")
	require.True(t, ok)
	firstSyncedAt := entry.SyncedAt

	// Jump backward to t=1000 and update.
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: ItemTypeFile, RemoteHash: "h2", Size: 200,
	}))

	bl, err = mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok = bl.GetByPath("file.txt")
	require.True(t, ok)

	// SyncedAt moved backward — acceptable, no crash.
	assert.Less(t, entry.SyncedAt, firstSyncedAt,
		"synced_at should reflect the new (backward) clock — no crash")
}

// TestClockSkew_BackwardJump_SyncFailureTimestamp verifies that recording
// sync failures works correctly with backward clock jumps.
func TestClockSkew_BackwardJump_SyncFailureTimestamp(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Record at t=5000.
	mgr.nowFunc = func() time.Time { return time.Unix(5000, 0) }

	require.NoError(t, mgr.RecordSyncFailure(ctx, "file1.txt", driveid.ID{}, "upload", "upload_failed", "err", 500, 0, "", "", ""))

	// Jump backward to t=1000.
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	// Should still succeed.
	require.NoError(t, mgr.RecordSyncFailure(ctx, "file2.txt", driveid.ID{}, "upload", "upload_failed", "err", 500, 0, "", "", ""))

	issues, err := mgr.ListSyncFailuresByIssueType(ctx, "upload_failed")
	require.NoError(t, err)
	assert.Len(t, issues, 2, "both issues recorded despite clock jump")
}

// TestClockSkew_BackwardJump_ConflictDetectedAt verifies that conflict
// recording works correctly with backward clock jumps.
func TestClockSkew_BackwardJump_ConflictDetectedAt(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	driveID := driveid.New(engineTestDriveID)

	// Record conflict at t=1000 via CommitOutcome with a conflict action.
	mgr.nowFunc = func() time.Time { return time.Unix(1000, 0) }

	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Path:         "file.txt",
		DriveID:      driveID,
		ItemID:       "item-1",
		Action:       ActionConflict,
		ConflictType: ConflictEditEdit,
		Success:      true,
	}))

	// Jump clock backward to t=500.
	mgr.nowFunc = func() time.Time { return time.Unix(500, 0) }

	// Record another conflict.
	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Path:         "other.txt",
		DriveID:      driveID,
		ItemID:       "item-2",
		Action:       ActionConflict,
		ConflictType: ConflictEditEdit,
		Success:      true,
	}))

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	assert.Len(t, conflicts, 2, "both conflicts recorded despite clock jump")
}
