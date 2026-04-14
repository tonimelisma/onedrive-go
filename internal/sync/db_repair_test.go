package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.47
func TestRepairStateDB_RepairsReadableStoreInPlace(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "repair.db")
	logger := newTestLogger(t)
	store, err := NewSyncStore(t.Context(), dbPath, logger)
	require.NoError(t, err)

	ctx := t.Context()
	seedRepairIntegrityProblems(t, store, ctx, driveid.New(testDriveID))
	require.NoError(t, store.Close(context.Background()))

	result, err := RepairStateDB(ctx, dbPath, logger)
	require.NoError(t, err)
	assert.Equal(t, StateDBRepairRepair, result.Action)
	assert.Positive(t, result.RepairsApplied)

	reopened, err := NewSyncStore(ctx, dbPath, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	report, err := reopened.AuditIntegrity(ctx)
	require.NoError(t, err)
	assert.False(t, report.HasFindings())
}

// Validates: R-2.10.47
func TestRepairStateDB_ResetsWhenExistingDatabaseCannotBeSalvaged(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "broken.db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a sqlite database"), 0o600))

	result, err := RepairStateDB(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	assert.Equal(t, StateDBRepairRebuild, result.Action)

	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})

	report, err := store.AuditIntegrity(t.Context())
	require.NoError(t, err)
	assert.False(t, report.HasFindings())

	var baselineCount int
	err = store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM baseline`).Scan(&baselineCount)
	require.NoError(t, err)
	assert.Zero(t, baselineCount)
}
