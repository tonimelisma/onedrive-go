package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// seedHashlessDivergence sets up a path whose local and remote copies hold
// different content while neither side can produce a content hash.
//
// Both halves are documented behavior of this codebase, not hypotheticals:
// the scanner persists an empty hash when hashing fails ("hash computation
// failed, emitting event with empty hash"), and driveops.SelectHash returns ""
// when Graph supplies no quickXor/sha256/sha1 (B-021).
func seedHashlessDivergence(t *testing.T, store *SyncStore, localHash, remoteHash string, remoteSize int64) {
	t.Helper()

	ctx := t.Context()
	driveID := driveid.New(engineTestDriveID)
	now := time.Now()

	require.NoError(t, store.CommitMutation(ctx, &BaselineMutation{
		Action: ActionDownload, Success: true,
		Path: "doc.txt", DriveID: driveID, ItemID: "item-1", ItemType: ItemTypeFile,
		LocalHash: "OLDLOCAL", RemoteHash: "OLDREMOTE",
		LocalSize: 100, LocalSizeKnown: true,
		RemoteSize: 100, RemoteSizeKnown: true,
		LocalMtime: now.UnixNano(), RemoteMtime: now.UnixNano(),
		ETag: "etag-1",
	}))

	require.NoError(t, store.ReplaceLocalState(ctx, []localStateRow{{
		Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: localHash, Size: 100, Mtime: now.Add(time.Hour).UnixNano(),
	}}))

	require.NoError(t, store.CommitObservation(ctx, []observedItem{{
		DriveID: driveID, ItemID: "item-1", Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: remoteHash, Size: remoteSize, Mtime: now.Add(2 * time.Hour).UnixNano(),
		ETag: "etag-2",
	}}, "token-2", driveID))
}

// Validates: R-2.3.1, R-6.2
//
// Equality between local and remote must be proven, never assumed. When
// neither side has a content hash, matching sizes say nothing about content,
// so the engine must not conclude the two copies agree. It previously did:
// ” = ” compared equal, size matched, and the only planned action was a
// baseline update recording agreement that did not exist — permanently and
// silently diverging the two copies.
func TestPlanCurrentState_HashlessSameSizeIsNotTreatedAsEqual(t *testing.T) {
	store := newTestStore(t)
	seedHashlessDivergence(t, store, "", "", 100)

	plan := planCurrentStateForStore(t, store)

	for i := range plan.Actions {
		assert.NotEqualf(t, ActionBaselineUpdate, plan.Actions[i].Type,
			"a baseline update would record agreement that was never proven")
	}
	assert.NotEmpty(t, plan.Actions,
		"unprovable equality must produce reconciliation work, not silence")
}

// Validates: R-2.3.1, R-6.2
//
// A hash on only one side is still no proof of equality.
func TestPlanCurrentState_HashOnOneSideOnlyIsNotTreatedAsEqual(t *testing.T) {
	store := newTestStore(t)
	seedHashlessDivergence(t, store, "LOCALHASH", "", 100)

	plan := planCurrentStateForStore(t, store)

	for i := range plan.Actions {
		assert.NotEqualf(t, ActionBaselineUpdate, plan.Actions[i].Type,
			"a one-sided hash cannot prove the copies agree")
	}
}

// Validates: R-2.3.1
//
// Genuine equality must still be recognized, or every sync would churn.
func TestPlanCurrentState_MatchingHashesStillCompareEqual(t *testing.T) {
	store := newTestStore(t)
	seedHashlessDivergence(t, store, "SAMEHASH", "SAMEHASH", 100)

	plan := planCurrentStateForStore(t, store)

	require.Len(t, plan.Actions, 1)
	assert.Equal(t, ActionBaselineUpdate, plan.Actions[0].Type,
		"proven-equal copies should converge to a baseline update")
}
