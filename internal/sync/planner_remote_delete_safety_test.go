package sync

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// A remote item that is absent locally is a remote delete only when a synced
// baseline proves the local copy existed. Without that baseline, local absence
// means the item was never downloaded, so it must be downloaded instead.
//
// Validates: R-6.2.1
func TestPlannerPlanCurrentState_LocalAbsenceDeletesRemoteOnlyWithBaseline(t *testing.T) {
	t.Parallel()

	t.Run("no baseline downloads instead of deleting", func(t *testing.T) {
		t.Parallel()

		store := newTestStore(t)
		ctx := t.Context()
		driveID := driveid.New(engineTestDriveID)

		require.NoError(t, store.CommitObservation(ctx, []observedItem{{
			DriveID:  driveID,
			ItemID:   "item-remote-only",
			Path:     "remote-only.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
			Size:     10,
			Mtime:    1,
			ETag:     "etag-remote-only",
		}}, "", driveID))
		require.NoError(t, store.ReplaceLocalState(ctx, nil))

		plan := planCurrentStateForStore(t, store)
		assertNoActionForPath(t, plan, "remote-only.txt", ActionRemoteDelete)
		assertPlanHasAction(t, plan, "remote-only.txt", ActionDownload)
	})

	t.Run("synced baseline deletes the remote copy", func(t *testing.T) {
		t.Parallel()

		store := newTestStore(t)
		ctx := t.Context()
		driveID := driveid.New(engineTestDriveID)

		_, err := store.rawDB().ExecContext(ctx, `
			INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
			VALUES ('item-synced', 'synced.txt', 'file', 'hash', 'hash', 10, 10, 1, 1, 'etag-synced')`)
		require.NoError(t, err)
		require.NoError(t, store.CommitObservation(ctx, []observedItem{{
			DriveID:  driveID,
			ItemID:   "item-synced",
			Path:     "synced.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
			Size:     10,
			Mtime:    1,
			ETag:     "etag-synced",
		}}, "", driveID))
		require.NoError(t, store.ReplaceLocalState(ctx, nil))

		plan := planCurrentStateForStore(t, store)
		assertPlanHasAction(t, plan, "synced.txt", ActionRemoteDelete)
	})
}

func assertPlanHasAction(t *testing.T, plan *actionPlan, path string, actionType actionType) {
	t.Helper()

	for i := range plan.Actions {
		if plan.Actions[i].Path == path && plan.Actions[i].Type == actionType {
			return
		}
	}

	require.Failf(t, "missing action", "want %s for %s in %d planned actions", actionType, path, len(plan.Actions))
}
