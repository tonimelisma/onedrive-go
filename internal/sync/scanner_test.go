package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// --- Mock Store ---

// scannerMockStore implements ScannerStore for testing the scanner.
type scannerMockStore struct {
	items          map[string]*Item // keyed by Path
	syncedItems    []*Item
	allActiveItems []*Item // returned by ListAllActiveItems for folder orphan detection
	upserted       []*Item // tracks all upserted items

	// Error injection for testing error paths
	listAllActiveErr error
	upsertItemErr    error
	getItemByPathErr error
}

func newScannerMockStore() *scannerMockStore {
	return &scannerMockStore{items: make(map[string]*Item)}
}

func (m *scannerMockStore) GetItemByPath(_ context.Context, path string) (*Item, error) {
	if m.getItemByPathErr != nil {
		return nil, m.getItemByPathErr
	}

	item, ok := m.items[path]
	if !ok {
		return nil, nil
	}
	// Return a copy so mutations don't silently bypass UpsertItem
	cp := *item
	return &cp, nil
}

func (m *scannerMockStore) UpsertItem(_ context.Context, item *Item) error {
	if m.upsertItemErr != nil {
		return m.upsertItemErr
	}

	cp := *item
	m.items[item.Path] = &cp
	m.upserted = append(m.upserted, &cp)
	return nil
}

func (m *scannerMockStore) ListSyncedItems(_ context.Context) ([]*Item, error) {
	return m.syncedItems, nil
}

func (m *scannerMockStore) ListAllActiveItems(context.Context) ([]*Item, error) {
	if m.listAllActiveErr != nil {
		return nil, m.listAllActiveErr
	}

	return m.allActiveItems, nil
}

// --- Mock Filter ---

// mockFilter implements the Filter interface. Includes everything by default;
// exclusions are configured via the excluded map.
type mockFilter struct {
	excluded map[string]string // path -> reason
}

func newMockFilter() *mockFilter {
	return &mockFilter{excluded: make(map[string]string)}
}

func (f *mockFilter) ShouldSync(path string, _ bool, _ int64) FilterResult {
	if reason, ok := f.excluded[path]; ok {
		return FilterResult{Included: false, Reason: reason}
	}
	return FilterResult{Included: true}
}

// --- Test helpers ---

// writeFile creates a file with the given content in the temp directory.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// hashContent computes the QuickXorHash of content bytes, base64-encoded.
func hashContent(content string) string {
	h := quickxorhash.New()
	h.Write([]byte(content))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// testScanner creates a Scanner with a test logger that writes to t.Log.
// DriveID is hardcoded to "test-drive" — all scanner tests use the same value.
func testScanner(t *testing.T, store ScannerStore, filter Filter, skipSymlinks bool) *Scanner {
	t.Helper()
	return NewScanner("test-drive", store, filter, skipSymlinks, testLogger(t))
}

// --- Tests ---

func TestScan_BasicFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "hello.txt", "hello world")
	writeFile(t, root, "data.bin", "binary data")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Both files should be upserted
	assert.Len(t, store.upserted, 2)

	item1 := store.items["hello.txt"]
	require.NotNil(t, item1)
	assert.Equal(t, "hello.txt", item1.Path)
	assert.Equal(t, "hello.txt", item1.Name)
	assert.Equal(t, ItemTypeFile, item1.ItemType)
	assert.Equal(t, hashContent("hello world"), item1.LocalHash)
	assert.NotNil(t, item1.LocalSize)
	assert.Equal(t, int64(11), *item1.LocalSize)
	assert.NotNil(t, item1.LocalMtime)

	item2 := store.items["data.bin"]
	require.NotNil(t, item2)
	assert.Equal(t, hashContent("binary data"), item2.LocalHash)
}

func TestScan_NosyncGuard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, ".nosync", "")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	assert.ErrorIs(t, err, ErrNosyncGuard)
	// No items should be scanned
	assert.Empty(t, store.upserted)
}

func TestScan_NosyncAbsent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "file.txt", "content")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)
	assert.Len(t, store.upserted, 1)
}

func TestScan_FilterExclusion(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "keep")
	writeFile(t, root, "skip.log", "skip")

	filter := newMockFilter()
	filter.excluded["skip.log"] = "matched *.log pattern"

	store := newScannerMockStore()
	scanner := testScanner(t, store, filter, true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	assert.Len(t, store.upserted, 1)
	assert.NotNil(t, store.items["keep.txt"])
	assert.Nil(t, store.items["skip.log"])
}

func TestScan_UnicodeNFCNormalization(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a file with NFD-composed name: e + combining acute = é
	nfdName := "cafe\u0301.txt" // NFD: e + combining accent
	nfcName := "caf\u00e9.txt"  // NFC: precomposed é

	writeFile(t, root, nfdName, "coffee")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// The stored path should be NFC-normalized
	item := store.items[nfcName]
	if item == nil {
		// On some systems, the OS may already normalize the name.
		// Check if the item was stored with the filesystem name.
		item = store.items[nfdName]
	}
	require.NotNil(t, item, "file should be tracked regardless of normalization form")
}

func TestScan_OneDriveNameValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileName string
		valid    bool
	}{
		{name: "normal file", fileName: "readme.txt", valid: true},
		{name: "dotfile", fileName: ".gitignore", valid: true},
		{name: "contains colon", fileName: "file:name.txt", valid: false},
		{name: "contains question mark", fileName: "what?.txt", valid: false},
		{name: "contains asterisk", fileName: "wild*.txt", valid: false},
		{name: "contains pipe", fileName: "pipe|file.txt", valid: false},
		{name: "contains less than", fileName: "less<file.txt", valid: false},
		{name: "contains greater than", fileName: "more>file.txt", valid: false},
		{name: "contains double quote", fileName: `say"what.txt`, valid: false},
		{name: "contains backslash", fileName: `back\slash.txt`, valid: false},
		{name: "trailing dot", fileName: "file.", valid: false},
		{name: "trailing space", fileName: "file ", valid: false},
		{name: "reserved CON", fileName: "CON", valid: false},
		{name: "reserved con.txt", fileName: "con.txt", valid: false},
		{name: "reserved PRN", fileName: "PRN", valid: false},
		{name: "reserved AUX", fileName: "AUX", valid: false},
		{name: "reserved NUL", fileName: "NUL", valid: false},
		{name: "reserved COM1", fileName: "COM1", valid: false},
		{name: "reserved LPT1", fileName: "LPT1", valid: false},
		{name: "reserved COM1.txt", fileName: "COM1.txt", valid: false},
		{name: "not reserved CONX", fileName: "CONX.txt", valid: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			valid, _ := isValidOneDriveName(tc.fileName)
			assert.Equal(t, tc.valid, valid)
		})
	}
}

func TestScan_SymlinkSkipMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a real file and a symlink to it
	writeFile(t, root, "real.txt", "real content")
	err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt"))
	require.NoError(t, err)

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true) // skipSymlinks=true

	err = scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Only the real file should be tracked, symlink skipped
	assert.Len(t, store.upserted, 1)
	assert.NotNil(t, store.items["real.txt"])
	assert.Nil(t, store.items["link.txt"])
}

func TestScan_SymlinkFollowMode(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	writeFile(t, root, "real.txt", "real content")
	err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt"))
	require.NoError(t, err)

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), false) // skipSymlinks=false

	err = scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Both should be tracked when following symlinks
	assert.Len(t, store.upserted, 2)
	assert.NotNil(t, store.items["real.txt"])
	assert.NotNil(t, store.items["link.txt"])
}

func TestScan_MtimeFastPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	filePath := writeFile(t, root, "stable.txt", "unchanged")

	info, err := os.Stat(filePath)
	require.NoError(t, err)

	mtime := ToUnixNano(info.ModTime().UTC())

	// Pre-populate store with matching mtime
	store := newScannerMockStore()
	store.items["stable.txt"] = &Item{
		Path:       "stable.txt",
		Name:       "stable.txt",
		ItemType:   ItemTypeFile,
		LocalSize:  Int64Ptr(info.Size()),
		LocalMtime: Int64Ptr(mtime),
		LocalHash:  hashContent("unchanged"),
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err = scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// No upserts should happen — fast path skipped the file
	assert.Empty(t, store.upserted)
}

func TestScan_MtimeSlowPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "changed.txt", "new content")

	// Pre-populate store with a different (old) mtime
	store := newScannerMockStore()
	store.items["changed.txt"] = &Item{
		Path:       "changed.txt",
		Name:       "changed.txt",
		ItemType:   ItemTypeFile,
		LocalSize:  Int64Ptr(5),
		LocalMtime: Int64Ptr(1000000000000), // old timestamp
		LocalHash:  "oldhash",
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Upsert should happen with new hash
	require.Len(t, store.upserted, 1)
	item := store.items["changed.txt"]
	require.NotNil(t, item)
	assert.Equal(t, hashContent("new content"), item.LocalHash)
	assert.Equal(t, int64(11), *item.LocalSize)
}

func TestScan_OrphanDetection_SyncedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// No files on disk — the synced item is missing

	store := newScannerMockStore()
	store.syncedItems = []*Item{
		{
			Path:       "deleted.txt",
			Name:       "deleted.txt",
			ItemType:   ItemTypeFile,
			LocalSize:  Int64Ptr(10),
			LocalMtime: Int64Ptr(1000),
			LocalHash:  "somehash",
			SyncedHash: "syncedhash", // was previously synced
		},
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Local fields should be cleared (orphan)
	require.Len(t, store.upserted, 1)
	item := store.upserted[0]
	assert.Equal(t, "deleted.txt", item.Path)
	assert.Empty(t, item.LocalHash)
	assert.Nil(t, item.LocalSize)
	assert.Nil(t, item.LocalMtime)
}

func TestScan_OrphanDetection_S1Safety(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// No files on disk

	store := newScannerMockStore()
	store.syncedItems = []*Item{
		{
			Path:       "never-synced.txt",
			Name:       "never-synced.txt",
			ItemType:   ItemTypeFile,
			LocalSize:  Int64Ptr(10),
			LocalMtime: Int64Ptr(1000),
			LocalHash:  "somehash",
			SyncedHash: "", // never synced — no synced_hash
		},
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// S1 safety: no upsert should happen for unsynced items
	assert.Empty(t, store.upserted)
}

func TestScan_NestedDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "top.txt", "top")
	writeFile(t, root, "sub/middle.txt", "middle")
	writeFile(t, root, "sub/deep/bottom.txt", "bottom")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// 3 files + 2 directories (sub, sub/deep) tracked
	assert.Len(t, store.upserted, 5)
	assert.NotNil(t, store.items["top.txt"])
	assert.NotNil(t, store.items["sub/middle.txt"])
	assert.NotNil(t, store.items["sub/deep/bottom.txt"])
	assert.NotNil(t, store.items["sub"])
	assert.NotNil(t, store.items["sub/deep"])
}

func TestScan_EmptyDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)
	assert.Empty(t, store.upserted)
}

func TestScan_ContextCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create enough files that the walk has work to do
	for i := 0; i < 10; i++ {
		writeFile(t, root, filepath.Join("dir", "file"+string(rune('a'+i))+".txt"), "data")
	}

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := scanner.Scan(ctx, root)
	assert.Error(t, err)
}

func TestScan_TombstonedFileResurrection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "revived.txt", "I'm back")

	deletedAt := Int64Ptr(NowNano())
	store := newScannerMockStore()
	store.items["revived.txt"] = &Item{
		Path:      "revived.txt",
		Name:      "revived.txt",
		ItemType:  ItemTypeFile,
		IsDeleted: true,
		DeletedAt: deletedAt,
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	require.Len(t, store.upserted, 1)
	item := store.items["revived.txt"]
	require.NotNil(t, item)
	assert.False(t, item.IsDeleted)
	assert.Nil(t, item.DeletedAt)
	assert.Equal(t, hashContent("I'm back"), item.LocalHash)
	assert.NotNil(t, item.LocalSize)
	assert.NotNil(t, item.LocalMtime)
}

func TestScan_BrokenSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a symlink pointing to a non-existent target
	err := os.Symlink("/nonexistent/target", filepath.Join(root, "broken.txt"))
	require.NoError(t, err)

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), false) // follow symlinks

	err = scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Broken symlink should be skipped, no error
	assert.Empty(t, store.upserted)
}

func TestScan_FilterExcludesDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "node_modules/package.json", "pkg")
	writeFile(t, root, "src/main.go", "main")

	filter := newMockFilter()
	filter.excluded["node_modules"] = "directory excluded"

	store := newScannerMockStore()
	scanner := testScanner(t, store, filter, true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// src/ directory + src/main.go tracked; node_modules excluded by filter
	assert.Len(t, store.upserted, 2)
	assert.NotNil(t, store.items["src/main.go"])
	assert.NotNil(t, store.items["src"])
	assert.Nil(t, store.items["node_modules"])
}

func TestScan_OrphanStillPresent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "stillhere.txt", "present")

	store := newScannerMockStore()
	store.syncedItems = []*Item{
		{
			Path:       "stillhere.txt",
			Name:       "stillhere.txt",
			ItemType:   ItemTypeFile,
			SyncedHash: "hash",
		},
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// File still present: it should be scanned as new (no existing in items map) but orphan
	// detection should NOT clear its fields. The upsert from the walk is the only mutation.
	for _, item := range store.upserted {
		if item.Path == "stillhere.txt" {
			// Should have a local hash from the scan, NOT cleared fields
			assert.NotEmpty(t, item.LocalHash)
			assert.NotNil(t, item.LocalSize)
		}
	}
}

func TestComputeHash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "test.txt", "hello")

	hash, err := computeHash(filepath.Join(root, "test.txt"))
	require.NoError(t, err)
	assert.Equal(t, hashContent("hello"), hash)
}

func TestComputeHash_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := computeHash("/nonexistent/file.txt")
	assert.Error(t, err)
}

func TestJoinRelPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "file.txt", joinRelPath("", "file.txt"))
	assert.Equal(t, "dir/file.txt", joinRelPath("dir", "file.txt"))
	assert.Equal(t, "a/b/c.txt", joinRelPath("a/b", "c.txt"))
}

func TestNewScanner_NilLogger(t *testing.T) {
	t.Parallel()

	// Should not panic when logger is nil
	s := NewScanner("test-drive", newScannerMockStore(), newMockFilter(), true, nil)
	assert.NotNil(t, s)
	assert.NotNil(t, s.logger)
}

func TestScan_NosyncGuardSentinelError(t *testing.T) {
	t.Parallel()

	// Verify ErrNosyncGuard is a proper sentinel that can be checked with errors.Is
	root := t.TempDir()
	writeFile(t, root, ".nosync", "")

	scanner := testScanner(t, newScannerMockStore(), newMockFilter(), true)
	err := scanner.Scan(context.Background(), root)
	assert.True(t, errors.Is(err, ErrNosyncGuard))
}

func TestScan_PathTooLong(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create deeply nested path that exceeds 400 chars
	// Build a path with enough nesting to exceed maxPathChars
	longDir := root
	for i := 0; i < 20; i++ {
		seg := "abcdefghijklmnopqrstu" // 21 chars each
		longDir = filepath.Join(longDir, seg)
	}
	require.NoError(t, os.MkdirAll(longDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(longDir, "deep.txt"), []byte("x"), 0o644))

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// The deeply nested file path exceeds 400 chars and should be skipped
	// Total rel path: 20 segments * 22 chars (21 + /) + 8 = 448 chars
	// Verify it was excluded
	for _, item := range store.upserted {
		assert.Less(t, len(item.Path), maxPathChars+1, "no item should exceed max path length")
	}
}

func TestScan_OrphanDetection_ErrorFromStore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a store that returns an error from ListSyncedItems
	store := &errorStore{listSyncedErr: errors.New("db error")}
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "orphan detection failed")
}

// errorStore returns errors from specific methods for testing error paths.
type errorStore struct {
	scannerMockStore
	listSyncedErr error
}

func (e *errorStore) ListSyncedItems(_ context.Context) ([]*Item, error) {
	return nil, e.listSyncedErr
}

func TestScan_OrphanDetection_ContextCancel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	store := newScannerMockStore()
	store.syncedItems = []*Item{
		{Path: "a.txt", SyncedHash: "hash1"},
		{Path: "b.txt", SyncedHash: "hash2"},
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := scanner.Scan(ctx, root)
	assert.Error(t, err)
}

func TestScan_ExistingFileNilMtime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "nomtime.txt", "data")

	// Existing item with nil LocalMtime triggers the slow path
	store := newScannerMockStore()
	store.items["nomtime.txt"] = &Item{
		Path:       "nomtime.txt",
		Name:       "nomtime.txt",
		ItemType:   ItemTypeFile,
		LocalSize:  Int64Ptr(4),
		LocalMtime: nil, // nil mtime
		LocalHash:  "oldhash",
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Should have upserted with new hash (slow path because nil mtime != any mtime)
	require.Len(t, store.upserted, 1)
	assert.Equal(t, hashContent("data"), store.upserted[0].LocalHash)
}

func TestScan_StoreGetItemByPathError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "problem.txt", "data")

	store := &getItemErrorStore{
		scannerMockStore: scannerMockStore{items: make(map[string]*Item)},
		errPath:          "problem.txt",
	}
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "store lookup")
}

// getItemErrorStore returns an error for GetItemByPath on a specific path.
type getItemErrorStore struct {
	scannerMockStore
	errPath string
}

func (g *getItemErrorStore) GetItemByPath(_ context.Context, path string) (*Item, error) {
	if path == g.errPath {
		return nil, errors.New("db read error")
	}
	return g.scannerMockStore.GetItemByPath(context.Background(), path)
}

func TestScan_UnreadableDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a directory that we cannot read
	unreadable := filepath.Join(root, "noaccess")
	require.NoError(t, os.MkdirAll(unreadable, 0o755))
	writeFile(t, root, "noaccess/file.txt", "hidden")
	require.NoError(t, os.Chmod(unreadable, 0o000))

	t.Cleanup(func() {
		// Restore permissions so cleanup can remove the directory
		os.Chmod(unreadable, 0o755)
	})

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	// The walk should fail when trying to read the unreadable directory
	err := scanner.Scan(context.Background(), root)
	assert.Error(t, err)
}

// --- Directory tracking tests (C1 / B-043) ---

func TestScan_DirectoryTracking_NewLocalDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create directories with a file so the walk enters them
	require.NoError(t, os.MkdirAll(filepath.Join(root, "photos", "vacation"), 0o755))
	writeFile(t, root, "photos/vacation/beach.txt", "sand")

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// Both directories and the file should be tracked
	photos := store.items["photos"]
	require.NotNil(t, photos, "photos directory should be tracked")
	assert.Equal(t, ItemTypeFolder, photos.ItemType)
	assert.NotNil(t, photos.LocalMtime, "directory should have LocalMtime set")
	assert.Equal(t, "photos", photos.Name)

	vacation := store.items["photos/vacation"]
	require.NotNil(t, vacation, "nested directory should be tracked")
	assert.Equal(t, ItemTypeFolder, vacation.ItemType)
	assert.NotNil(t, vacation.LocalMtime)

	file := store.items["photos/vacation/beach.txt"]
	require.NotNil(t, file, "file should be tracked")
	assert.Equal(t, ItemTypeFile, file.ItemType)
}

func TestScan_DirectoryTracking_ResurrectedDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create directory on disk
	require.NoError(t, os.MkdirAll(filepath.Join(root, "docs"), 0o755))
	writeFile(t, root, "docs/readme.txt", "hello")

	// Pre-populate a tombstoned folder in store
	deletedAt := Int64Ptr(NowNano())
	store := newScannerMockStore()
	store.items["docs"] = &Item{
		Path:      "docs",
		Name:      "docs",
		ItemType:  ItemTypeFolder,
		ItemID:    "remote-id-123",
		IsDeleted: true,
		DeletedAt: deletedAt,
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	item := store.items["docs"]
	require.NotNil(t, item)
	assert.False(t, item.IsDeleted, "tombstone should be cleared")
	assert.Nil(t, item.DeletedAt, "deleted_at should be nil")
	assert.NotNil(t, item.LocalMtime, "LocalMtime should be set")
	assert.Equal(t, "remote-id-123", item.ItemID, "remote ID should be preserved")
}

func TestScan_DirectoryTracking_RemoteOnlyGetsLocalMtime(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create directory on disk
	require.NoError(t, os.MkdirAll(filepath.Join(root, "shared"), 0o755))
	writeFile(t, root, "shared/file.txt", "data")

	// Pre-populate a remote-only folder: has ItemID but no LocalMtime
	store := newScannerMockStore()
	store.items["shared"] = &Item{
		Path:     "shared",
		Name:     "shared",
		ItemType: ItemTypeFolder,
		ItemID:   "remote-shared-id",
		DriveID:  "drive-1",
	}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	item := store.items["shared"]
	require.NotNil(t, item)
	assert.NotNil(t, item.LocalMtime, "LocalMtime should now be set for remote-only folder found locally")
	assert.Equal(t, "remote-shared-id", item.ItemID, "remote ID should be preserved")
	assert.Equal(t, "drive-1", item.DriveID, "drive ID should be preserved")
}

func TestScan_FolderOrphanDetection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Do NOT create the "deleted-folder" directory on disk

	mtime := Int64Ptr(NowNano())
	folderItem := &Item{
		Path:       "deleted-folder",
		Name:       "deleted-folder",
		ItemType:   ItemTypeFolder,
		ItemID:     "remote-folder-id",
		LocalMtime: mtime,
	}

	store := newScannerMockStore()
	store.items["deleted-folder"] = folderItem
	// allActiveItems must include the folder for detectFolderOrphans to find it
	store.allActiveItems = []*Item{folderItem}

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	item := store.items["deleted-folder"]
	require.NotNil(t, item, "item should still exist in store")
	assert.Nil(t, item.LocalMtime, "LocalMtime should be cleared for deleted folder")
	assert.Equal(t, "remote-folder-id", item.ItemID, "remote ID should be preserved")
}

// TestScan_DirectoryTracking_FolderBoundaryWithReconciler (M2) verifies the
// delta-to-scan-to-reconcile pipeline for folders. A folder item created by
// the delta processor (has ItemID, no LocalMtime) gets LocalMtime set when the
// scanner finds the corresponding directory on disk, making it a D2 (both exist)
// item for the reconciler.
func TestScan_DirectoryTracking_FolderBoundaryWithReconciler(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Simulate a folder that arrived from delta: has remote fields, no local fields
	store := newScannerMockStore()
	store.items["projects"] = &Item{
		Path:     "projects",
		Name:     "projects",
		ItemType: ItemTypeFolder,
		DriveID:  "drive-abc",
		ItemID:   "item-xyz",
	}

	// Create the directory on disk (simulating it was downloaded or already existed)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "projects"), 0o755))
	writeFile(t, root, "projects/code.go", "package main")

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	item := store.items["projects"]
	require.NotNil(t, item)

	// After scan, folder should have both remote identity and local presence
	assert.Equal(t, "item-xyz", item.ItemID, "remote ItemID preserved from delta")
	assert.Equal(t, "drive-abc", item.DriveID, "remote DriveID preserved from delta")
	assert.NotNil(t, item.LocalMtime, "LocalMtime set by scanner")

	// Verify the reconciler would see this as D2 (both exist):
	// localExists = LocalMtime != nil → true
	// remoteExists = !IsDeleted && ItemID != "" → true
	assert.False(t, item.IsDeleted)
	assert.NotEmpty(t, item.ItemID)
}

// --- Error path tests for detectFolderOrphans and processDirectoryEntry ---

func TestDetectFolderOrphans_ListActiveError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	store := newScannerMockStore()
	store.listAllActiveErr = errors.New("db read error")

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "folder orphan detection failed")
}

func TestDetectFolderOrphans_ContextCancel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Populate active folder items so detectFolderOrphans has work to do.
	// The folder has LocalMtime set and was NOT visited during walk,
	// but the context is canceled before iteration proceeds.
	mtime := Int64Ptr(NowNano())
	folderItem := &Item{
		Path:       "ghost-folder",
		Name:       "ghost-folder",
		ItemType:   ItemTypeFolder,
		ItemID:     "folder-id",
		LocalMtime: mtime,
	}

	store := newScannerMockStore()
	store.allActiveItems = []*Item{folderItem}

	scanner := testScanner(t, store, newMockFilter(), true)

	ctx, cancel := context.WithCancel(context.Background())

	// Let the walk complete (empty root), then cancel before folder orphan detection loops.
	// Since the walk is empty, it will finish quickly; the cancel happens before
	// detectFolderOrphans iterates through items. We cancel before calling Scan
	// so the context check in detectFolderOrphans catches it.
	cancel()

	err := scanner.Scan(ctx, root)
	assert.Error(t, err)
}

func TestDetectFolderOrphans_UpsertError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Do NOT create "orphaned-dir" on disk — it should be detected as orphan.

	mtime := Int64Ptr(NowNano())
	folderItem := &Item{
		Path:       "orphaned-dir",
		Name:       "orphaned-dir",
		ItemType:   ItemTypeFolder,
		ItemID:     "folder-id",
		LocalMtime: mtime,
	}

	store := newScannerMockStore()
	store.items["orphaned-dir"] = folderItem
	store.allActiveItems = []*Item{folderItem}
	// UpsertItem will fail when the scanner tries to clear LocalMtime on the orphan.
	store.upsertItemErr = errors.New("write error")

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "folder orphan detection failed")
}

// --- Mock DirEntry/FileInfo for validateEntry tests ---
// APFS normalizes filenames, so we cannot create real files with invalid UTF-8 on macOS.
// These mocks let us exercise validateEntry's guards without touching the filesystem.

// scannerMockFileInfo implements os.FileInfo with configurable name and size.
type scannerMockFileInfo struct {
	name string
	size int64
}

func (fi *scannerMockFileInfo) Name() string      { return fi.name }
func (fi *scannerMockFileInfo) Size() int64       { return fi.size }
func (fi *scannerMockFileInfo) Mode() os.FileMode { return 0o644 }
func (fi *scannerMockFileInfo) ModTime() time.Time {
	return time.Now()
}

func (fi *scannerMockFileInfo) IsDir() bool { return false }
func (fi *scannerMockFileInfo) Sys() any    { return nil }

// scannerMockDirEntry implements os.DirEntry, delegating to scannerMockFileInfo.
type scannerMockDirEntry struct {
	name    string
	isDir   bool
	info    os.FileInfo
	infoErr error
}

func (de *scannerMockDirEntry) Name() string               { return de.name }
func (de *scannerMockDirEntry) IsDir() bool                { return de.isDir }
func (de *scannerMockDirEntry) Type() os.FileMode          { return 0 }
func (de *scannerMockDirEntry) Info() (os.FileInfo, error) { return de.info, de.infoErr }

// --- validateEntry edge-case tests ---

// TestValidateEntry_InvalidUTF8 verifies that validateEntry rejects filenames
// containing invalid UTF-8 byte sequences. We use mock types because macOS APFS
// normalizes filenames, making it impossible to create truly invalid names on disk.
func TestValidateEntry_InvalidUTF8(t *testing.T) {
	t.Parallel()

	badName := "file\xff\xfe.txt"
	entry := &scannerMockDirEntry{
		name: badName,
		info: &scannerMockFileInfo{name: badName, size: 0},
	}

	scanner := testScanner(t, newScannerMockStore(), newMockFilter(), true)

	err := scanner.validateEntry(badName, badName, entry)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid utf8")
}

// TestValidateEntry_PathTooLong verifies that validateEntry rejects entries whose
// relative path exceeds maxPathChars (400 characters, the OneDrive limit).
func TestValidateEntry_PathTooLong(t *testing.T) {
	t.Parallel()

	name := "ok.txt"
	// Build a relPath that exceeds maxPathChars (400)
	longPath := ""
	for len(longPath) <= maxPathChars {
		longPath += "abcdefghijklmnopqrstu/"
	}
	longPath += name

	entry := &scannerMockDirEntry{
		name: name,
		info: &scannerMockFileInfo{name: name, size: 0},
	}

	scanner := testScanner(t, newScannerMockStore(), newMockFilter(), true)

	err := scanner.validateEntry(name, longPath, entry)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path too long")
}

// TestScan_HandleNewFile_HashFailure verifies that scanner skips (logs and continues)
// when it cannot compute a hash for a new file (e.g., unreadable due to permissions).
// The file should NOT appear in the store because hashing is required for new files.
func TestScan_HandleNewFile_HashFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a file then make it unreadable so computeHash fails.
	filePath := writeFile(t, root, "unreadable.txt", "secret")
	require.NoError(t, os.Chmod(filePath, 0o000))

	t.Cleanup(func() {
		// Restore permissions so t.TempDir cleanup can remove the file.
		os.Chmod(filePath, 0o644)
	})

	store := newScannerMockStore()
	scanner := testScanner(t, store, newMockFilter(), true)

	// Scan should succeed overall — hash failures are logged and skipped, not fatal.
	err := scanner.Scan(context.Background(), root)
	require.NoError(t, err)

	// The unreadable file should NOT be tracked: hashing failed, so handleNewFile returned nil.
	assert.Nil(t, store.items["unreadable.txt"], "unreadable file should not be stored")
	assert.Empty(t, store.upserted, "no items should have been upserted")
}

func TestProcessDirectoryEntry_StoreLookupError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Create a real directory so the scanner walk reaches processDirectoryEntry.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "testdir"), 0o755))
	writeFile(t, root, "testdir/file.txt", "data")

	store := newScannerMockStore()
	store.getItemByPathErr = errors.New("db corruption")

	scanner := testScanner(t, store, newMockFilter(), true)

	err := scanner.Scan(context.Background(), root)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store lookup")
}
