package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only quick_check findings or SQLITE_CORRUPT mean "damaged". Anything else
// (a canceled context, a busy or failing read) is an ordinary failure and must
// not steer the user to a reset.
//
// Validates: R-2.5.8
func TestIntegrityCheckFailure_OnlyDamageIsCorrupt(t *testing.T) {
	t.Parallel()

	damaged := fmt.Errorf("check: %w", &quickCheckFindingsError{findings: []string{"Tree 5 page 5: btreeInitPage() returns error code 11"}})
	resetErr := &StateStoreIncompatibleError{}
	require.ErrorAs(t, integrityCheckFailure(damaged), &resetErr)
	assert.Equal(t, StateStoreIncompatibleReasonCorrupt, resetErr.Reason)

	for name, cause := range map[string]error{
		"canceled":     context.Canceled,
		"read failure": errors.New("disk I/O error (10)"),
	} {
		err := integrityCheckFailure(fmt.Errorf("run sync store quick_check: %w", cause))
		require.Error(t, err, name)
		assert.False(t, IsStateStoreIncompatible(err), "%s is not a reason to reset", name)
		assert.ErrorIs(t, err, cause, name)
	}
}

// Validates: R-2.5.8
func TestCheckStoreIntegrity_CanceledCheckIsNotDamage(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := checkStoreIntegrity(ctx, store)
	require.Error(t, err)
	assert.False(t, IsStateStoreIncompatible(integrityCheckFailure(err)))
	require.NoError(t, checkStoreIntegrity(t.Context(), store), "a sound DB passes")
}
