package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

	const unlisted = ActionCleanup + 1
	store := newTestStore(t)
	ctx := t.Context()
	mutation := &BaselineMutation{Action: unlisted, Path: "a.txt", ItemID: "item-a", DriveID: driveid.New("d")}

	_, err := unlisted.Value()
	require.ErrorContains(t, err, "invalid ActionType 10")
	_, err = classifyBaselineMutation(unlisted)
	require.ErrorContains(t, err, OutsideTheEnum(unlisted))
	_, err = PublicationMutationFromAction(&Action{Type: unlisted, Path: "a.txt"}, driveid.New("d"))
	require.ErrorContains(t, err, "ActionType(10) is not publication-only")
	require.ErrorContains(t, store.recordDeleteLedgerTx(ctx, store.RawDBForTest(), mutation), OutsideTheEnum(unlisted))
	require.ErrorContains(t, updateRemoteStateOnOutcome(ctx, store.RawDBForTest(), mutation), OutsideTheEnum(unlisted))

	outcome := (&WorkerPool{}).DispatchActionForTest(ctx, nil, &TrackedAction{Action: Action{Type: unlisted, Path: "a.txt"}})
	require.ErrorIs(t, outcome.Error, ErrUnknownActionType)
	require.ErrorContains(t, outcome.Error, OutsideTheEnum(unlisted))
	assert.False(t, outcome.Success)
}
