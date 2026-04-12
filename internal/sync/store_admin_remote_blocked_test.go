package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	testRemoteBlockedBoundaryTeamDocs = "Shared/TeamDocs"
)

func recordStoreRemoteBlockedFailure(
	t *testing.T,
	mgr *SyncStore,
	ctx context.Context,
	driveID driveid.ID,
	path string,
	boundary string,
	actionType ActionType,
) {
	t.Helper()

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       path,
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: actionType,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		ErrMsg:     "shared folder is read-only",
		IssueType:  IssueSharedFolderBlocked,
		ScopeKey:   SKPermRemote(boundary),
	}, nil))
}

// Validates: R-2.3.6
func TestSyncStore_ApproveHeldDeletes_MarksOnlyHeldDeletesApproved(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")

	require.NoError(t, mgr.UpsertHeldDeletes(ctx, []HeldDeleteRecord{{
		Path:          "delete/a.txt",
		DriveID:       driveID,
		ActionType:    ActionRemoteDelete,
		ItemID:        "item-a",
		State:         HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
		LastError:     "held delete",
	}}))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "bad:name.txt",
		DriveID:    driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleItem,
		Category:   CategoryActionable,
		IssueType:  IssueInvalidFilename,
		ErrMsg:     "invalid",
	}, nil))
	recordStoreRemoteBlockedFailure(t, mgr, ctx, driveID, "Shared/Docs/a.txt", "Shared/Docs", ActionUpload)

	require.NoError(t, mgr.ApproveHeldDeletes(ctx))

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"bad:name.txt", "Shared/Docs/a.txt"}, []string{rows[0].Path, rows[1].Path})

	held, err := mgr.ListHeldDeletesByState(ctx, HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, held)

	approved, err := mgr.ListHeldDeletesByState(ctx, HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)
	assert.Equal(t, "delete/a.txt", approved[0].Path)
}

func TestSyncStore_DropLegacyRemoteBlockedScope_RemovesOnlyLegacyAuthorityRows(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New("drive1")
	boundary := testRemoteBlockedBoundaryTeamDocs
	scopeKey := SKPermRemote(boundary)

	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          scopeKey,
		IssueType:    IssueSharedFolderBlocked,
		TimingSource: ScopeTimingNone,
		BlockedAt:    time.Date(2025, 4, 2, 10, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       boundary,
		DriveID:    driveID,
		Direction:  DirectionUpload,
		Role:       FailureRoleBoundary,
		Category:   CategoryActionable,
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "legacy boundary",
		ScopeKey:   scopeKey,
		ActionType: ActionFolderCreate,
	}, nil))
	recordStoreRemoteBlockedFailure(t, mgr, ctx, driveID, boundary+"/draft.txt", boundary, ActionUpload)

	require.NoError(t, mgr.DropLegacyRemoteBlockedScope(ctx, scopeKey))

	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, FailureRoleHeld, rows[0].Role)
	assert.Equal(t, scopeKey, rows[0].ScopeKey)
	assert.Equal(t, boundary+"/draft.txt", rows[0].Path)
}
