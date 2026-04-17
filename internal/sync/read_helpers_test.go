package sync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.47
func TestHasScopeBlockAtPath(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO scope_blocks
		(scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count)
		VALUES (?, ?, 'none', 1, 0, 0, 0, 0)`, SKService().String(), IssueServiceOutage)
	require.NoError(t, err)

	hasServiceScope, err := HasScopeBlockAtPath(t.Context(), dbPath, SKService(), newTestLogger(t))
	require.NoError(t, err)
	assert.True(t, hasServiceScope)

	hasQuotaScope, err := HasScopeBlockAtPath(t.Context(), dbPath, SKQuotaOwn(), newTestLogger(t))
	require.NoError(t, err)
	assert.False(t, hasQuotaScope)
}

func TestFinalizeInspectorRead_PreservesSuccessfulReadOnCloseError(t *testing.T) {
	t.Parallel()

	result, err := finalizeInspectorRead("state.db", newTestLogger(t), true, nil, errors.New("close failed"))
	require.NoError(t, err)
	assert.True(t, result)
}

func TestFinalizeInspectorRead_JoinsReadAndCloseErrors(t *testing.T) {
	t.Parallel()

	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")

	_, err := finalizeInspectorRead("state.db", newTestLogger(t), false, readErr, closeErr)
	require.Error(t, err)
	require.ErrorIs(t, err, readErr)
	require.ErrorIs(t, err, closeErr)
}
