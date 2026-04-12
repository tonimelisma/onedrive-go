package syncstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	testRemoteBlockedBoundaryTeamDocs = "Shared/TeamDocs"
)

func recordRemoteBlockedFailure(
	t *testing.T,
	mgr *SyncStore,
	ctx context.Context,
	driveID driveid.ID,
	path string,
	boundary string,
	actionType synctypes.ActionType,
) {
	t.Helper()

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       path,
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: actionType,
		Role:       synctypes.FailureRoleHeld,
		Category:   synctypes.CategoryTransient,
		ErrMsg:     "shared folder is read-only",
		IssueType:  synctypes.IssueSharedFolderBlocked,
		ScopeKey:   synctypes.SKPermRemote(boundary),
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
		ActionType:    synctypes.ActionRemoteDelete,
		ItemID:        "item-a",
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
		LastError:     "held delete",
	}}))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "bad:name.txt",
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		ActionType: synctypes.ActionUpload,
		Role:       synctypes.FailureRoleItem,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssueInvalidFilename,
		ErrMsg:     "invalid",
	}, nil))
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, "Shared/Docs/a.txt", "Shared/Docs", synctypes.ActionUpload)

	require.NoError(t, mgr.ApproveHeldDeletes(ctx))

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.ElementsMatch(t, []string{"bad:name.txt", "Shared/Docs/a.txt"}, []string{rows[0].Path, rows[1].Path})

	held, err := mgr.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, held)

	approved, err := mgr.ListHeldDeletesByState(ctx, synctypes.HeldDeleteStateApproved)
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
	scopeKey := synctypes.SKPermRemote(boundary)

	require.NoError(t, mgr.UpsertScopeBlock(ctx, &ScopeBlock{
		Key:          scopeKey,
		IssueType:    synctypes.IssueSharedFolderBlocked,
		TimingSource: synctypes.ScopeTimingNone,
		BlockedAt:    time.Date(2025, 4, 2, 10, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       boundary,
		DriveID:    driveID,
		Direction:  synctypes.DirectionUpload,
		Role:       synctypes.FailureRoleBoundary,
		Category:   synctypes.CategoryActionable,
		IssueType:  synctypes.IssuePermissionDenied,
		ErrMsg:     "legacy boundary",
		ScopeKey:   scopeKey,
		ActionType: synctypes.ActionFolderCreate,
	}, nil))
	recordRemoteBlockedFailure(t, mgr, ctx, driveID, boundary+"/draft.txt", boundary, synctypes.ActionUpload)

	require.NoError(t, mgr.DropLegacyRemoteBlockedScope(ctx, scopeKey))

	blocks, err := mgr.ListScopeBlocks(ctx)
	require.NoError(t, err)
	assert.Empty(t, blocks)

	rows, err := mgr.ListSyncFailures(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, synctypes.FailureRoleHeld, rows[0].Role)
	assert.Equal(t, scopeKey, rows[0].ScopeKey)
	assert.Equal(t, boundary+"/draft.txt", rows[0].Path)
}
