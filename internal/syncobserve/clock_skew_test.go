package syncobserve

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// B-319: Clock skew resilience tests
// ---------------------------------------------------------------------------

// TestClockSkew_BackwardJump_BaselineSyncedAt verifies that baseline commits
// work correctly even when the clock jumps backward.
func TestClockSkew_BackwardJump_BaselineSyncedAt(t *testing.T) {
	t.Parallel()

	mgr := synctest.NewTestStore(t)
	ctx := t.Context()

	driveID := driveid.New(synctest.TestDriveID)

	// Commit at t=2000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(2000, 0) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: synctypes.ItemTypeFile, RemoteHash: "h1", Size: 100,
	}))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := bl.GetByPath("file.txt")
	require.True(t, ok)
	firstSyncedAt := entry.SyncedAt

	// Jump backward to t=1000 and update.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Action: synctypes.ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: synctypes.ItemTypeFile, RemoteHash: "h2", Size: 200,
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

	mgr := synctest.NewTestStore(t)
	ctx := t.Context()

	// Record at t=5000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })

	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "file1.txt",
		Direction:  "upload",
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	// Jump backward to t=1000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	// Should still succeed.
	require.NoError(t, mgr.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       "file2.txt",
		Direction:  "upload",
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	issues, err := mgr.ListSyncFailuresByIssueType(ctx, "upload_failed")
	require.NoError(t, err)
	assert.Len(t, issues, 2, "both issues recorded despite clock jump")
}

// TestClockSkew_BackwardJump_ConflictDetectedAt verifies that conflict
// recording works correctly with backward clock jumps.
func TestClockSkew_BackwardJump_ConflictDetectedAt(t *testing.T) {
	t.Parallel()

	mgr := synctest.NewTestStore(t)
	ctx := t.Context()

	driveID := driveid.New(synctest.TestDriveID)

	// Record conflict at t=1000 via CommitOutcome with a conflict action.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Path:         "file.txt",
		DriveID:      driveID,
		ItemID:       "item-1",
		Action:       synctypes.ActionConflict,
		ConflictType: synctypes.ConflictEditEdit,
		Success:      true,
	}))

	// Jump clock backward to t=500.
	mgr.SetNowFunc(func() time.Time { return time.Unix(500, 0) })

	// Record another conflict.
	require.NoError(t, mgr.CommitOutcome(ctx, &synctypes.Outcome{
		Path:         "other.txt",
		DriveID:      driveID,
		ItemID:       "item-2",
		Action:       synctypes.ActionConflict,
		ConflictType: synctypes.ConflictEditEdit,
		Success:      true,
	}))

	conflicts, err := mgr.ListConflicts(ctx)
	require.NoError(t, err)
	assert.Len(t, conflicts, 2, "both conflicts recorded despite clock jump")
}
