package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.2, R-2.10.1
func TestRefreshLocalBaseline_PreservesRemoteMetadataAndLeavesMirrorTruthUntouched(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	seedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return seedTime })

	seed := BaselineMutation{
		Action:          ActionDownload,
		Success:         true,
		Path:            "file.txt",
		DriveID:         driveid.New("d1"),
		ItemID:          "item-1",
		ItemType:        ItemTypeFile,
		LocalHash:       "local-old",
		RemoteHash:      "remote-old",
		LocalSize:       100,
		LocalSizeKnown:  true,
		RemoteSize:      120,
		RemoteSizeKnown: true,
		LocalMtime:      seedTime.UnixNano(),
		RemoteMtime:     seedTime.Add(2 * time.Second).UnixNano(),
		ETag:            "etag-old",
	}
	require.NoError(t, mgr.CommitMutation(ctx, &seed))

	_, err := mgr.DB().ExecContext(ctx,
		`INSERT INTO remote_state (item_id, path, item_type, hash, size, mtime)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"item-1", "file.txt", ItemTypeFile, "remote-old", 120,
		seedTime.Add(2*time.Second).UnixNano())
	require.NoError(t, err)

	refreshTime := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return refreshTime })

	require.NoError(t, mgr.RefreshLocalBaseline(ctx, LocalBaselineRefresh{
		Path:           "file.txt",
		DriveID:        driveid.New("d1"),
		ItemID:         "item-1",
		ItemType:       ItemTypeFile,
		LocalHash:      "local-new",
		LocalSize:      150,
		LocalSizeKnown: true,
		LocalMtime:     refreshTime.UnixNano(),
	}))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := bl.GetByPath("file.txt")
	require.True(t, ok)
	assert.Equal(t, "local-new", entry.LocalHash)
	assert.Equal(t, int64(150), entry.LocalSize)
	assert.Equal(t, refreshTime.UnixNano(), entry.LocalMtime)
	assert.Equal(t, "remote-old", entry.RemoteHash, "remote metadata should be preserved")
	assert.Equal(t, int64(120), entry.RemoteSize, "remote metadata should be preserved")
	assert.Equal(t, seedTime.Add(2*time.Second).UnixNano(), entry.RemoteMtime, "remote metadata should be preserved")
	assert.Equal(t, "etag-old", entry.ETag, "etag should be preserved")

	var hash string
	var size int64
	var mtime int64
	err = mgr.DB().QueryRowContext(ctx,
		`SELECT hash, size, mtime FROM remote_state WHERE item_id = ?`,
		"item-1",
	).Scan(&hash, &size, &mtime)
	require.NoError(t, err)
	assert.Equal(t, "remote-old", hash, "remote_state hash should not be overwritten")
	assert.Equal(t, int64(120), size, "remote_state size should not be overwritten")
	assert.Equal(t, seedTime.Add(2*time.Second).UnixNano(), mtime, "remote_state mtime should not be overwritten")
}

// Validates: R-2.2, R-2.10.1
func TestRefreshLocalBaseline_CreatesUnknownRemoteFieldsForLocalOnlyEntry(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()

	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	mgr.SetNowFunc(func() time.Time { return now })

	require.NoError(t, mgr.RefreshLocalBaseline(ctx, LocalBaselineRefresh{
		Path:           "copy.conflict-1.txt",
		DriveID:        driveid.New("d1"),
		ItemID:         "conflict-copy-placeholder",
		ItemType:       ItemTypeFile,
		LocalHash:      "local-only",
		LocalSize:      77,
		LocalSizeKnown: true,
		LocalMtime:     now.UnixNano(),
	}))

	bl, err := mgr.Load(ctx)
	require.NoError(t, err)

	entry, ok := bl.GetByPath("copy.conflict-1.txt")
	require.True(t, ok)
	assert.Equal(t, "local-only", entry.LocalHash)
	assert.Empty(t, entry.RemoteHash)
	assert.Zero(t, entry.RemoteSize)
	assert.False(t, entry.RemoteSizeKnown)
	assert.Zero(t, entry.RemoteMtime)
	assert.Empty(t, entry.ETag)
}
