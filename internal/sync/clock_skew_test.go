package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
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

	driveID := driveid.New(synctest.TestDriveID)

	// Commit at t=2000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(2000, 0) })

	require.NoError(t, mgr.CommitMutation(ctx, mutationFromActionOutcome(&ActionOutcome{
		Action: ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: ItemTypeFile, RemoteHash: "h1",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
	})))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := bl.GetByPath("file.txt")
	require.True(t, ok)
	firstSyncedAt := entry.SyncedAt

	// Jump backward to t=1000 and update.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	require.NoError(t, mgr.CommitMutation(ctx, mutationFromActionOutcome(&ActionOutcome{
		Action: ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveID, ItemID: "item-1", ParentID: "root",
		ItemType: ItemTypeFile, RemoteHash: "h2",
		LocalSize: 200, LocalSizeKnown: true,
		RemoteSize: 200, RemoteSizeKnown: true,
	})))

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
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "file1.txt",
		Direction:  DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	// Jump backward to t=1000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	// Should still succeed.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "file2.txt",
		Direction:  DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	issues, err := mgr.ListSyncFailuresByIssueType(ctx, "upload_failed")
	require.NoError(t, err)
	assert.Len(t, issues, 2, "both issues recorded despite clock jump")
}
