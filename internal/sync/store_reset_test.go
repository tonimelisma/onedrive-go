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

// Validates: R-2.1.3
func TestResetStateDB_RecreatesFreshCanonicalStore(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := NewSyncStore(t.Context(), dbPath, newTestLogger(t))
	require.NoError(t, err)

	require.NoError(t, store.UpsertObservationIssue(t.Context(), &ObservationIssue{
		Path:       "bad:name.txt",
		DriveID:    driveid.New(testDriveID),
		ActionType: ActionUpload,
		IssueType:  IssueInvalidFilename,
		Error:      "invalid filename",
	}))
	require.NoError(t, store.UpsertBlockScope(t.Context(), &BlockScope{
		Key:          SKService(),
		IssueType:    IssueServiceOutage,
		TimingSource: ScopeTimingNone,
		BlockedAt:    store.nowFunc(),
	}))
	require.NoError(t, store.Close(context.Background()))

	require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("stale wal"), 0o600))
	require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("stale shm"), 0o600))

	require.NoError(t, ResetStateDB(t.Context(), dbPath, newTestLogger(t)))

	reopened, err := openSyncStore(t.Context(), dbPath, newTestLogger(t), false)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	issues, err := reopened.ListObservationIssues(t.Context())
	require.NoError(t, err)
	assert.Empty(t, issues)

	scopes, err := reopened.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes)

	_, err = os.Stat(dbPath)
	require.NoError(t, err)
}
