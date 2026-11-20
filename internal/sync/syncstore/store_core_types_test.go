package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.2.16
//
// Where a decision on the action type already returns an error, a value
// outside the enum is refused with one: the row value, the mutation
// classification, the publication mutation, the delete ledger, the remote
// mirror and worker dispatch. The delete ledger used to record nothing and the
// remote mirror to change nothing, both read as "not mine"; the ledger checks
// membership before it asks DeleteSafetySideOf, which panics on such a value.
func TestActionTypeRefusals_ATypeOutsideTheEnumIsAnError(t *testing.T) {
	t.Parallel()

	const unlisted = syncdomain.ActionCleanup + 1
	store := newTestStore(t)
	ctx := t.Context()
	mutation := &syncdomain.BaselineMutation{Action: unlisted, Path: "a.txt", ItemID: "item-a", DriveID: driveid.New("d")}

	_, err := unlisted.Value()
	require.ErrorContains(t, err, "invalid ActionType 10")
	_, err = classifyBaselineMutation(unlisted)
	require.ErrorContains(t, err, syncdomain.OutsideTheEnum(unlisted))
	_, err = PublicationMutationFromAction(&syncdomain.Action{Type: unlisted, Path: "a.txt"}, driveid.New("d"))
	require.ErrorContains(t, err, "ActionType(10) is not publication-only")
	require.ErrorContains(t, store.recordDeleteLedgerTx(ctx, store.RawDBForTest(), mutation), syncdomain.OutsideTheEnum(unlisted))
	require.ErrorContains(t, updateRemoteStateOnOutcome(ctx, store.RawDBForTest(), mutation), syncdomain.OutsideTheEnum(unlisted))

	outcome := (&syncexec.WorkerPool{}).DispatchActionForTest(ctx, nil, &syncdomain.TrackedAction{Action: syncdomain.Action{Type: unlisted, Path: "a.txt"}})
	require.ErrorIs(t, outcome.Error, syncexec.ErrUnknownActionType)
	require.ErrorContains(t, outcome.Error, syncdomain.OutsideTheEnum(unlisted))
	assert.False(t, outcome.Success)
}
