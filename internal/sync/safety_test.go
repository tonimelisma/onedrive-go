package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// --- Safety-specific mock store ---

// safetyMockStore implements the Store interface for safety checker tests.
// Only methods used by the safety checker have real implementations;
// unused methods return zero values.
type safetyMockStore struct {
	items         []*Item          // returned by ListAllActiveItems
	deltaComplete map[string]bool  // driveID -> complete
	deltaErr      map[string]error // driveID -> error from IsDeltaComplete
}

func newSafetyMockStore() *safetyMockStore {
	return &safetyMockStore{
		deltaComplete: make(map[string]bool),
		deltaErr:      make(map[string]error),
	}
}

func (s *safetyMockStore) ListAllActiveItems(_ context.Context) ([]*Item, error) {
	return s.items, nil
}

func (s *safetyMockStore) IsDeltaComplete(_ context.Context, driveID string) (bool, error) {
	if err, ok := s.deltaErr[driveID]; ok {
		return false, err
	}

	return s.deltaComplete[driveID], nil
}

// Unused Store interface methods — required for interface satisfaction.

func (s *safetyMockStore) GetItem(context.Context, string, string) (*Item, error) { return nil, nil }
func (s *safetyMockStore) UpsertItem(context.Context, *Item) error                { return nil }

func (s *safetyMockStore) MarkDeleted(context.Context, string, string, int64) error { return nil }

func (s *safetyMockStore) ListChildren(context.Context, string, string) ([]*Item, error) {
	return nil, nil
}

func (s *safetyMockStore) GetItemByPath(context.Context, string) (*Item, error) { return nil, nil }

func (s *safetyMockStore) ListSyncedItems(context.Context) ([]*Item, error) { return nil, nil }

func (s *safetyMockStore) BatchUpsert(context.Context, []*Item) error { return nil }

func (s *safetyMockStore) MaterializePath(context.Context, string, string) (string, error) {
	return "", nil
}

func (s *safetyMockStore) CascadePathUpdate(context.Context, string, string) error { return nil }

func (s *safetyMockStore) CleanupTombstones(context.Context, int) (int64, error) { return 0, nil }

func (s *safetyMockStore) GetDeltaToken(context.Context, string) (string, error) { return "", nil }

func (s *safetyMockStore) SaveDeltaToken(context.Context, string, string) error { return nil }

func (s *safetyMockStore) DeleteDeltaToken(context.Context, string) error { return nil }

func (s *safetyMockStore) SetDeltaComplete(context.Context, string, bool) error { return nil }

func (s *safetyMockStore) RecordConflict(context.Context, *ConflictRecord) error { return nil }

func (s *safetyMockStore) ListConflicts(context.Context, string) ([]*ConflictRecord, error) {
	return nil, nil
}

func (s *safetyMockStore) ResolveConflict(context.Context, string, ConflictResolution, ConflictResolvedBy) error {
	return nil
}

func (s *safetyMockStore) ConflictCount(context.Context, string) (int, error) { return 0, nil }

func (s *safetyMockStore) RecordStaleFile(context.Context, *StaleRecord) error { return nil }

func (s *safetyMockStore) ListStaleFiles(context.Context) ([]*StaleRecord, error) { return nil, nil }

func (s *safetyMockStore) RemoveStaleFile(context.Context, string) error { return nil }

func (s *safetyMockStore) SaveUploadSession(context.Context, *UploadSessionRecord) error {
	return nil
}

func (s *safetyMockStore) GetUploadSession(context.Context, string) (*UploadSessionRecord, error) {
	return nil, nil
}

func (s *safetyMockStore) DeleteUploadSession(context.Context, string) error { return nil }

func (s *safetyMockStore) ListExpiredSessions(context.Context, int64) ([]*UploadSessionRecord, error) {
	return nil, nil
}

func (s *safetyMockStore) GetConfigSnapshot(context.Context, string) (string, error) { return "", nil }

func (s *safetyMockStore) SaveConfigSnapshot(context.Context, string, string) error { return nil }

func (s *safetyMockStore) Checkpoint() error { return nil }

func (s *safetyMockStore) Close() error { return nil }

// --- Test helpers ---

func safetyChecker(t *testing.T, store Store) *SafetyChecker {
	t.Helper()

	sc := NewSafetyChecker(
		store,
		&config.SafetyConfig{
			BigDeleteThreshold:  10,
			BigDeletePercentage: 50,
			BigDeleteMinItems:   5,
			MinFreeSpace:        "1GB",
		},
		"/tmp/sync",
		testLogger(t),
	)

	// Default mock: report ample disk space so tests not targeting S6 pass.
	sc.statfsFunc = func(_ string) (uint64, error) {
		return 100_000_000_000, nil // 100 GB
	}

	return sc
}

func safetyItem(syncedHash string) *Item {
	return &Item{
		DriveID:    "d",
		ItemID:     "item-1",
		SyncedHash: syncedHash,
		Size:       Int64Ptr(1024),
	}
}

// --- S1: Never delete remote from local absence ---

func TestSafety_S1_RemoteDeleteWithoutSyncedHash(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		RemoteDeletes: []Action{
			{Type: ActionRemoteDelete, Path: "unsynced.txt", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.RemoteDeletes, "remote delete without SyncedHash should be removed")
}

func TestSafety_S1_RemoteDeleteWithSyncedHash(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		RemoteDeletes: []Action{
			{Type: ActionRemoteDelete, Path: "synced.txt", ItemID: "a1", Item: safetyItem("abc123")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Len(t, result.RemoteDeletes, 1, "remote delete with SyncedHash should pass")
}

func TestSafety_S1_RemoteDeleteNilItem(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		RemoteDeletes: []Action{
			{Type: ActionRemoteDelete, Path: "no-item.txt", ItemID: "a1", Item: nil},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.RemoteDeletes, "remote delete with nil item should be removed")
}

// --- S2: No deletions from incomplete delta ---

func TestSafety_S2_IncompleteDelta(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaComplete["d"] = false
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "file.txt", ItemID: "a1", Item: safetyItem("hash")},
			{Type: ActionLocalDelete, DriveID: "d", Path: "other.txt", ItemID: "a2", Item: safetyItem("hash")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.LocalDeletes, "all local deletes should be removed for incomplete delta")
}

func TestSafety_S2_CompleteDelta(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "file.txt", ItemID: "a1", Item: safetyItem("hash")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Len(t, result.LocalDeletes, 1, "local deletes should pass with complete delta")
}

func TestSafety_S2_DeltaCheckError(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaErr["d"] = errors.New("db error")
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "file.txt", ItemID: "a1", Item: safetyItem("hash")},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S2")
}

// --- S3: Atomic file writes ---

func TestSafety_S3_DownloadPartialPath(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	// A download targeting a .partial path is a plan-level error.
	// The check only logs a warning; it does not remove the action.
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "file.partial", ItemID: "a1", Item: safetyItem("hash")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	// S3 is advisory at plan time — the download remains.
	assert.Len(t, result.Downloads, 1)
}

// --- S4: Hash-before-delete guard ---

func TestSafety_S4_LocalDeleteWithoutSyncedHash(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "unsynced.txt", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.LocalDeletes, "local delete without SyncedHash should be removed by S4")
}

func TestSafety_S4_LocalDeleteWithSyncedHash(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "synced.txt", ItemID: "a1", Item: safetyItem("abc123")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Len(t, result.LocalDeletes, 1, "local delete with SyncedHash should pass S4")
}

func TestSafety_S4_LocalDeleteNilItem(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Path: "no-item.txt", ItemID: "a1", Item: nil},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.LocalDeletes, "local delete with nil item should be removed by S4")
}

// --- S5: Big-delete protection ---

func TestSafety_S5_BigDeleteAbsoluteThreshold(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	// 20 active items, threshold is 10 items.
	for i := range 20 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	// 11 local deletes > threshold of 10.
	var deletes []Action
	for range 11 {
		deletes = append(deletes, Action{
			Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash"),
		})
	}

	plan := &ActionPlan{LocalDeletes: deletes}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBigDeleteBlocked)
}

func TestSafety_S5_BigDeletePercentageThreshold(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	// 10 active items, percentage threshold is 50%.
	for i := range 10 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	// 6 local deletes = 60% of 10, exceeds 50%.
	// But absolute count (6) does not exceed threshold (10).
	// Percentage alone should trigger.
	var deletes []Action
	for range 6 {
		deletes = append(deletes, Action{
			Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash"),
		})
	}

	plan := &ActionPlan{LocalDeletes: deletes}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBigDeleteBlocked)
}

func TestSafety_S5_BigDeleteBelowMinItems(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	// 3 active items, below min_items threshold (5).
	for i := range 3 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	// 3 deletes = 100% of 3 items, but below min_items so check is skipped.
	var deletes []Action
	for range 3 {
		deletes = append(deletes, Action{
			Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash"),
		})
	}

	plan := &ActionPlan{LocalDeletes: deletes}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err, "big-delete check should be skipped for drives below min items")
}

func TestSafety_S5_BigDeleteWithForce(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	for i := range 20 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	var deletes []Action
	for range 15 {
		deletes = append(deletes, Action{
			Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash"),
		})
	}

	plan := &ActionPlan{LocalDeletes: deletes}

	_, err := sc.Check(context.Background(), plan, true, false)
	require.NoError(t, err, "big-delete should be allowed with force=true")
}

func TestSafety_S5_BigDeleteZeroDeletes(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	for i := range 20 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Downloads: []Action{{Type: ActionDownload, Item: safetyItem("hash")}},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err, "zero deletes should never trigger big-delete check")
}

// --- S6: Disk space check ---

func TestSafety_S6_InsufficientDiskSpace(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	// Inject a statfsFunc that reports only 500MB available.
	sc.statfsFunc = func(_ string) (uint64, error) {
		return 500_000_000, nil // 500 MB
	}

	downloadSize := int64(600_000_000) // 600 MB download
	plan := &ActionPlan{
		Downloads: []Action{
			{
				Type: ActionDownload, Path: "big.bin", ItemID: "a1",
				Item: &Item{Size: &downloadSize},
			},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInsufficientDiskSpace)
}

func TestSafety_S6_SufficientDiskSpace(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	// Inject a statfsFunc that reports 10GB available.
	sc.statfsFunc = func(_ string) (uint64, error) {
		return 10_000_000_000, nil // 10 GB
	}

	downloadSize := int64(1_000_000) // 1 MB download
	plan := &ActionPlan{
		Downloads: []Action{
			{
				Type: ActionDownload, Path: "small.bin", ItemID: "a1",
				Item: &Item{Size: &downloadSize},
			},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
}

func TestSafety_S6_StatfsError(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	sc.statfsFunc = func(_ string) (uint64, error) {
		return 0, errors.New("filesystem error")
	}

	downloadSize := int64(1024)
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "file.bin", ItemID: "a1", Item: &Item{Size: &downloadSize}},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S6")
}

// --- S7: No temp/partial uploads ---

func TestSafety_S7_PartialFileUpload(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "download.partial", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.Uploads, ".partial upload should be removed")
}

func TestSafety_S7_TmpFileUpload(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "data.tmp", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.Uploads, ".tmp upload should be removed")
}

func TestSafety_S7_TildeFileUpload(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "~lockfile", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.Uploads, "~file upload should be removed")
}

func TestSafety_S7_NormalFileUpload(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "document.docx", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Len(t, result.Uploads, 1, "normal file should pass S7")
}

func TestSafety_S7_UppercasePartial(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "FILE.PARTIAL", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.Uploads, "uppercase .PARTIAL should be caught case-insensitively")
}

func TestSafety_S7_NestedPartialPath(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	// Only the filename matters, not parent directories.
	plan := &ActionPlan{
		Uploads: []Action{
			{Type: ActionUpload, Path: "dir/sub/file.partial", ItemID: "a1", Item: safetyItem("")},
		},
	}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Empty(t, result.Uploads, "nested .partial should be caught")
}

// --- Dry-run mode ---

func TestSafety_DryRunMode(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	for i := range 20 {
		store.items = append(store.items, &Item{ItemID: string(rune('A' + i))})
	}

	store.deltaComplete["d"] = true
	sc := safetyChecker(t, store)

	// Inject insufficient disk space.
	sc.statfsFunc = func(_ string) (uint64, error) {
		return 500_000_000, nil // 500 MB
	}

	downloadSize := int64(600_000_000)

	// Create a plan that violates both S5 (big delete) and S6 (disk space).
	var deletes []Action
	for range 15 {
		deletes = append(deletes, Action{
			Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash"),
		})
	}

	plan := &ActionPlan{
		LocalDeletes: deletes,
		Downloads: []Action{
			{Type: ActionDownload, Path: "big.bin", Item: &Item{Size: &downloadSize}},
		},
	}

	// With dryRun=true, violations should be logged but not block.
	result, err := sc.Check(context.Background(), plan, false, true)
	require.NoError(t, err, "dry-run should not return errors")
	assert.NotNil(t, result)
}

// --- Empty plan ---

func TestSafety_EmptyPlan(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	plan := &ActionPlan{}

	result, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalActions())
}

// --- NewSafetyChecker nil logger ---

func TestNewSafetyChecker_NilLogger(t *testing.T) {
	t.Parallel()

	sc := NewSafetyChecker(
		newSafetyMockStore(),
		&config.SafetyConfig{},
		"/tmp/sync",
		nil,
	)
	assert.NotNil(t, sc)
	assert.NotNil(t, sc.logger)
}

// --- S5 edge cases ---

func TestSafety_S5_ListActiveItemsError(t *testing.T) {
	t.Parallel()

	// Use a store that errors on ListAllActiveItems.
	store := &safetyListErrorStore{}
	sc := safetyChecker(t, store)

	plan := &ActionPlan{
		LocalDeletes: []Action{
			{Type: ActionLocalDelete, DriveID: "d", Item: safetyItem("hash")},
		},
	}

	// IsDeltaComplete needs to return true so S2 doesn't strip the deletes.
	// But our error store doesn't implement it, so we need a richer approach.
	// Actually, the S4 filter may strip these if SyncedHash is empty.
	// Let's ensure S5 is reached by having synced items that pass S4.

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S5")
}

// safetyListErrorStore errors on ListAllActiveItems but supports delta checks.
type safetyListErrorStore struct {
	safetyMockStore
}

func (s *safetyListErrorStore) ListAllActiveItems(_ context.Context) ([]*Item, error) {
	return nil, errors.New("db read error")
}

func (s *safetyListErrorStore) IsDeltaComplete(_ context.Context, _ string) (bool, error) {
	return true, nil
}

// --- S6 edge cases ---

func TestSafety_S6_ZeroDownloadSize(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	// Downloads with nil size items — total download is zero.
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "empty.bin", ItemID: "a1", Item: &Item{}},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err, "zero download size should skip disk space check")
}

func TestSafety_S6_InvalidMinFreeSpace(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := NewSafetyChecker(
		store,
		&config.SafetyConfig{
			MinFreeSpace: "invalid-size",
		},
		"/tmp/sync",
		testLogger(t),
	)

	sc.statfsFunc = func(_ string) (uint64, error) {
		return 100_000_000_000, nil
	}

	downloadSize := int64(1024)
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "file.bin", ItemID: "a1", Item: &Item{Size: &downloadSize}},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "S6: parse min_free_space")
}

func TestSafety_S6_ZeroMinFreeSpace(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := NewSafetyChecker(
		store,
		&config.SafetyConfig{
			MinFreeSpace: "0",
		},
		"/tmp/sync",
		testLogger(t),
	)

	sc.statfsFunc = func(_ string) (uint64, error) {
		return 1, nil // almost no space
	}

	downloadSize := int64(1024)
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "file.bin", ItemID: "a1", Item: &Item{Size: &downloadSize}},
		},
	}

	// min_free_space = 0 disables the check.
	_, err := sc.Check(context.Background(), plan, false, false)
	require.NoError(t, err)
}

func TestSafety_S6_DryRunInsufficientSpace(t *testing.T) {
	t.Parallel()

	store := newSafetyMockStore()
	sc := safetyChecker(t, store)

	sc.statfsFunc = func(_ string) (uint64, error) {
		return 500_000_000, nil // 500 MB
	}

	downloadSize := int64(600_000_000)
	plan := &ActionPlan{
		Downloads: []Action{
			{Type: ActionDownload, Path: "big.bin", ItemID: "a1", Item: &Item{Size: &downloadSize}},
		},
	}

	_, err := sc.Check(context.Background(), plan, false, true)
	require.NoError(t, err, "dry-run should not return disk space error")
}

// --- isTempFile ---

func TestIsTempFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		expected bool
	}{
		{"partial extension", "file.partial", true},
		{"PARTIAL upper", "FILE.PARTIAL", true},
		{"tmp extension", "data.tmp", true},
		{"TMP upper", "DATA.TMP", true},
		{"tilde prefix", "~lockfile", true},
		{"tilde dollar", "~$document.docx", true},
		{"normal file", "document.docx", false},
		{"partial in name", "partial-results.csv", false},
		{"tmp in name", "tmpdir-config.json", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isTempFile(tt.filename))
		})
	}
}
