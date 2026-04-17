package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.1.3
func TestReplacePlannedActions_ReplacesLatestGenerationOnly(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplacePlannedActions(ctx, "plan-a", []PlannedActionRow{
		{Path: "one.txt", ActionType: ActionUpload},
		{Path: "two.txt", ActionType: ActionDownload},
	}))

	require.NoError(t, store.ReplacePlannedActions(ctx, "plan-b", []PlannedActionRow{
		{Path: "final.txt", ActionType: ActionLocalDelete},
	}))

	rows, err := store.ListPlannedActions(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "plan-b", rows[0].PlanID)
	assert.Equal(t, "final.txt", rows[0].Path)
	assert.Equal(t, ActionLocalDelete, rows[0].ActionType)
}

// Validates: R-2.1.3, R-2.1.4
func TestMaterializePlannedActions_UsesSQLiteReconciliationOutput(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-conflict', 'conflict.txt', 'file', 'base', 'base', 1, 1, 1, 1, 'etag-base')`)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, hash, size, mtime, content_identity, observed_at)
		VALUES
			('upload.txt', 'file', 'new-local', 2, 2, 'new-local', 1),
			('conflict.txt', 'file', 'local-conflict', 2, 2, 'local-conflict', 1)`)
	require.NoError(t, err)

	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag, content_identity)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 1, 1, 'etag-old', 'old'),
			('item-conflict', 'conflict.txt', 'file', 'remote-conflict', 3, 3, 'etag-remote', 'remote-conflict')`)
	require.NoError(t, err)

	require.NoError(t, store.MaterializePlannedActions(ctx, "plan-sql"))

	rows, err := store.ListPlannedActions(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byPath := make(map[string]PlannedActionRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}

	assert.Equal(t, "plan-sql", byPath["upload.txt"].PlanID)
	assert.Equal(t, ActionUpload, byPath["upload.txt"].ActionType)

	assert.Equal(t, ActionConflict, byPath["conflict.txt"].ActionType)
	assert.Equal(t, ConflictEditEdit, byPath["conflict.txt"].SourceIdentity)
}
