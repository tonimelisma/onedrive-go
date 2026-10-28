package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func comparisonKindsByPath(rows []sQLiteComparisonRow) map[string]string {
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Path] = rows[i].ComparisonKind
	}

	return out
}

func reconciliationKindsByPath(rows []sQLiteReconciliationRow) map[string]string {
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
		INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES
			('upload.txt', 'file', 'new-local', 2, 2),
			('download.txt', 'file', 'same', 1, 1),
			('conflict.txt', 'file', 'local-conflict', 2, 2),
			('new-local.txt', 'file', 'local-create', 3, 3)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES
			('item-upload', 'upload.txt', 'file', 'old', 1, 1, 'etag-old'),
			('item-download', 'download.txt', 'file', 'remote-new', 2, 2, 'etag-new'),
			('item-conflict', 'conflict.txt', 'file', 'remote-conflict', 3, 3, 'etag-remote'),
			('item-redownload', 'redownload.txt', 'file', 'remote-redownload', 5, 5, 'etag-remote'),
			('item-new-remote', 'new-remote.txt', 'file', 'remote-create', 4, 4, 'etag-create')`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)

	assert.Equal(t, "upload", got["upload.txt"])
	assert.Equal(t, "download", got["download.txt"])
	assert.Equal(t, "conflict_edit_edit", got["conflict.txt"])
	assert.Equal(t, "baseline_remove", got["delete.txt"])
	assert.Equal(t, "download", got["redownload.txt"])
	assert.Equal(t, "upload", got["new-local.txt"])
	assert.Equal(t, "download", got["new-remote.txt"])
}

// Validates: R-2.1.3, R-2.1.4, R-2.17.2
func TestQueryReconciliationState_FolderDecisionMatrix(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type)
		VALUES
			('item-deleted-locally', 'deleted-locally', 'folder'),
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
			('item-deleted-locally', 'deleted-locally', 'folder'),
			('item-new-remote', 'new-remote-folder', 'folder')`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)

	// A folder the user deleted locally is deleted on OneDrive, exactly like
	// a file. It must never be re-created locally (that resurrected the source
	// tree of every unpaired local folder move and of every rm -rf).
	assert.Equal(t, "remote_delete", got["deleted-locally"])
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

// Validates: R-2.1.3, R-2.1.4
func TestQueryReconciliationState_LocalFolderMoveUsesFilesystemIdentity(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_device, local_inode, local_has_identity
		)
		VALUES ('item-folder', 'Projects', 'folder', 11, 22, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (
			path, item_type, local_device, local_inode, local_has_identity
		)
		VALUES ('Renamed Projects', 'folder', 11, 22, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type)
		VALUES ('item-folder', 'Projects', 'folder')`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	kinds := comparisonKindsByPath(comparisonRows)
	assert.Equal(t, "local_move_source", kinds["Projects"])
	assert.Equal(t, "local_move_dest", kinds["Renamed Projects"])

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)
	assert.Equal(t, "local_move", got["Projects"])
}

// Validates: R-2.17.1
func TestQueryReconciliationState_LocalFileMoveIdentityWithoutContentIsNotAMove(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES ('item-file', 'old.txt', 'file', 'same-hash', 'same-hash', 12, 12, 1, 1, 5, 6, 1)`)
	require.NoError(t, err)

	// The inode moved to a file with different content and size, and an exact
	// copy appeared under a different inode. Neither proves a move: the
	// identity match lacks content corroboration, and a baseline row that has
	// identity never falls back to hash pairing.
	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (
			path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity
		)
		VALUES
			('identity-target.txt', 'file', 'edited-hash', 20, 2, 5, 6, 1),
			('same-hash-copy.txt', 'file', 'same-hash', 12, 1, 70, 80, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES ('item-file', 'old.txt', 'file', 'same-hash', 12, 1)`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)
	assert.Equal(t, "remote_delete", got["old.txt"])
	assert.Equal(t, "upload", got["identity-target.txt"])
	assert.Equal(t, "upload", got["same-hash-copy.txt"])
}

// Validates: R-2.17.1
func TestQueryReconciliationState_LocalFileMoveRequiresContentCorroboration(t *testing.T) {
	t.Parallel()

	const (
		baseSize  = 5
		baseMtime = 100
	)

	tests := []struct {
		name     string
		hash     any
		size     int64
		mtime    int64
		wantMove bool
	}{
		{name: "rename keeps hash size and mtime", hash: "base-hash", size: baseSize, mtime: baseMtime, wantMove: true},
		{name: "touched rename keeps hash and size", hash: "base-hash", size: baseSize, mtime: baseMtime + 1, wantMove: true},
		{name: "unhashed rename keeps size and mtime", hash: nil, size: baseSize, mtime: baseMtime, wantMove: true},
		// The live repro: vim.swp deleted, secret.txt created unreadable in the
		// same batch, ext4 reused the inode. Same size, no hash, newer mtime.
		{name: "inode reuse by unreadable new file", hash: nil, size: baseSize, mtime: baseMtime + 7, wantMove: false},
		{name: "inode reuse with different content", hash: "other-hash", size: baseSize, mtime: baseMtime, wantMove: false},
		{name: "inode reuse with different size", hash: "base-hash", size: baseSize + 1, mtime: baseMtime, wantMove: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			ctx := t.Context()

			_, err := store.rawDB().ExecContext(ctx, `
				INSERT INTO baseline (
					item_id, path, item_type, local_hash, remote_hash,
					local_size, remote_size, local_mtime, remote_mtime,
					local_device, local_inode, local_has_identity
				)
				VALUES ('item-swap', 'vim.swp', 'file', 'base-hash', 'base-hash', ?, ?, ?, ?, 7, 8, 1)`,
				baseSize, baseSize, baseMtime, baseMtime)
			require.NoError(t, err)

			_, err = store.rawDB().ExecContext(ctx, `
				INSERT INTO local_state (
					path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity
				)
				VALUES ('secret.txt', 'file', ?, ?, ?, 7, 8, 1)`,
				tt.hash, tt.size, tt.mtime)
			require.NoError(t, err)

			_, err = store.rawDB().ExecContext(ctx, `
				INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
				VALUES ('item-swap', 'vim.swp', 'file', 'base-hash', ?, ?)`,
				baseSize, baseMtime)
			require.NoError(t, err)

			reconciliationRows, err := store.QueryReconciliationState(ctx)
			require.NoError(t, err)
			got := reconciliationKindsByPath(reconciliationRows)
			if tt.wantMove {
				assert.Equal(t, "local_move", got["vim.swp"])
				assert.Equal(t, "local_move", got["secret.txt"])
				return
			}
			assert.Equal(t, "remote_delete", got["vim.swp"])
			assert.Equal(t, "upload", got["secret.txt"])
		})
	}
}

// Validates: R-2.2.6
//
// Filesystem identity is a pairing hint, never a change signal (D-057). A new
// inode at the same path (rm -rf and restore, cp over the file, a device
// renumbered after reboot) is an edit only when the content says so; a folder
// with a new inode is the same folder. Either way the baseline learns the new
// identity. Before, every identity change read as local_changed: a recreated
// folder planned an "upload" of a directory, and a renumbered device
// re-uploaded every file as a new version.
func TestQueryReconciliationState_IdentityChangeAtSamePathIsNotAnEdit(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (
			item_id, path, item_type, local_hash, remote_hash,
			local_size, remote_size, local_mtime, remote_mtime,
			local_device, local_inode, local_has_identity
		)
		VALUES
			('item-same', 'same-content.txt', 'file', 'h', 'h', 12, 12, 1, 1, 5, 6, 1),
			('item-edit', 'edited.txt', 'file', 'h', 'h', 12, 12, 1, 1, 5, 7, 1),
			('item-unhashed', 'unhashed.txt', 'file', 'h', 'h', 12, 12, 1, 1, 5, 8, 1),
			('item-dir', 'restored-dir', 'folder', NULL, NULL, NULL, NULL, NULL, NULL, 5, 9, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (
			path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity
		)
		VALUES
			('same-content.txt', 'file', 'h', 12, 1, 50, 60, 1),
			('edited.txt', 'file', 'other', 12, 1, 50, 61, 1),
			('unhashed.txt', 'file', '', 12, 1, 50, 62, 1),
			('restored-dir', 'folder', NULL, NULL, NULL, 50, 63, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-same', 'same-content.txt', 'file', 'h', 12, 1),
			('item-edit', 'edited.txt', 'file', 'h', 12, 1),
			('item-unhashed', 'unhashed.txt', 'file', 'h', 12, 1),
			('item-dir', 'restored-dir', 'folder', NULL, NULL, NULL)`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	changed := map[string]bool{}
	for i := range comparisonRows {
		changed[comparisonRows[i].Path] = comparisonRows[i].LocalChanged
		assert.Empty(t, comparisonRows[i].LocalMoveTarget, "an identity change at one path is never a move")
	}
	assert.False(t, changed["same-content.txt"])
	assert.True(t, changed["edited.txt"])
	assert.True(t, changed["unhashed.txt"], "without a hash the content is unproven, so it is uploaded")
	assert.False(t, changed["restored-dir"])

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)
	assert.Equal(t, "baseline_update", got["same-content.txt"])
	assert.Equal(t, "upload", got["edited.txt"])
	assert.Equal(t, "upload", got["unhashed.txt"])
	assert.Equal(t, "baseline_update", got["restored-dir"])
}

// Validates: R-2.2.7
//
// Remote content facts OneDrive does not always supply (D-059b): a zero-byte
// file may carry no hash, and some saves drop the hash an item had. Neither is
// evidence of an edit or of a difference.
func TestQueryReconciliationState_MissingRemoteHashIsUnknownNotChanged(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, etag)
		VALUES
			('item-lost', 'lost-hash.txt', 'file', 'h', 'h', 7, 7, 1, 1, 'e1'),
			('item-resized', 'resized.txt', 'file', 'h', 'h', 7, 7, 1, 1, 'e1')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES
			('lost-hash.txt', 'file', 'h', 7, 1),
			('resized.txt', 'file', 'h', 7, 1),
			('empty.txt', 'file', 'AAAAAAAAAAAAAAAAAAAAAAAAAAA=', 0, 5)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime, etag)
		VALUES
			('item-lost', 'lost-hash.txt', 'file', NULL, 7, 9, 'e1'),
			('item-resized', 'resized.txt', 'file', NULL, 8, 9, 'e2'),
			('item-empty', 'empty.txt', 'file', NULL, 0, 6, 'e3')`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	remoteChanged := map[string]bool{}
	for i := range comparisonRows {
		remoteChanged[comparisonRows[i].Path] = comparisonRows[i].RemoteChanged
	}
	assert.False(t, remoteChanged["lost-hash.txt"], "a lost hash with the same size and eTag is not an edit")
	assert.True(t, remoteChanged["resized.txt"], "size still proves a change without a hash")

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)
	assert.NotEqual(t, "download", got["lost-hash.txt"])
	assert.Equal(t, "download", got["resized.txt"])
	assert.Equal(t, "baseline_update", got["empty.txt"], "two empty files are equal without a conflict copy")
}

// Validates: R-2.10.49, R-6.2.1
//
// A folder deleted locally while OneDrive deleted and re-created it (a new
// item ID at the same path) is not the folder the user deleted. Planned
// through remote_changed, which ignores folder content, it became a remote
// delete of the new item.
func TestQueryReconciliationState_LocallyDeletedFolderReplacedOnlineIsRecreatedLocally(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type)
		VALUES ('item-old', 'shared', 'folder')`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type)
		VALUES ('item-new', 'shared', 'folder')`)
	require.NoError(t, err)

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "folder_create_local", reconciliationKindsByPath(reconciliationRows)["shared"])
}
