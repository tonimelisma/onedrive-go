package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-6.6.28, R-2.10.47
//
// A status snapshot is one moment of the store: an issue and a retry row a
// live engine commits together between two of the snapshot's reads are seen
// together or not at all. Read query by query, the snapshot would show the
// retry row without the issue committed with it.
func TestReadDriveStatusSnapshot_AWriteBetweenItsReadsIsNotSeenHalf(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	require.NoError(t, os.WriteFile(dbPath, loadTestStoreTemplate(t), 0o600))
	store, err := openSyncStore(synctest.TestContext(t), dbPath, synctest.TestLogger(t), false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.Background())) })

	inspector, err := openStoreInspector(dbPath, synctest.TestLogger(t))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, inspector.Close()) })

	var writeErr error
	inspector.betweenSnapshotReads = func() {
		writeErr = commitIssueAndRetryRowTogether(t.Context(), store)
	}
	snapshot, err := inspector.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, writeErr)
	assert.Empty(t, snapshot.ObservationIssues, "committed after the snapshot began")
	assert.Empty(t, snapshot.RetryingWork, "the retry row committed with the issue is not seen without it")

	inspector.betweenSnapshotReads = nil
	snapshot, err = inspector.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.Len(t, snapshot.ObservationIssues, 1, "the next snapshot sees both")
	assert.Len(t, snapshot.RetryingWork, 1)
}

// commitIssueAndRetryRowTogether writes an observation issue and a retrying
// retry row in one transaction, as one engine commit.
func commitIssueAndRetryRowTogether(ctx context.Context, store *SyncStore) (err error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "rollback")
	}()

	if _, err = tx.ExecContext(ctx,
		`INSERT INTO observation_issues (path, issue_type, scope_key) VALUES (?, ?, '')`,
		"CON", string(IssueInvalidFilename),
	); err != nil {
		return fmt.Errorf("insert issue: %w", err)
	}
	if err = upsertRetryWorkTx(ctx, tx, &RetryWorkRow{
		Path:         "busy.txt",
		ActionType:   ActionUpload,
		AttemptCount: unresolvedTestRetryingAttempts,
	}); err != nil {
		return fmt.Errorf("insert retry row: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}
