package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func comparisonKindsByPath(rows []SQLiteComparisonRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ComparisonKind
	}

	return out
}

func reconciliationKindsByPath(rows []SQLiteReconciliationRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ReconciliationKind
	}

	return out
}

// Validates: R-2.1.3, R-2.1.4
func TestQueryReconciliationState_BaselineAbsentFromBothRemovesBaseline(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	_, err := store.rawDB().ExecContext(t.Context(), `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime)
		VALUES ('item-1', 'gone.txt', 'file', 'hash-a', 'hash-a', 10, 10, 100, 100)`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "both_missing", comparisonKindsByPath(comparisonRows)["gone.txt"])

	reconciliationRows, err := store.QueryReconciliationState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "baseline_remove", reconciliationKindsByPath(reconciliationRows)["gone.txt"])
}

// Validates: R-2.1.3, R-2.1.4
func TestQueryReconciliationState_FileDecisionMatrix(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 'old', 1, 1, 1, 1, 'etag-old'),
			('item-download', 'download.txt', 'file', 'same', 'same', 1, 1, 1, 1, 'etag-old'),
			('item-conflict', 'conflict.txt', 'file', 'base', 'base', 1, 1, 1, 1, 'etag-base'),
			('item-delete', 'delete.txt', 'file', 'same', 'same', 1, 1, 1, 1, 'etag-same'),
			('item-redownload', 'redownload.txt', 'file', 'same', 'same', 1, 1, 1, 1, 'etag-same')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, hash, size, mtime, content_identity)
		VALUES
			('upload.txt', 'file', 'new-local', 2, 2, 'new-local'),
			('download.txt', 'file', 'same', 1, 1, 'same'),
			('conflict.txt', 'file', 'local-conflict', 2, 2, 'local-conflict'),
			('new-local.txt', 'file', 'local-create', 3, 3, 'local-create')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag, content_identity)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 1, 1, 'etag-old', 'old'),
			('item-download', 'download.txt', 'file', 'remote-new', 2, 2, 'etag-new', 'remote-new'),
			('item-conflict', 'conflict.txt', 'file', 'remote-conflict', 3, 3, 'etag-remote', 'remote-conflict'),
			('item-redownload', 'redownload.txt', 'file', 'remote-redownload', 5, 5, 'etag-remote', 'remote-redownload'),
			('item-new-remote', 'new-remote.txt', 'file', 'remote-create', 4, 4, 'etag-create', 'remote-create')`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)

	assert.Equal(t, "upload", got["upload.txt"])
	assert.Equal(t, "download", got["download.txt"])
	assert.Equal(t, "conflict_edit_edit", got["conflict.txt"])
	assert.Equal(t, "local_move", got["delete.txt"])
	assert.Equal(t, "download", got["redownload.txt"])
	assert.Equal(t, "upload", got["new-local.txt"])
	assert.Equal(t, "download", got["new-remote.txt"])
}

// Validates: R-2.1.3, R-2.1.4
func TestQueryReconciliationState_FolderDecisionMatrix(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type)
		VALUES
			('item-keep-remote', 'keep-remote', 'folder'),
			('item-delete-local', 'delete-local', 'folder')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type)
		VALUES
			('delete-local', 'folder'),
			('new-local-folder', 'folder')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type)
		VALUES
			('item-keep-remote', 'keep-remote', 'folder'),
			('item-new-remote', 'new-remote-folder', 'folder')`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)

	assert.Equal(t, "folder_create_local", got["keep-remote"])
	assert.Equal(t, "local_delete", got["delete-local"])
	assert.Equal(t, "folder_create_remote", got["new-local-folder"])
	assert.Equal(t, "folder_create_local", got["new-remote-folder"])
}

// Validates: R-2.1.3, R-2.1.4
func TestQueryReconciliationState_FolderMetadataChurnIsNoop(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_size, remote_size, local_mtime, remote_mtime, etag
		)
		VALUES
			('item-folder', 'stable-folder', 'folder', 0, 0, 100, 100, 'etag-old')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, size, mtime)
		VALUES ('stable-folder', 'folder', 4096, 200)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, size, mtime, etag)
		VALUES ('item-folder', 'stable-folder', 'folder', 1234, 300, 'etag-new')`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", comparisonKindsByPath(comparisonRows)["stable-folder"])

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "noop", reconciliationKindsByPath(reconciliationRows)["stable-folder"])
}
