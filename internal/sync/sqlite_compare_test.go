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

// Validates: R-2.17.8
//
// Move pairing asks whether the item left its baseline path (D-056). A second
// name for the same inode (a hard link) or a copy of an identity-less file
// leaves the source in place and is a new file; a rename followed by a new
// entry at the old name is a move plus a create.
func TestQueryReconciliationState_MovePairingRequiresTheSourceToBeVacated(t *testing.T) {
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
			('item-linked', 'linked.txt', 'file', 'h1', 'h1', 3, 3, 1, 1, 9, 1, 1),
			('item-copied', 'copied.txt', 'file', 'h2', 'h2', 3, 3, 1, 1, 0, 0, 0),
			('item-recreated', 'recreated.txt', 'file', 'h3', 'h3', 3, 3, 1, 1, 9, 3, 1),
			('item-dir', 'dir', 'folder', NULL, NULL, NULL, NULL, NULL, NULL, 9, 4, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity)
		VALUES
			('linked.txt', 'file', 'h1', 3, 1, 9, 1, 1),
			('hard-link.txt', 'file', 'h1', 3, 1, 9, 1, 1),
			('copied.txt', 'file', 'h2', 3, 1, 0, 0, 0),
			('copy.txt', 'file', 'h2', 3, 1, 0, 0, 0),
			('recreated.txt', 'file', 'fresh', 5, 2, 9, 30, 1),
			('renamed.txt', 'file', 'h3', 3, 1, 9, 3, 1),
			('dir', 'folder', NULL, NULL, NULL, 9, 40, 1),
			('renamed-dir', 'folder', NULL, NULL, NULL, 9, 4, 1)`)
	require.NoError(t, err)

	_, err = store.rawDB().ExecContext(ctx, `
		INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		VALUES
			('item-linked', 'linked.txt', 'file', 'h1', 3, 1),
			('item-copied', 'copied.txt', 'file', 'h2', 3, 1),
			('item-recreated', 'recreated.txt', 'file', 'h3', 3, 1),
			('item-dir', 'dir', 'folder', NULL, NULL, NULL)`)
	require.NoError(t, err)

	comparisonRows, err := store.QueryComparisonState(ctx)
	require.NoError(t, err)
	kinds := comparisonKindsByPath(comparisonRows)
	assert.Equal(t, "unchanged", kinds["linked.txt"], "a hard link leaves the synced name in place")
	assert.Equal(t, "local_only_create", kinds["hard-link.txt"])
	assert.Equal(t, "unchanged", kinds["copied.txt"])
	assert.Equal(t, "local_only_create", kinds["copy.txt"])
	assert.Equal(t, "local_move_source", kinds["recreated.txt"], "renamed away, then recreated")
	assert.Equal(t, "local_move_dest", kinds["renamed.txt"])
	assert.Equal(t, "local_move_source", kinds["dir"], "a folder renamed away with a new folder at its name")
	assert.Equal(t, "local_move_dest", kinds["renamed-dir"])

	reconciliationRows, err := store.QueryReconciliationState(ctx)
	require.NoError(t, err)
	got := reconciliationKindsByPath(reconciliationRows)
	assert.Equal(t, "upload", got["hard-link.txt"])
	assert.Equal(t, "upload", got["copy.txt"])
}

// Validates: R-2.17.1
//
// Move pairing compares birth times whenever both sides know one
// (sqlSameLocalEntry): the same (device, inode) with another birth time is a
// reused inode, a delete plus a create, for a folder and for a file whose
// content corroborates the pairing. A side without a birth time pairs on
// (device, inode) as before.
func TestQueryReconciliationState_LocalMovePairingComparesBirthTimes(t *testing.T) {
	t.Parallel()

	const (
		born   = int64(1_700_000_000_000_000_001)
		reborn = int64(1_700_000_000_000_000_002)
	)
	for _, itemType := range []string{strFolder, strFile} {
		for _, tc := range []struct {
			name          string
			baselineBirth int64
			localBirth    int64
			wantMove      bool
		}{
			{name: "same birth time", baselineBirth: born, localBirth: born, wantMove: true},
			{name: "reused inode", baselineBirth: born, localBirth: reborn},
			{name: "no birth time recorded", baselineBirth: 0, localBirth: reborn, wantMove: true},
			{name: "no birth time observed", baselineBirth: born, localBirth: 0, wantMove: true},
		} {
			t.Run(itemType+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				store := newTestStore(t)
				ctx := t.Context()
				var hash any
				if itemType == strFile {
					hash = "same-hash"
				}
				_, err := store.rawDB().ExecContext(ctx, `
					INSERT INTO baseline (
						item_id, path, item_type, local_hash, remote_hash,
						local_size, remote_size, local_mtime, remote_mtime,
						local_device, local_inode, local_birth_time, local_has_identity
					)
					VALUES ('item-old', 'old', ?, ?, ?, 4, 4, 1, 1, 5, 6, ?, 1)`,
					itemType, hash, hash, tc.baselineBirth)
				require.NoError(t, err)
				_, err = store.rawDB().ExecContext(ctx, `
					INSERT INTO local_state (
						path, item_type, hash, size, mtime, local_device, local_inode, local_birth_time, local_has_identity
					)
					VALUES ('new', ?, ?, 4, 1, 5, 6, ?, 1)`,
					itemType, hash, tc.localBirth)
				require.NoError(t, err)
				_, err = store.rawDB().ExecContext(ctx, `
					INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
					VALUES ('item-old', 'old', ?, ?, 4, 1)`,
					itemType, hash)
				require.NoError(t, err)

				reconciliationRows, err := store.QueryReconciliationState(ctx)
				require.NoError(t, err)
				got := reconciliationKindsByPath(reconciliationRows)
				if tc.wantMove {
					assert.Equal(t, "local_move", got["old"])
					assert.Equal(t, "local_move", got["new"])
					return
				}
				assert.Equal(t, "remote_delete", got["old"])
				if itemType == strFolder {
					assert.Equal(t, "folder_create_remote", got["new"])
				} else {
					assert.Equal(t, "upload", got["new"])
				}
			})
		}
	}
}

// Validates: R-2.17.1, R-2.2.6
//
// At the synced path, a reused inode with another birth time is a new entry
// (local_item_replaced), while a birth time the baseline was written without
// is learned through metadata drift with nothing transferred.
func TestQueryReconciliationState_BirthTimeAtTheSamePath(t *testing.T) {
	t.Parallel()

	const (
		born   = int64(1_700_000_000_000_000_001)
		reborn = int64(1_700_000_000_000_000_002)
	)
	for _, tc := range []struct {
		name          string
		baselineBirth int64
		localBirth    int64
		want          string
	}{
		{name: "same birth time", baselineBirth: born, localBirth: born, want: "noop"},
		{name: "birth time learned", baselineBirth: 0, localBirth: born, want: "baseline_update"},
		{name: "birth time not observed", baselineBirth: born, localBirth: 0, want: "noop"},
		// The same content in an entry born later: nothing to transfer, and
		// the baseline learns the new identity (D-057).
		{name: "reused inode with the synced content", baselineBirth: born, localBirth: reborn, want: "baseline_update"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			ctx := t.Context()
			_, err := store.rawDB().ExecContext(ctx, `
				INSERT INTO baseline (
					item_id, path, item_type, local_hash, remote_hash,
					local_size, remote_size, local_mtime, remote_mtime,
					local_device, local_inode, local_birth_time, local_has_identity
				)
				VALUES ('item-a', 'a.txt', 'file', 'h', 'h', 4, 4, 1, 1, 5, 6, ?, 1)`, tc.baselineBirth)
			require.NoError(t, err)
			_, err = store.rawDB().ExecContext(ctx, `
				INSERT INTO local_state (
					path, item_type, hash, size, mtime, local_device, local_inode, local_birth_time, local_has_identity
				)
				VALUES ('a.txt', 'file', 'h', 4, 1, 5, 6, ?, 1)`, tc.localBirth)
			require.NoError(t, err)
			_, err = store.rawDB().ExecContext(ctx, `
				INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
				VALUES ('item-a', 'a.txt', 'file', 'h', 4, 1)`)
			require.NoError(t, err)

			reconciliationRows, err := store.QueryReconciliationState(ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.want, reconciliationKindsByPath(reconciliationRows)["a.txt"])
		})
	}
}

// Validates: R-2.17.1
//
// The identity expressions of the comparison SQL are one pattern written out
// per local-row alias; this keeps them from drifting apart.
func TestSQLSameLocalEntry_EveryAliasFollowsThePattern(t *testing.T) {
	t.Parallel()

	pattern := func(local string) string {
		return "(" + local + ".local_device = b.local_device AND " + local + ".local_inode = b.local_inode" +
			" AND (" + local + ".local_birth_time = 0 OR b.local_birth_time = 0 OR " +
			local + ".local_birth_time = b.local_birth_time))"
	}
	assert.Equal(t, sqlSameLocalEntryL, pattern("l"))
	assert.Equal(t, sqlSameLocalEntryLs, pattern("ls"))
	assert.Equal(t, sqlSameLocalEntrySrc, pattern("src"))
}
