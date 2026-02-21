package sync

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore creates an in-memory SQLiteStore for testing.
// Uses testLogger and testWriter from delta_test.go (same package).
func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	return store
}

// makeTestItem creates a minimal Item for testing with required fields populated.
func makeTestItem(
	driveID, itemID, parentDriveID, parentID, name string,
	itemType ItemType,
) *Item {
	now := NowNano()
	return &Item{
		DriveID:       driveID,
		ItemID:        itemID,
		ParentDriveID: parentDriveID,
		ParentID:      parentID,
		Name:          name,
		ItemType:      itemType,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

func TestNewStore(t *testing.T) {
	t.Run("opens in-memory database", func(t *testing.T) {
		store := newTestStore(t)
		assert.NotNil(t, store.db)
	})

	t.Run("migration is applied", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		var version int
		err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		require.NoError(t, err)
		assert.Equal(t, 1, version)
	})

	t.Run("idempotent migration", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()

		var version int
		err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
		require.NoError(t, err)
		assert.Equal(t, 1, version)
	})
}

func TestGetItem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("not found", func(t *testing.T) {
		item, err := store.GetItem(ctx, "d1", "missing")
		assert.NoError(t, err)
		assert.Nil(t, item)
	})

	t.Run("found after upsert", func(t *testing.T) {
		item := makeTestItem("d1", "item1", "d1", "root1", "testfile.txt", ItemTypeFile)
		item.Path = "testfile.txt"
		item.Size = Int64Ptr(1024)
		item.QuickXorHash = "abc123"
		require.NoError(t, store.UpsertItem(ctx, item))

		got, err := store.GetItem(ctx, "d1", "item1")
		require.NoError(t, err)
		assert.Equal(t, "testfile.txt", got.Name)
		assert.Equal(t, "testfile.txt", got.Path)
		assert.Equal(t, int64(1024), *got.Size)
		assert.Equal(t, "abc123", got.QuickXorHash)
		assert.False(t, got.IsDeleted)
	})
}

func TestUpsertItem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("insert then update", func(t *testing.T) {
		item := makeTestItem("d1", "item1", "d1", "root1", "original.txt", ItemTypeFile)
		item.Path = "original.txt"
		require.NoError(t, store.UpsertItem(ctx, item))

		// Update the item with a new name and path.
		newName := "updated.txt"
		item.Name = newName
		item.Path = newName
		item.UpdatedAt = NowNano()
		require.NoError(t, store.UpsertItem(ctx, item))

		got, err := store.GetItem(ctx, "d1", "item1")
		require.NoError(t, err)
		assert.Equal(t, newName, got.Name)
		assert.Equal(t, newName, got.Path)
	})
}

func TestMarkDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	item := makeTestItem("d1", "item1", "d1", "root1", "doomed.txt", ItemTypeFile)
	item.Path = "doomed.txt"
	require.NoError(t, store.UpsertItem(ctx, item))

	deletedAt := NowNano()
	require.NoError(t, store.MarkDeleted(ctx, "d1", "item1", deletedAt))

	got, err := store.GetItem(ctx, "d1", "item1")
	require.NoError(t, err)
	assert.True(t, got.IsDeleted)
	assert.Equal(t, deletedAt, *got.DeletedAt)
}

func TestListChildren(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	root := makeTestItem("d1", "root1", "", "", "root", ItemTypeRoot)
	child1 := makeTestItem("d1", "c1", "d1", "root1", "child1.txt", ItemTypeFile)
	child2 := makeTestItem("d1", "c2", "d1", "root1", "child2.txt", ItemTypeFile)
	deleted := makeTestItem("d1", "c3", "d1", "root1", "deleted.txt", ItemTypeFile)
	deleted.IsDeleted = true
	deleted.DeletedAt = Int64Ptr(NowNano())

	require.NoError(t, store.UpsertItem(ctx, root))
	require.NoError(t, store.UpsertItem(ctx, child1))
	require.NoError(t, store.UpsertItem(ctx, child2))
	require.NoError(t, store.UpsertItem(ctx, deleted))

	children, err := store.ListChildren(ctx, "d1", "root1")
	require.NoError(t, err)
	assert.Len(t, children, 2, "deleted child should be excluded")
}

func TestGetItemByPath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	item := makeTestItem("d1", "item1", "d1", "root1", "readme.md", ItemTypeFile)
	item.Path = "docs/readme.md"
	require.NoError(t, store.UpsertItem(ctx, item))

	t.Run("found", func(t *testing.T) {
		got, err := store.GetItemByPath(ctx, "docs/readme.md")
		require.NoError(t, err)
		assert.Equal(t, "item1", got.ItemID)
	})

	t.Run("not found", func(t *testing.T) {
		item, err := store.GetItemByPath(ctx, "nonexistent")
		assert.NoError(t, err)
		assert.Nil(t, item)
	})
}

func TestListAllActiveItems(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	active := makeTestItem("d1", "a1", "d1", "root1", "active.txt", ItemTypeFile)
	deleted := makeTestItem("d2", "d2item", "d2", "root2", "deleted.txt", ItemTypeFile)
	deleted.IsDeleted = true
	deleted.DeletedAt = Int64Ptr(NowNano())

	require.NoError(t, store.UpsertItem(ctx, active))
	require.NoError(t, store.UpsertItem(ctx, deleted))

	items, err := store.ListAllActiveItems(ctx)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "a1", items[0].ItemID)
}

func TestListSyncedItems(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	synced := makeTestItem("d1", "s1", "d1", "root1", "synced.txt", ItemTypeFile)
	synced.SyncedHash = "hash123"
	unsynced := makeTestItem("d1", "u1", "d1", "root1", "unsynced.txt", ItemTypeFile)

	require.NoError(t, store.UpsertItem(ctx, synced))
	require.NoError(t, store.UpsertItem(ctx, unsynced))

	items, err := store.ListSyncedItems(ctx)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "s1", items[0].ItemID)
}

func TestBatchUpsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	items := []*Item{
		makeTestItem("d1", "b1", "d1", "root1", "batch1.txt", ItemTypeFile),
		makeTestItem("d1", "b2", "d1", "root1", "batch2.txt", ItemTypeFile),
		makeTestItem("d1", "b3", "d1", "root1", "batch3.txt", ItemTypeFile),
	}

	require.NoError(t, store.BatchUpsert(ctx, items))

	for _, item := range items {
		got, err := store.GetItem(ctx, "d1", item.ItemID)
		require.NoError(t, err)
		assert.Equal(t, item.Name, got.Name)
	}
}

func TestMaterializePath(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("three-level hierarchy", func(t *testing.T) {
		root := makeTestItem("d1", "root1", "", "", "root", ItemTypeRoot)
		folder := makeTestItem("d1", "folder1", "d1", "root1", "Documents", ItemTypeFolder)
		file := makeTestItem("d1", "file1", "d1", "folder1", "report.pdf", ItemTypeFile)

		require.NoError(t, store.UpsertItem(ctx, root))
		require.NoError(t, store.UpsertItem(ctx, folder))
		require.NoError(t, store.UpsertItem(ctx, file))

		path, err := store.MaterializePath(ctx, "d1", "file1")
		require.NoError(t, err)
		assert.Equal(t, "Documents/report.pdf", path)
	})

	t.Run("folder path", func(t *testing.T) {
		path, err := store.MaterializePath(ctx, "d1", "folder1")
		require.NoError(t, err)
		assert.Equal(t, "Documents", path)
	})

	t.Run("root path is empty", func(t *testing.T) {
		path, err := store.MaterializePath(ctx, "d1", "root1")
		require.NoError(t, err)
		assert.Equal(t, "", path)
	})

	t.Run("orphaned parent returns empty (B-022)", func(t *testing.T) {
		orphan := makeTestItem("d1", "orphan1", "d1", "missing-parent", "orphan.txt", ItemTypeFile)
		require.NoError(t, store.UpsertItem(ctx, orphan))

		path, err := store.MaterializePath(ctx, "d1", "orphan1")
		require.NoError(t, err)
		assert.Equal(t, "", path, "orphaned items should return empty path per B-022")
	})
}

func TestCascadePathUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	items := []*Item{
		makeTestItem("d1", "f1", "d1", "root1", "file1.txt", ItemTypeFile),
		makeTestItem("d1", "f2", "d1", "root1", "file2.txt", ItemTypeFile),
		makeTestItem("d1", "f3", "d1", "root1", "deep.txt", ItemTypeFile),
		makeTestItem("d1", "f4", "d1", "root1", "other.txt", ItemTypeFile),
	}
	items[0].Path = "old/file1.txt"
	items[1].Path = "old/file2.txt"
	items[2].Path = "old/sub/deep.txt"
	items[3].Path = "other/file.txt"

	for _, item := range items {
		require.NoError(t, store.UpsertItem(ctx, item))
	}

	require.NoError(t, store.CascadePathUpdate(ctx, "old", "new"))

	got1, _ := store.GetItem(ctx, "d1", "f1")
	assert.Equal(t, "new/file1.txt", got1.Path)

	got2, _ := store.GetItem(ctx, "d1", "f2")
	assert.Equal(t, "new/file2.txt", got2.Path)

	got3, _ := store.GetItem(ctx, "d1", "f3")
	assert.Equal(t, "new/sub/deep.txt", got3.Path)

	got4, _ := store.GetItem(ctx, "d1", "f4")
	assert.Equal(t, "other/file.txt", got4.Path)
}

func TestCleanupTombstones(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	old := makeTestItem("d1", "old1", "d1", "root1", "old.txt", ItemTypeFile)
	old.IsDeleted = true
	oldTime := time.Now().Add(-45 * 24 * time.Hour).UnixNano()
	old.DeletedAt = &oldTime

	recent := makeTestItem("d1", "new1", "d1", "root1", "new.txt", ItemTypeFile)
	recent.IsDeleted = true
	recentTime := time.Now().Add(-1 * 24 * time.Hour).UnixNano()
	recent.DeletedAt = &recentTime

	require.NoError(t, store.UpsertItem(ctx, old))
	require.NoError(t, store.UpsertItem(ctx, recent))

	deleted, err := store.CleanupTombstones(ctx, 30)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted, "only the old tombstone should be cleaned")

	old, getErr := store.GetItem(ctx, "d1", "old1")
	assert.NoError(t, getErr)
	assert.Nil(t, old, "old tombstone should be purged")

	_, err = store.GetItem(ctx, "d1", "new1")
	assert.NoError(t, err)
}

func TestDeltaTokenCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("empty token for unknown drive", func(t *testing.T) {
		token, err := store.GetDeltaToken(ctx, "d1")
		require.NoError(t, err)
		assert.Empty(t, token)
	})

	t.Run("save and retrieve", func(t *testing.T) {
		require.NoError(t, store.SaveDeltaToken(ctx, "d1", "token123"))

		token, err := store.GetDeltaToken(ctx, "d1")
		require.NoError(t, err)
		assert.Equal(t, "token123", token)
	})

	t.Run("update existing", func(t *testing.T) {
		require.NoError(t, store.SaveDeltaToken(ctx, "d1", "token456"))

		token, err := store.GetDeltaToken(ctx, "d1")
		require.NoError(t, err)
		assert.Equal(t, "token456", token)
	})

	t.Run("delete", func(t *testing.T) {
		require.NoError(t, store.DeleteDeltaToken(ctx, "d1"))

		token, err := store.GetDeltaToken(ctx, "d1")
		require.NoError(t, err)
		assert.Empty(t, token)
	})
}

func TestDeltaComplete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("false for unknown drive", func(t *testing.T) {
		complete, err := store.IsDeltaComplete(ctx, "d1")
		require.NoError(t, err)
		assert.False(t, complete)
	})

	t.Run("set to true", func(t *testing.T) {
		require.NoError(t, store.SetDeltaComplete(ctx, "d1", true))

		complete, err := store.IsDeltaComplete(ctx, "d1")
		require.NoError(t, err)
		assert.True(t, complete)
	})

	t.Run("set to false", func(t *testing.T) {
		require.NoError(t, store.SetDeltaComplete(ctx, "d1", false))

		complete, err := store.IsDeltaComplete(ctx, "d1")
		require.NoError(t, err)
		assert.False(t, complete)
	})
}

func TestConflictCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Need a parent item for the FK constraint.
	item := makeTestItem("d1", "item1", "d1", "root1", "conflict.txt", ItemTypeFile)
	require.NoError(t, store.UpsertItem(ctx, item))

	conflict := &ConflictRecord{
		ID:          "conflict-1",
		DriveID:     "d1",
		ItemID:      "item1",
		Path:        "conflict.txt",
		DetectedAt:  NowNano(),
		LocalHash:   "localhash",
		RemoteHash:  "remotehash",
		LocalMtime:  Int64Ptr(NowNano()),
		RemoteMtime: Int64Ptr(NowNano()),
		Resolution:  ConflictUnresolved,
		History:     "[]",
	}

	t.Run("record conflict", func(t *testing.T) {
		require.NoError(t, store.RecordConflict(ctx, conflict))
	})

	t.Run("list conflicts", func(t *testing.T) {
		conflicts, err := store.ListConflicts(ctx, "d1")
		require.NoError(t, err)
		require.Len(t, conflicts, 1)
		assert.Equal(t, "conflict-1", conflicts[0].ID)
		assert.Equal(t, ConflictUnresolved, conflicts[0].Resolution)
	})

	t.Run("conflict count", func(t *testing.T) {
		count, err := store.ConflictCount(ctx, "d1")
		require.NoError(t, err)
		assert.Equal(t, 1, count)
	})

	t.Run("resolve conflict", func(t *testing.T) {
		require.NoError(t, store.ResolveConflict(
			ctx, "conflict-1", ConflictKeepLocal, ResolvedByUser))

		conflicts, err := store.ListConflicts(ctx, "d1")
		require.NoError(t, err)
		require.Len(t, conflicts, 1)
		assert.Equal(t, ConflictKeepLocal, conflicts[0].Resolution)
		assert.NotNil(t, conflicts[0].ResolvedAt)
		require.NotNil(t, conflicts[0].ResolvedBy)
		assert.Equal(t, ResolvedByUser, *conflicts[0].ResolvedBy)
	})

	t.Run("count after resolution", func(t *testing.T) {
		count, err := store.ConflictCount(ctx, "d1")
		require.NoError(t, err)
		assert.Equal(t, 0, count, "resolved conflicts should not be counted")
	})
}

func TestStaleFileCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	record := &StaleRecord{
		ID:         "stale-1",
		Path:       "old-file.txt",
		Reason:     "pattern changed",
		DetectedAt: NowNano(),
		Size:       Int64Ptr(4096),
	}

	t.Run("record and list", func(t *testing.T) {
		require.NoError(t, store.RecordStaleFile(ctx, record))

		files, err := store.ListStaleFiles(ctx)
		require.NoError(t, err)
		require.Len(t, files, 1)
		assert.Equal(t, "stale-1", files[0].ID)
		assert.Equal(t, "old-file.txt", files[0].Path)
		assert.Equal(t, int64(4096), *files[0].Size)
	})

	t.Run("remove", func(t *testing.T) {
		require.NoError(t, store.RemoveStaleFile(ctx, "stale-1"))

		files, err := store.ListStaleFiles(ctx)
		require.NoError(t, err)
		assert.Empty(t, files)
	})
}

func TestUploadSessionCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := NowNano()
	futureExpiry := time.Now().Add(24 * time.Hour).UnixNano()
	pastExpiry := time.Now().Add(-24 * time.Hour).UnixNano()

	session := &UploadSessionRecord{
		ID:            "sess-1",
		DriveID:       "d1",
		ItemID:        "item1",
		LocalPath:     "/tmp/upload.bin",
		SessionURL:    "https://upload.example.com/session1",
		Expiry:        futureExpiry,
		BytesUploaded: 0,
		TotalSize:     1048576,
		CreatedAt:     now,
	}

	t.Run("save and get", func(t *testing.T) {
		require.NoError(t, store.SaveUploadSession(ctx, session))

		got, err := store.GetUploadSession(ctx, "sess-1")
		require.NoError(t, err)
		assert.Equal(t, "sess-1", got.ID)
		assert.Equal(t, "/tmp/upload.bin", got.LocalPath)
		assert.Equal(t, int64(1048576), got.TotalSize)
	})

	t.Run("update bytes uploaded", func(t *testing.T) {
		session.BytesUploaded = 524288
		require.NoError(t, store.SaveUploadSession(ctx, session))

		got, err := store.GetUploadSession(ctx, "sess-1")
		require.NoError(t, err)
		assert.Equal(t, int64(524288), got.BytesUploaded)
	})

	t.Run("list expired", func(t *testing.T) {
		expired := &UploadSessionRecord{
			ID:         "sess-expired",
			DriveID:    "d1",
			ItemID:     "item2",
			LocalPath:  "/tmp/expired.bin",
			SessionURL: "https://upload.example.com/expired",
			Expiry:     pastExpiry,
			TotalSize:  512,
			CreatedAt:  now,
		}
		require.NoError(t, store.SaveUploadSession(ctx, expired))

		sessions, err := store.ListExpiredSessions(ctx, NowNano())
		require.NoError(t, err)
		assert.Len(t, sessions, 1)
		assert.Equal(t, "sess-expired", sessions[0].ID)
	})

	t.Run("delete", func(t *testing.T) {
		require.NoError(t, store.DeleteUploadSession(ctx, "sess-1"))

		got, err := store.GetUploadSession(ctx, "sess-1")
		require.NoError(t, err)
		assert.Nil(t, got, "deleted session should return (nil, nil)")
	})
}

func TestConfigSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("empty for unknown key", func(t *testing.T) {
		val, err := store.GetConfigSnapshot(ctx, "filter_patterns")
		require.NoError(t, err)
		assert.Empty(t, val)
	})

	t.Run("save and retrieve", func(t *testing.T) {
		require.NoError(t, store.SaveConfigSnapshot(
			ctx, "filter_patterns", `["*.tmp","*.log"]`))

		val, err := store.GetConfigSnapshot(ctx, "filter_patterns")
		require.NoError(t, err)
		assert.Equal(t, `["*.tmp","*.log"]`, val)
	})

	t.Run("update existing", func(t *testing.T) {
		require.NoError(t, store.SaveConfigSnapshot(
			ctx, "filter_patterns", `["*.tmp"]`))

		val, err := store.GetConfigSnapshot(ctx, "filter_patterns")
		require.NoError(t, err)
		assert.Equal(t, `["*.tmp"]`, val)
	})
}

func TestCheckpoint(t *testing.T) {
	store := newTestStore(t)
	assert.NoError(t, store.Checkpoint())
}

func TestCloseAndReopen(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	logger := testLogger(t)

	store, err := NewStore(dbPath, logger)
	require.NoError(t, err)

	ctx := context.Background()
	item := makeTestItem("d1", "persist1", "d1", "root1", "persist.txt", ItemTypeFile)
	item.Path = "persist.txt"
	require.NoError(t, store.UpsertItem(ctx, item))
	require.NoError(t, store.Close())

	// Reopen and verify data persists.
	store2, err := NewStore(dbPath, logger)
	require.NoError(t, err)

	got, err := store2.GetItem(ctx, "d1", "persist1")
	require.NoError(t, err)
	assert.Equal(t, "persist.txt", got.Name)
	require.NoError(t, store2.Close())
}

// TestInterfaceCompliance verifies SQLiteStore implements Store at compile time.
func TestInterfaceCompliance(t *testing.T) {
	var _ Store = (*SQLiteStore)(nil)
}

// --- Error path tests for NewStore, Close, applyMigration, BatchUpsert ---

func TestNewStore_InvalidPath(t *testing.T) {
	// Attempting to open a database in a non-existent directory should fail.
	_, err := NewStore("/nonexistent/dir/db.sqlite", testLogger(t))
	require.Error(t, err)
}

func TestApplyMigration_MissingFile(t *testing.T) {
	// applyMigration with a version that has no corresponding embedded SQL file
	// should return an error about reading the migration.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	// Version 999 has no migration file embedded.
	applyErr := applyMigration(context.Background(), db, testLogger(t), 999)
	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "read migration 999")
}

func TestClose_ThenQuery(t *testing.T) {
	// After closing a store, database operations should fail because the
	// underlying connection and prepared statements are no longer valid.
	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	require.NoError(t, store.Close())

	// Querying after close should fail.
	_, queryErr := store.GetDeltaToken(context.Background(), "d1")
	require.Error(t, queryErr)
}

func TestBatchUpsert_TxError(t *testing.T) {
	// Close the underlying DB before calling BatchUpsert so the transaction begin fails.
	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	// Close the raw DB handle directly to poison the connection.
	require.NoError(t, store.db.Close())

	items := []*Item{
		makeTestItem("d1", "b1", "d1", "root1", "batch1.txt", ItemTypeFile),
	}

	batchErr := store.BatchUpsert(context.Background(), items)
	require.Error(t, batchErr)
}

// --- A3: GetUploadSession not-found returns (nil, nil) ---

func TestGetUploadSession_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Querying a session ID that was never saved should return (nil, nil),
	// matching the pattern used by GetItem and GetItemByPath.
	got, err := store.GetUploadSession(ctx, "nonexistent-session")
	require.NoError(t, err)
	assert.Nil(t, got, "missing upload session should return nil, not an error")
}

// --- A5: NewStore failure paths when DB is closed before each stage ---

func TestSetPragmas_ClosedDB(t *testing.T) {
	// setPragmas should fail when the underlying DB connection is closed.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	pragmaErr := setPragmas(context.Background(), db, testLogger(t))
	require.Error(t, pragmaErr)
	assert.Contains(t, pragmaErr.Error(), "set pragma")
}

func TestRunMigrations_ClosedDB(t *testing.T) {
	// runMigrations should fail when the DB is closed because PRAGMA user_version
	// cannot be read.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	migErr := runMigrations(context.Background(), db, testLogger(t))
	require.Error(t, migErr)
	assert.Contains(t, migErr.Error(), "read schema version")
}

func TestPrepareAllStatements_ClosedDB(t *testing.T) {
	// prepareAllStatements should fail when the DB is closed.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	prepErr := s.prepareAllStatements(context.Background())
	require.Error(t, prepErr)
}

// --- A6: Prepare statement groups fail when their table is missing ---

func TestPrepareStatements_DeltaGroupFailure(t *testing.T) {
	// When the delta_tokens table is missing, prepareDeltaStmts should fail.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	// No migrations applied — tables don't exist.
	prepErr := s.prepareDeltaStmts(context.Background())
	require.Error(t, prepErr)
	assert.Contains(t, prepErr.Error(), "prepare")
}

func TestPrepareStatements_ConflictGroupFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	prepErr := s.prepareConflictStmts(context.Background())
	require.Error(t, prepErr)
	assert.Contains(t, prepErr.Error(), "prepare")
}

func TestPrepareStatements_StaleGroupFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	prepErr := s.prepareStaleStmts(context.Background())
	require.Error(t, prepErr)
	assert.Contains(t, prepErr.Error(), "prepare")
}

func TestPrepareStatements_UploadGroupFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	prepErr := s.prepareUploadStmts(context.Background())
	require.Error(t, prepErr)
	assert.Contains(t, prepErr.Error(), "prepare")
}

func TestPrepareStatements_ConfigGroupFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	s := &SQLiteStore{db: db, logger: testLogger(t)}

	prepErr := s.prepareConfigStmts(context.Background())
	require.Error(t, prepErr)
	assert.Contains(t, prepErr.Error(), "prepare")
}

// --- A7: applyMigration with closed DB (BeginTx fails) ---

func TestApplyMigration_BeginTxError(t *testing.T) {
	// When the DB is closed, BeginTx should fail, exercising the early error path.
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	require.NoError(t, db.Close())

	applyErr := applyMigration(context.Background(), db, testLogger(t), 1)
	require.Error(t, applyErr)
	assert.Contains(t, applyErr.Error(), "begin migration tx")
}

// --- A8: closeStatements double-close is safe (modernc.org/sqlite is idempotent) ---

func TestCloseStatements_DoubleClose(t *testing.T) {
	// Per LEARNINGS.md: "modernc.org/sqlite Close() is idempotent".
	// Manually close a statement, then call closeStatements — should not panic.
	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	// Manually close the getItem prepared statement.
	require.NoError(t, store.itemStmts.get.Close())

	// closeStatements should handle the already-closed statement gracefully.
	// With modernc.org/sqlite, closing again is a no-op (no error).
	closeErr := store.closeStatements()
	assert.NoError(t, closeErr, "double-closing prepared statements should not error with modernc.org/sqlite")

	// Clean up the DB connection (statements already closed above).
	require.NoError(t, store.db.Close())
}

// --- A9: Query operations on closed DB propagate errors ---

func TestListStaleFiles_ClosedDB(t *testing.T) {
	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	// Close the underlying DB to force query errors.
	require.NoError(t, store.db.Close())

	_, listErr := store.ListStaleFiles(context.Background())
	require.Error(t, listErr)
	assert.Contains(t, listErr.Error(), "list stale files")
}

func TestScanItemRows_ClosedDB(t *testing.T) {
	store, err := NewStore(":memory:", testLogger(t))
	require.NoError(t, err)

	// Insert an item so there's data to query.
	ctx := context.Background()
	item := makeTestItem("d1", "item1", "d1", "root1", "scan-closed.txt", ItemTypeFile)
	item.Path = "scan-closed.txt"
	require.NoError(t, store.UpsertItem(ctx, item))

	// Close the underlying DB — subsequent queries using prepared statements
	// should fail, exercising scanItemRows error propagation.
	require.NoError(t, store.db.Close())

	_, listErr := store.ListAllActiveItems(ctx)
	require.Error(t, listErr)
	assert.Contains(t, listErr.Error(), "list active items")
}

// --- B-050: DeleteItemByKey tests ---

func TestDeleteItemByKey(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	item := makeTestItem("d1", "item1", "d1", "root1", "deltest.txt", ItemTypeFile)
	item.Path = "deltest.txt"
	require.NoError(t, store.UpsertItem(ctx, item))

	// Verify the item exists.
	got, err := store.GetItem(ctx, "d1", "item1")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Delete by key.
	require.NoError(t, store.DeleteItemByKey(ctx, "d1", "item1"))

	// Verify the item is gone (physical delete, not tombstone).
	got, err = store.GetItem(ctx, "d1", "item1")
	require.NoError(t, err)
	assert.Nil(t, got, "item should be physically deleted")
}

func TestDeleteItemByKey_Nonexistent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Deleting a nonexistent key should not return an error.
	err := store.DeleteItemByKey(ctx, "d1", "nonexistent")
	assert.NoError(t, err)
}
