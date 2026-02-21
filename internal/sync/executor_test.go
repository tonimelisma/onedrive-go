package sync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// --- Mock types (executor-prefixed to avoid collision with delta/reconciler mocks) ---

type executorMockStore struct {
	mu    gosync.Mutex     // protects all fields for thread-safe parallel transfer tests
	items map[string]*Item // keyed by "driveID/itemID"

	// Call recordings
	upsertCalls      []*Item
	markDeletedCalls []executorMarkDeletedCall
	recordConflicts  []*ConflictRecord
	checkpointCalled bool

	// Path-to-item lookup for GetItemByPath
	pathItems map[string]*Item // path -> item

	// Error injection
	upsertErr        error
	markDeletedErr   error
	deleteByKeyErr   error
	getItemByPathErr error
	cascadeErr       error
	conflictErr      error
}

type executorMarkDeletedCall struct {
	DriveID   string
	ItemID    string
	DeletedAt int64
}

func newExecutorMockStore() *executorMockStore {
	return &executorMockStore{
		items:     make(map[string]*Item),
		pathItems: make(map[string]*Item),
	}
}

func (s *executorMockStore) storeKey(driveID, itemID string) string {
	return driveID + "/" + itemID
}

func (s *executorMockStore) GetItem(_ context.Context, driveID, itemID string) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.items[s.storeKey(driveID, itemID)], nil
}

func (s *executorMockStore) GetItemByPath(_ context.Context, path string) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.getItemByPathErr != nil {
		return nil, s.getItemByPathErr
	}

	return s.pathItems[path], nil
}

func (s *executorMockStore) UpsertItem(_ context.Context, item *Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.upsertCalls = append(s.upsertCalls, item)
	s.items[s.storeKey(item.DriveID, item.ItemID)] = item

	return s.upsertErr
}

func (s *executorMockStore) MarkDeleted(_ context.Context, driveID, itemID string, deletedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markDeletedCalls = append(s.markDeletedCalls, executorMarkDeletedCall{
		DriveID:   driveID,
		ItemID:    itemID,
		DeletedAt: deletedAt,
	})

	return s.markDeletedErr
}

func (s *executorMockStore) DeleteItemByKey(_ context.Context, driveID, itemID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.deleteByKeyErr != nil {
		return s.deleteByKeyErr
	}

	delete(s.items, s.storeKey(driveID, itemID))

	return nil
}

func (s *executorMockStore) MaterializePath(_ context.Context, _, _ string) (string, error) {
	return "", nil
}

func (s *executorMockStore) CascadePathUpdate(_ context.Context, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cascadeErr
}

func (s *executorMockStore) RecordConflict(_ context.Context, record *ConflictRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recordConflicts = append(s.recordConflicts, record)

	return s.conflictErr
}

func (s *executorMockStore) Checkpoint() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.checkpointCalled = true

	return nil
}

// executorMockItems implements ItemClient for executor tests.
type executorMockItems struct {
	err error

	createFolderResult *graph.Item
	moveItemResult     *graph.Item
	deleteItemErr      error // separate so we can set it independently

	createFolderCalls []executorCreateFolderCall
	moveItemCalls     []executorMoveItemCall
	deleteItemCalls   []executorDeleteItemCall
}

type executorCreateFolderCall struct {
	DriveID  string
	ParentID string
	Name     string
}

type executorMoveItemCall struct {
	DriveID     string
	ItemID      string
	NewParentID string
	NewName     string
}

type executorDeleteItemCall struct {
	DriveID string
	ItemID  string
}

func (m *executorMockItems) GetItem(_ context.Context, _, _ string) (*graph.Item, error) {
	return nil, m.err
}

func (m *executorMockItems) ListChildren(_ context.Context, _, _ string) ([]graph.Item, error) {
	return nil, m.err
}

func (m *executorMockItems) CreateFolder(_ context.Context, driveID, parentID, name string) (*graph.Item, error) {
	m.createFolderCalls = append(m.createFolderCalls, executorCreateFolderCall{
		DriveID:  driveID,
		ParentID: parentID,
		Name:     name,
	})

	if m.err != nil {
		return nil, m.err
	}

	if m.createFolderResult != nil {
		return m.createFolderResult, nil
	}

	return &graph.Item{ID: "new-folder-id", Name: name, ETag: "etag-1", IsFolder: true}, nil
}

func (m *executorMockItems) MoveItem(_ context.Context, driveID, itemID, newParentID, newName string) (*graph.Item, error) {
	m.moveItemCalls = append(m.moveItemCalls, executorMoveItemCall{
		DriveID:     driveID,
		ItemID:      itemID,
		NewParentID: newParentID,
		NewName:     newName,
	})

	if m.err != nil {
		return nil, m.err
	}

	if m.moveItemResult != nil {
		return m.moveItemResult, nil
	}

	return &graph.Item{ID: itemID, Name: newName, ETag: "etag-moved"}, nil
}

func (m *executorMockItems) DeleteItem(_ context.Context, driveID, itemID string) error {
	m.deleteItemCalls = append(m.deleteItemCalls, executorDeleteItemCall{
		DriveID: driveID,
		ItemID:  itemID,
	})

	if m.deleteItemErr != nil {
		return m.deleteItemErr
	}

	return m.err
}

// executorMockTransfer implements TransferClient for executor tests.
type executorMockTransfer struct {
	downloadContent []byte // written to writer on Download
	uploadedItem    *graph.Item
	downloadErr     error
	uploadErr       error
	sessionErr      error
	// chunkItem is returned on every UploadChunk call; set to non-nil to signal completion.
	chunkItem *graph.Item
	chunkErr  error

	downloadCalls int
	uploadCalls   int
	chunkCalls    int
}

func (m *executorMockTransfer) Download(_ context.Context, _, _ string, w io.Writer) (int64, error) {
	m.downloadCalls++

	if m.downloadErr != nil {
		return 0, m.downloadErr
	}

	n, err := w.Write(m.downloadContent)

	return int64(n), err
}

func (m *executorMockTransfer) SimpleUpload(_ context.Context, _, _, _ string, r io.Reader, _ int64) (*graph.Item, error) {
	m.uploadCalls++

	if m.uploadErr != nil {
		return nil, m.uploadErr
	}

	_, _ = io.Copy(io.Discard, r) // consume reader so TeeReader feeds the hasher

	if m.uploadedItem != nil {
		return m.uploadedItem, nil
	}

	return &graph.Item{ID: "uploaded-id", ETag: "etag-up"}, nil
}

func (m *executorMockTransfer) CreateUploadSession(
	_ context.Context, _, _, _ string, _ int64, _ time.Time,
) (*graph.UploadSession, error) {
	if m.sessionErr != nil {
		return nil, m.sessionErr
	}

	return &graph.UploadSession{
		UploadURL:      "https://example.com/upload",
		ExpirationTime: time.Now().Add(time.Hour),
	}, nil
}

func (m *executorMockTransfer) UploadChunk(
	_ context.Context, _ *graph.UploadSession, chunk io.Reader, _, _, _ int64,
) (*graph.Item, error) {
	m.chunkCalls++

	if m.chunkErr != nil {
		return nil, m.chunkErr
	}

	_, _ = io.Copy(io.Discard, chunk)

	return m.chunkItem, nil
}

// --- Test helper builders ---

// newTestExecutorWithCfg creates an Executor using mock dependencies and returns all mocks.
func newTestExecutorWithCfg(t *testing.T, syncRoot string, cfg *config.SafetyConfig) (*Executor, *executorMockStore, *executorMockItems, *executorMockTransfer) {
	t.Helper()

	store := newExecutorMockStore()
	items := &executorMockItems{}
	transfer := &executorMockTransfer{}

	exec := NewExecutor(store, items, transfer, syncRoot, cfg, nil, testLogger(t))

	return exec, store, items, transfer
}

// newTestExecutor creates an Executor with a zero-value SafetyConfig.
func newTestExecutor(t *testing.T, syncRoot string) (*Executor, *executorMockStore, *executorMockItems, *executorMockTransfer) {
	t.Helper()

	return newTestExecutorWithCfg(t, syncRoot, &config.SafetyConfig{})
}

// executorHash computes QuickXorHash of b and returns base64.
func executorHash(b []byte) string {
	h := quickxorhash.New()
	_, _ = h.Write(b)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// --- FolderCreate tests ---

func TestExecutor_FolderCreate_Local(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:       ActionFolderCreate,
		DriveID:    "d1",
		ItemID:     "i1",
		Path:       "docs/notes",
		CreateSide: FolderCreateLocal,
		Item: &Item{
			DriveID:  "d1",
			ItemID:   "i1",
			Name:     "notes",
			ItemType: ItemTypeFolder,
			Path:     "docs/notes",
		},
	}

	plan := &ActionPlan{FolderCreates: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.FoldersCreated)

	// Directory should exist on disk.
	_, statErr := os.Stat(filepath.Join(syncRoot, "docs", "notes"))
	assert.NoError(t, statErr)

	// DB should be updated.
	require.Len(t, store.upsertCalls, 1)
	assert.NotNil(t, store.upsertCalls[0].LocalMtime)
}

func TestExecutor_FolderCreate_Remote(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, items, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:       ActionFolderCreate,
		DriveID:    "d1",
		ItemID:     "i1",
		Path:       "docs",
		CreateSide: FolderCreateRemote,
		Item: &Item{
			DriveID:  "d1",
			ItemID:   "i1",
			ParentID: "root-id",
			Name:     "docs",
			ItemType: ItemTypeFolder,
		},
	}

	plan := &ActionPlan{FolderCreates: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.FoldersCreated)

	// Graph API should have been called.
	require.Len(t, items.createFolderCalls, 1)
	assert.Equal(t, "root-id", items.createFolderCalls[0].ParentID)
	assert.Equal(t, "docs", items.createFolderCalls[0].Name)

	// DB updated with new ItemID from API response.
	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "new-folder-id", store.upsertCalls[0].ItemID)
}

func TestExecutor_FolderCreate_Remote_APIError_Skip(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, items, _ := newTestExecutor(t, syncRoot)

	items.err = graph.ErrForbidden

	action := Action{
		Type:       ActionFolderCreate,
		DriveID:    "d1",
		Path:       "docs",
		CreateSide: FolderCreateRemote,
		Item:       &Item{DriveID: "d1", Name: "docs"},
	}

	plan := &ActionPlan{FolderCreates: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier: not fatal
	assert.Equal(t, 0, report.FoldersCreated)
	assert.Equal(t, 1, report.Skipped)
	require.Len(t, report.Errors, 1)
	assert.Equal(t, ErrorSkip, report.Errors[0].Tier)
}

// --- Move tests ---

func TestExecutor_Move_Rename(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, items, _ := newTestExecutor(t, syncRoot)

	// Parent "." = sync root; seeded so GetItemByPath returns the root item.
	store.pathItems["."] = &Item{DriveID: "d1", ItemID: "root-id"}

	action := Action{
		Type:    ActionRemoteMove,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "old-name.txt",
		NewPath: "new-name.txt",
		Item: &Item{
			DriveID:  "d1",
			ItemID:   "file-id",
			ParentID: "root-id",
			Name:     "old-name.txt",
			ItemType: ItemTypeFile,
			Path:     "old-name.txt",
		},
	}

	plan := &ActionPlan{Moves: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Moved)

	require.Len(t, items.moveItemCalls, 1)
	assert.Equal(t, "new-name.txt", items.moveItemCalls[0].NewName)

	// DB path should be updated.
	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "new-name.txt", store.upsertCalls[0].Path)
}

func TestExecutor_Move_Error_Skip(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, items, _ := newTestExecutor(t, syncRoot)

	items.err = graph.ErrForbidden

	action := Action{
		Type:    ActionRemoteMove,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "old.txt",
		NewPath: "new.txt",
		Item:    &Item{DriveID: "d1", ItemID: "file-id", ParentID: "root"},
	}

	plan := &ActionPlan{Moves: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 0, report.Moved)
	assert.Equal(t, 1, report.Skipped)
}

// --- Download tests ---

func TestExecutor_Download_Success_HashVerify(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, transfer := newTestExecutor(t, syncRoot)

	content := []byte("hello, onedrive!")
	transfer.downloadContent = content

	expectedHash := executorHash(content)
	remoteSize := int64(len(content))

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "hello.txt",
		Item: &Item{
			DriveID:      "d1",
			ItemID:       "file-id",
			Name:         "hello.txt",
			QuickXorHash: expectedHash,
			Size:         &remoteSize,
			RemoteMtime:  Int64Ptr(time.Now().UnixNano()),
			ItemType:     ItemTypeFile,
		},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Downloaded)
	assert.Equal(t, int64(len(content)), report.BytesDownloaded)

	// File should exist with correct content.
	got, readErr := os.ReadFile(filepath.Join(syncRoot, "hello.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, content, got)

	// .partial must be cleaned up.
	_, statErr := os.Stat(filepath.Join(syncRoot, "hello.txt.partial"))
	assert.True(t, os.IsNotExist(statErr))

	// DB should have local+synced fields set.
	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	require.NotNil(t, item.LocalSize)
	assert.Equal(t, int64(len(content)), *item.LocalSize)
	assert.Equal(t, expectedHash, item.LocalHash)
	assert.Equal(t, expectedHash, item.SyncedHash)
	assert.NotNil(t, item.LastSyncedAt)
}

func TestExecutor_Download_HashMismatch_CleanupPartial(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.downloadContent = []byte("actual content")

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "data.bin",
		Item: &Item{
			DriveID:      "d1",
			ItemID:       "file-id",
			Name:         "data.bin",
			QuickXorHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // deliberate wrong hash
			ItemType:     ItemTypeFile,
		},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier: not fatal
	assert.Equal(t, 0, report.Downloaded)
	assert.Equal(t, 1, report.Skipped)

	// .partial must not linger.
	_, statErr := os.Stat(filepath.Join(syncRoot, "data.bin.partial"))
	assert.True(t, os.IsNotExist(statErr))
}

func TestExecutor_Download_APIError_Skip(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.downloadErr = graph.ErrForbidden

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "file-id", Name: "file.txt", ItemType: ItemTypeFile},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
	assert.Equal(t, ErrorSkip, report.Errors[0].Tier)
}

func TestExecutor_Download_FatalError_Abort(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.downloadErr = graph.ErrUnauthorized

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "file-id",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "file-id", Name: "file.txt", ItemType: ItemTypeFile},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	_, err := exec.Execute(context.Background(), plan)

	// Fatal error must propagate.
	require.Error(t, err)
	assert.True(t, errors.Is(err, graph.ErrUnauthorized))
}

// --- Upload tests ---

func TestExecutor_Upload_Simple(t *testing.T) {
	syncRoot := t.TempDir()

	// Write a small file (<= simpleUploadMax).
	content := []byte("small file content")
	localPath := filepath.Join(syncRoot, "small.txt")
	require.NoError(t, os.WriteFile(localPath, content, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionUpload,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "small.txt",
		Item: &Item{
			DriveID:  "d1",
			ItemID:   "item-id",
			ParentID: "parent-id",
			Name:     "small.txt",
			ItemType: ItemTypeFile,
		},
	}

	plan := &ActionPlan{Uploads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Uploaded)
	assert.Equal(t, int64(len(content)), report.BytesUploaded)
	assert.Equal(t, 1, transfer.uploadCalls)

	require.Len(t, store.upsertCalls, 1)
	assert.NotEmpty(t, store.upsertCalls[0].SyncedHash)
}

func TestExecutor_Upload_APIError_Skip(t *testing.T) {
	syncRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "file.txt"), []byte("content"), 0o644))

	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.uploadErr = graph.ErrForbidden

	action := Action{
		Type:    ActionUpload,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", ParentID: "p", Name: "file.txt", ItemType: ItemTypeFile},
	}

	plan := &ActionPlan{Uploads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Skipped)
}

func TestExecutor_Upload_Chunked(t *testing.T) {
	syncRoot := t.TempDir()

	// Write a file slightly larger than simpleUploadMax.
	const bigSize = simpleUploadMax + 1
	content := make([]byte, bigSize)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "big.bin"), content, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)

	// Return non-nil item from UploadChunk to signal upload completion.
	transfer.chunkItem = &graph.Item{ID: "chunked-id", ETag: "chunk-etag"}

	action := Action{
		Type:    ActionUpload,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "big.bin",
		Item: &Item{
			DriveID:  "d1",
			ItemID:   "item-id",
			ParentID: "p",
			Name:     "big.bin",
			ItemType: ItemTypeFile,
		},
	}

	plan := &ActionPlan{Uploads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Uploaded)
	assert.Equal(t, int64(bigSize), report.BytesUploaded)

	// Should have used chunked upload, not simple upload.
	assert.Equal(t, 0, transfer.uploadCalls)
	assert.Greater(t, transfer.chunkCalls, 0)

	require.Len(t, store.upsertCalls, 1)
	assert.Equal(t, "chunked-id", store.upsertCalls[0].ItemID)
}

// --- LocalDelete tests ---

func TestExecutor_LocalDelete_Unchanged(t *testing.T) {
	syncRoot := t.TempDir()

	content := []byte("stable content")
	localPath := filepath.Join(syncRoot, "stable.txt")
	require.NoError(t, os.WriteFile(localPath, content, 0o644))

	exec, store, _, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionLocalDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "stable.txt",
		Item: &Item{
			DriveID:    "d1",
			ItemID:     "item-id",
			SyncedHash: executorHash(content), // matches current file
		},
	}

	plan := &ActionPlan{LocalDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.LocalDeleted)

	// File should be gone.
	_, statErr := os.Stat(localPath)
	assert.True(t, os.IsNotExist(statErr))

	// DB should be marked deleted.
	require.Len(t, store.markDeletedCalls, 1)
	assert.Equal(t, "d1", store.markDeletedCalls[0].DriveID)
}

func TestExecutor_LocalDelete_Modified_ConflictBackup(t *testing.T) {
	syncRoot := t.TempDir()

	content := []byte("modified content")
	localPath := filepath.Join(syncRoot, "modified.txt")
	require.NoError(t, os.WriteFile(localPath, content, 0o644))

	exec, store, _, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionLocalDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "modified.txt",
		Item: &Item{
			DriveID:    "d1",
			ItemID:     "item-id",
			SyncedHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // mismatch: file was modified
		},
	}

	plan := &ActionPlan{LocalDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.LocalDeleted) // backup + mark = still counted

	// Original file renamed away (to conflict path).
	_, statErr := os.Stat(localPath)
	assert.True(t, os.IsNotExist(statErr))

	// Conflict file exists with new timestamp-based naming pattern (modified.conflict-*.txt).
	matches, globErr := filepath.Glob(filepath.Join(syncRoot, "modified.conflict-*.txt"))
	require.NoError(t, globErr)
	assert.Len(t, matches, 1, "expected one conflict file with timestamped name")

	// Conflict should be recorded.
	require.Len(t, store.recordConflicts, 1)
	assert.Equal(t, ConflictUnresolved, store.recordConflicts[0].Resolution)

	// DB should be marked deleted.
	require.Len(t, store.markDeletedCalls, 1)
}

func TestExecutor_LocalDelete_AlreadyGone(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, _ := newTestExecutor(t, syncRoot)

	// File does not exist.
	action := Action{
		Type:    ActionLocalDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "missing.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", SyncedHash: "any-hash"},
	}

	plan := &ActionPlan{LocalDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.LocalDeleted)
	require.Len(t, store.markDeletedCalls, 1)
}

// --- RemoteDelete tests ---

func TestExecutor_RemoteDelete_Success(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, items, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionRemoteDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "docs/file.txt",
	}

	plan := &ActionPlan{RemoteDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.RemoteDeleted)

	require.Len(t, items.deleteItemCalls, 1)
	assert.Equal(t, "item-id", items.deleteItemCalls[0].ItemID)
	require.Len(t, store.markDeletedCalls, 1)
}

func TestExecutor_RemoteDelete_NotFound_Succeeds(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, items, _ := newTestExecutor(t, syncRoot)

	// 404 = item already gone on remote = success.
	items.deleteItemErr = graph.ErrNotFound

	action := Action{
		Type:    ActionRemoteDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "gone.txt",
	}

	plan := &ActionPlan{RemoteDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.RemoteDeleted)
	require.Len(t, store.markDeletedCalls, 1)
}

func TestExecutor_RemoteDelete_Forbidden_Skip(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, items, _ := newTestExecutor(t, syncRoot)

	items.deleteItemErr = graph.ErrForbidden

	action := Action{
		Type:    ActionRemoteDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "protected.txt",
	}

	plan := &ActionPlan{RemoteDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 0, report.RemoteDeleted)
	assert.Equal(t, 1, report.Skipped)
}

// --- Conflict tests ---

// TestExecutor_Conflict_EditEdit verifies that an edit-edit conflict (F5):
// renames the local file to a conflict copy, downloads the remote version,
// and records a keep_both resolution.
func TestExecutor_Conflict_EditEdit(t *testing.T) {
	syncRoot := t.TempDir()

	// Local file that will be renamed to a conflict copy.
	content := []byte("local content")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "conflict.txt"), content, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)
	transfer.downloadContent = []byte("remote content") // simulates remote version

	item := &Item{
		DriveID:  "d1",
		ItemID:   "item-id",
		ParentID: "parent-id",
		Name:     "conflict.txt",
		Path:     "conflict.txt",
	}

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "conflict.txt",
		Item:    item,
		ConflictInfo: &ConflictRecord{
			DriveID:    "d1",
			ItemID:     "item-id",
			Path:       "conflict.txt",
			LocalHash:  "local-hash",
			RemoteHash: "remote-hash",
			Type:       ConflictEditEdit,
		},
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Conflicts)

	// Conflict recorded with keep_both resolution.
	require.Len(t, store.recordConflicts, 1)
	rc := store.recordConflicts[0]
	assert.Equal(t, ConflictKeepBoth, rc.Resolution)
	assert.NotEmpty(t, rc.ID)
	assert.Greater(t, rc.DetectedAt, int64(0))

	// After rename + download the file is recreated by executeDownload; verify download occurred.
	assert.Equal(t, 1, transfer.downloadCalls)
}

// TestExecutor_Conflict_EditDelete verifies that an edit-delete conflict (F9):
// does not rename the local file, uploads it to restore the remote, and records keep_both.
func TestExecutor_Conflict_EditDelete(t *testing.T) {
	syncRoot := t.TempDir()

	// Local file to be re-uploaded.
	content := []byte("local edit")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "deleted.txt"), content, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)

	item := &Item{
		DriveID:  "d1",
		ItemID:   "item-id",
		ParentID: "parent-id",
		Name:     "deleted.txt",
		Path:     "deleted.txt",
	}

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "deleted.txt",
		Item:    item,
		ConflictInfo: &ConflictRecord{
			DriveID:   "d1",
			ItemID:    "item-id",
			Path:      "deleted.txt",
			LocalHash: "local-hash",
			Type:      ConflictEditDelete,
		},
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Conflicts)

	// Conflict recorded with keep_both.
	require.Len(t, store.recordConflicts, 1)
	assert.Equal(t, ConflictKeepBoth, store.recordConflicts[0].Resolution)

	// Local file still exists (no rename for edit-delete).
	_, statErr := os.Stat(filepath.Join(syncRoot, "deleted.txt"))
	assert.NoError(t, statErr)

	// Upload was triggered to restore the remote.
	assert.Equal(t, 1, transfer.uploadCalls)
}

// TestExecutor_Conflict_NilConflictInfo verifies that a conflict action with nil
// ConflictInfo is treated as a skip-tier error.
func TestExecutor_Conflict_NilConflictInfo(t *testing.T) {
	syncRoot := t.TempDir()
	// Use only the executor; name store and items to avoid dogsled (>2 blank identifiers).
	exec, store, items, _ := newTestExecutor(t, syncRoot)
	_, _ = store, items

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "file.txt",
		// ConflictInfo intentionally nil.
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier, not fatal
	assert.Equal(t, 0, report.Conflicts)
	assert.Equal(t, 1, report.Skipped)
}

// TestExecutor_Conflict_DownloadSubActionFails verifies that when the sub-action
// download fails after the conflict is recorded, the error propagates as a skip.
func TestExecutor_Conflict_DownloadSubActionFails(t *testing.T) {
	syncRoot := t.TempDir()

	// Local file exists so the rename succeeds.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "fail.txt"), []byte("local"), 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)
	transfer.downloadErr = graph.ErrForbidden // download will fail

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "fail.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", ParentID: "p", Name: "fail.txt", Path: "fail.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:   "d1",
			ItemID:    "item-id",
			Path:      "fail.txt",
			LocalHash: "local-hash",
			Type:      ConflictEditEdit,
		},
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier, not fatal
	assert.Equal(t, 0, report.Conflicts, "conflict should not count as success")
	assert.Equal(t, 1, report.Skipped)

	// Conflict was recorded before the download failed.
	require.Len(t, store.recordConflicts, 1)
	assert.Equal(t, ConflictKeepBoth, store.recordConflicts[0].Resolution)
}

// TestExecutor_Conflict_RecordConflictFails verifies that when RecordConflict
// returns an error, the entire conflict action is skipped.
func TestExecutor_Conflict_RecordConflictFails(t *testing.T) {
	syncRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "rec.txt"), []byte("local"), 0o644))

	exec, store, _, _ := newTestExecutor(t, syncRoot)
	store.conflictErr = fmt.Errorf("db write failed")

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "rec.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", ParentID: "p", Name: "rec.txt", Path: "rec.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:   "d1",
			ItemID:    "item-id",
			Path:      "rec.txt",
			LocalHash: "local-hash",
			Type:      ConflictEditEdit,
		},
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier, not fatal
	assert.Equal(t, 0, report.Conflicts)
	assert.Equal(t, 1, report.Skipped)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Err.Error(), "record conflict")
}

// TestExecutor_Conflict_UploadSubActionFails verifies that when the sub-action
// upload fails for an edit-delete conflict, the error propagates as a skip.
func TestExecutor_Conflict_UploadSubActionFails(t *testing.T) {
	syncRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "upfail.txt"), []byte("content"), 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)
	transfer.uploadErr = graph.ErrForbidden // upload will fail

	action := Action{
		Type:    ActionConflict,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "upfail.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", ParentID: "p", Name: "upfail.txt", Path: "upfail.txt"},
		ConflictInfo: &ConflictRecord{
			DriveID:   "d1",
			ItemID:    "item-id",
			Path:      "upfail.txt",
			LocalHash: "local-hash",
			Type:      ConflictEditDelete,
		},
	}

	plan := &ActionPlan{Conflicts: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err) // skip-tier, not fatal
	assert.Equal(t, 0, report.Conflicts)
	assert.Equal(t, 1, report.Skipped)

	// Conflict was recorded before the upload failed.
	require.Len(t, store.recordConflicts, 1)
}

// --- SyncedUpdate tests ---

func TestExecutor_SyncedUpdate(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, _ := newTestExecutor(t, syncRoot)

	localSize := int64(100)
	localMtime := Int64Ptr(NowNano())

	action := Action{
		Type:    ActionUpdateSynced,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "file.txt",
		Item: &Item{
			DriveID:    "d1",
			ItemID:     "item-id",
			Name:       "file.txt",
			LocalSize:  &localSize,
			LocalMtime: localMtime,
			LocalHash:  "local-hash",
		},
	}

	plan := &ActionPlan{SyncedUpdates: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.SyncedUpdates)

	require.Len(t, store.upsertCalls, 1)
	item := store.upsertCalls[0]
	assert.Equal(t, "local-hash", item.SyncedHash)
	assert.Equal(t, localMtime, item.SyncedMtime)
	assert.NotNil(t, item.LastSyncedAt)
}

// --- Cleanup tests ---

func TestExecutor_Cleanup(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionCleanup,
		DriveID: "d1",
		ItemID:  "stale-item",
		Path:    "stale.txt",
	}

	plan := &ActionPlan{Cleanups: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Cleanups)

	require.Len(t, store.markDeletedCalls, 1)
	assert.Equal(t, "stale-item", store.markDeletedCalls[0].ItemID)
}

// --- Error classification tests ---

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantTier ErrorTier
	}{
		{"nil", nil, ErrorSkip},
		{"context canceled", context.Canceled, ErrorFatal},
		{"context deadline", context.DeadlineExceeded, ErrorFatal},
		{"unauthorized", graph.ErrUnauthorized, ErrorFatal},
		{"not logged in", graph.ErrNotLoggedIn, ErrorFatal},
		{"throttled", graph.ErrThrottled, ErrorRetryable},
		{"server error", graph.ErrServerError, ErrorRetryable},
		{"forbidden", graph.ErrForbidden, ErrorSkip},
		{"bad request", graph.ErrBadRequest, ErrorSkip},
		{"locked", graph.ErrLocked, ErrorSkip},
		{"not found", graph.ErrNotFound, ErrorSkip},
		{"no download url", graph.ErrNoDownloadURL, ErrorSkip},
		{"unknown", errors.New("random error"), ErrorSkip},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantTier, classifyError(tc.err))
		})
	}
}

func TestClassifyError_WrappedErrors(t *testing.T) {
	// errors.Is must chain through fmt.Errorf %w wrapping.
	assert.Equal(t, ErrorFatal, classifyError(fmt.Errorf("outer: %w", graph.ErrUnauthorized)))
	assert.Equal(t, ErrorRetryable, classifyError(fmt.Errorf("outer: %w", graph.ErrThrottled)))
	assert.Equal(t, ErrorSkip, classifyError(fmt.Errorf("outer: %w", graph.ErrForbidden)))
}

// --- Phase ordering / context cancellation ---

func TestExecutor_ContextCancellation_BeforeDownloadPhase(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before execution begins

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "file.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item-id", Name: "file.txt"},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	_, err := exec.Execute(ctx, plan)

	// Context canceled before download phase: executor returns the context error.
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, transfer.downloadCalls) // phase never started
}

func TestExecutor_Checkpoint_CalledAfterExecution(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, _, _ := newTestExecutor(t, syncRoot)

	plan := &ActionPlan{}
	_, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.True(t, store.checkpointCalled)
}

func TestExecutor_MultipleErrors_AllRecorded(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.downloadErr = graph.ErrForbidden

	plan := &ActionPlan{
		Downloads: []Action{
			{
				Type: ActionDownload, DriveID: "d1", ItemID: "i1", Path: "a.txt",
				Item: &Item{DriveID: "d1", ItemID: "i1", Name: "a.txt"},
			},
			{
				Type: ActionDownload, DriveID: "d1", ItemID: "i2", Path: "b.txt",
				Item: &Item{DriveID: "d1", ItemID: "i2", Name: "b.txt"},
			},
		},
	}

	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 2, report.Skipped)
	assert.Len(t, report.Errors, 2)
}

// --- Integration test: real filesystem + real SQLite ---

func TestExecutor_Integration_Download(t *testing.T) {
	// End-to-end test using real SQLiteStore (in-memory) and real temp dir.
	// The graph transfer is mocked to serve canned content.
	syncRoot := t.TempDir()
	realStore := newTestStore(t)

	content := []byte("integration test content for download")
	expectedHash := executorHash(content)
	remoteSize := int64(len(content))
	now := NowNano()

	// Seed the item in the real DB.
	ctx := context.Background()
	item := &Item{
		DriveID:      "test-drive",
		ItemID:       "test-item",
		ParentID:     "root",
		Name:         "download.txt",
		ItemType:     ItemTypeFile,
		Path:         "download.txt",
		QuickXorHash: expectedHash,
		Size:         &remoteSize,
		RemoteMtime:  Int64Ptr(now),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, realStore.UpsertItem(ctx, item))

	transfer := &executorMockTransfer{downloadContent: content}
	items := &executorMockItems{}
	exec := NewExecutor(realStore, items, transfer, syncRoot, &config.SafetyConfig{}, nil, testLogger(t))

	plan := &ActionPlan{Downloads: []Action{{
		Type:    ActionDownload,
		DriveID: item.DriveID,
		ItemID:  item.ItemID,
		Path:    item.Path,
		Item:    item,
	}}}

	report, err := exec.Execute(ctx, plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Downloaded)

	// File exists on disk with correct content.
	got, readErr := os.ReadFile(filepath.Join(syncRoot, "download.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, content, got)

	// .partial cleaned up.
	_, partialErr := os.Stat(filepath.Join(syncRoot, "download.txt.partial"))
	assert.True(t, os.IsNotExist(partialErr))

	// DB state reflects completed download.
	updated, getErr := realStore.GetItem(ctx, item.DriveID, item.ItemID)
	require.NoError(t, getErr)
	require.NotNil(t, updated)
	assert.Equal(t, expectedHash, updated.LocalHash)
	assert.Equal(t, expectedHash, updated.SyncedHash)
	require.NotNil(t, updated.LocalSize)
	assert.Equal(t, int64(len(content)), *updated.LocalSize)
	assert.NotNil(t, updated.LastSyncedAt)
}

// --- Transfer pipeline integration (SetTransferManager + Execute) ---

// TestExecutor_Execute_WithTransferManager verifies that Execute() dispatches
// downloads and uploads through the TransferManager pool when SetTransferManager is called.
// This covers the `if e.transferMgr != nil` branches in Execute().
func TestExecutor_Execute_WithTransferManager(t *testing.T) {
	syncRoot := t.TempDir()

	dlContent := []byte("pool download content")
	ulContent := []byte("pool upload content")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "upload.txt"), ulContent, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)
	transfer.downloadContent = dlContent

	// Wire TransferManager with 2 download workers and 2 upload workers.
	cfg := &config.TransfersConfig{
		ParallelDownloads: 2,
		ParallelUploads:   2,
		BandwidthLimit:    "0",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	exec.SetTransferManager(tm)

	dlHash := executorHash(dlContent)
	dlSize := int64(len(dlContent))

	plan := &ActionPlan{
		Downloads: []Action{{
			Type:    ActionDownload,
			DriveID: "d1",
			ItemID:  "dl-item",
			Path:    "downloaded.txt",
			Item: &Item{
				DriveID:      "d1",
				ItemID:       "dl-item",
				Name:         "downloaded.txt",
				QuickXorHash: dlHash,
				Size:         &dlSize,
				RemoteMtime:  Int64Ptr(NowNano()),
				ItemType:     ItemTypeFile,
			},
		}},
		Uploads: []Action{{
			Type:    ActionUpload,
			DriveID: "d1",
			ItemID:  "ul-item",
			Path:    "upload.txt",
			Item: &Item{
				DriveID:  "d1",
				ItemID:   "ul-item",
				ParentID: "parent",
				Name:     "upload.txt",
				ItemType: ItemTypeFile,
			},
		}},
	}

	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Downloaded)
	assert.Equal(t, int64(len(dlContent)), report.BytesDownloaded)
	assert.Equal(t, 1, report.Uploaded)
	assert.Equal(t, int64(len(ulContent)), report.BytesUploaded)

	// Verify files on disk.
	got, readErr := os.ReadFile(filepath.Join(syncRoot, "downloaded.txt"))
	require.NoError(t, readErr)
	assert.Equal(t, dlContent, got)

	// Verify DB state was updated for both operations.
	assert.GreaterOrEqual(t, len(store.upsertCalls), 2)
}

// --- Chunk size config tests ---

// TestExecutor_LocalDelete_ConflictRenameFails verifies that when a local delete triggers
// a conflict backup (hash mismatch) but the rename fails (e.g., parent directory is read-only),
// the error is classified as skip-tier and reported without aborting the sync.
func TestExecutor_LocalDelete_ConflictRenameFails(t *testing.T) {
	syncRoot := t.TempDir()

	// Create file inside a subdirectory so we can make the directory read-only.
	subDir := filepath.Join(syncRoot, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	content := []byte("locally modified content")
	localPath := filepath.Join(subDir, "file.txt")
	require.NoError(t, os.WriteFile(localPath, content, 0o644))

	// Make sub/ read-only so os.Rename fails when trying to create the conflict copy.
	require.NoError(t, os.Chmod(subDir, 0o555))

	t.Cleanup(func() {
		// Restore permissions so t.TempDir cleanup can remove the directory.
		os.Chmod(subDir, 0o755)
	})

	exec, store, _, _ := newTestExecutor(t, syncRoot)

	action := Action{
		Type:    ActionLocalDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "sub/file.txt",
		Item: &Item{
			DriveID:    "d1",
			ItemID:     "item-id",
			SyncedHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // mismatch triggers conflict path
		},
	}

	plan := &ActionPlan{LocalDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	// Skip-tier error: the sync continues but this action is recorded as skipped.
	require.NoError(t, err)
	assert.Equal(t, 0, report.LocalDeleted)
	assert.Equal(t, 1, report.Skipped)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Err.Error(), "backup conflict file")

	// No conflict should have been recorded since the rename failed before RecordConflict.
	assert.Empty(t, store.recordConflicts)
}

// TestExecutor_LocalDelete_RecordConflictFails verifies that when a local delete triggers
// a conflict backup and the rename succeeds, but RecordConflict returns a DB error,
// the error is classified as skip-tier and reported.
func TestExecutor_LocalDelete_RecordConflictFails(t *testing.T) {
	syncRoot := t.TempDir()

	content := []byte("locally modified for conflict")
	localPath := filepath.Join(syncRoot, "conflict-record.txt")
	require.NoError(t, os.WriteFile(localPath, content, 0o644))

	exec, store, _, _ := newTestExecutor(t, syncRoot)
	store.conflictErr = fmt.Errorf("db write failed")

	action := Action{
		Type:    ActionLocalDelete,
		DriveID: "d1",
		ItemID:  "item-id",
		Path:    "conflict-record.txt",
		Item: &Item{
			DriveID:    "d1",
			ItemID:     "item-id",
			SyncedHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA=", // mismatch triggers conflict path
		},
	}

	plan := &ActionPlan{LocalDeletes: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	// Skip-tier error: rename succeeded but RecordConflict failed.
	require.NoError(t, err)
	assert.Equal(t, 0, report.LocalDeleted)
	assert.Equal(t, 1, report.Skipped)
	require.Len(t, report.Errors, 1)
	assert.Contains(t, report.Errors[0].Err.Error(), "record delete conflict")

	// Original file should be gone (renamed to conflict copy).
	_, statErr := os.Stat(localPath)
	assert.True(t, os.IsNotExist(statErr))

	// Conflict copy should exist on disk even though DB recording failed.
	matches, globErr := filepath.Glob(filepath.Join(syncRoot, "conflict-record.conflict-*.txt"))
	require.NoError(t, globErr)
	assert.Len(t, matches, 1, "conflict copy should exist despite DB failure")
}

func TestNewExecutor_ChunkSize_Default(t *testing.T) {
	store := newExecutorMockStore()
	exec := NewExecutor(store, &executorMockItems{}, &executorMockTransfer{}, "/tmp", nil, nil, testLogger(t))
	assert.Equal(t, int64(uploadChunkSize), exec.chunkSize)
}

func TestNewExecutor_ChunkSize_FromConfig(t *testing.T) {
	store := newExecutorMockStore()
	cfg := &config.TransfersConfig{ChunkSize: "5MiB"}
	exec := NewExecutor(store, &executorMockItems{}, &executorMockTransfer{}, "/tmp", nil, cfg, testLogger(t))
	assert.Equal(t, int64(5_242_880), exec.chunkSize)
}

func TestNewExecutor_ChunkSize_EmptyFallback(t *testing.T) {
	store := newExecutorMockStore()
	cfg := &config.TransfersConfig{ChunkSize: ""}
	exec := NewExecutor(store, &executorMockItems{}, &executorMockTransfer{}, "/tmp", nil, cfg, testLogger(t))
	assert.Equal(t, int64(uploadChunkSize), exec.chunkSize)
}

func TestNewExecutor_ChunkSize_InvalidFallback(t *testing.T) {
	store := newExecutorMockStore()
	cfg := &config.TransfersConfig{ChunkSize: "garbage"}
	exec := NewExecutor(store, &executorMockItems{}, &executorMockTransfer{}, "/tmp", nil, cfg, testLogger(t))
	assert.Equal(t, int64(uploadChunkSize), exec.chunkSize, "invalid chunk size should fall back to default")
}

// --- B-050: Stale row cleanup tests ---

// TestExecutor_Upload_CleansStaleRow verifies that after upload assigns a new ItemID,
// the stale scanner-originated row (with empty ItemID) is deleted from the store.
func TestExecutor_Upload_CleansStaleRow(t *testing.T) {
	syncRoot := t.TempDir()

	content := []byte("new local file")
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "new.txt"), content, 0o644))

	exec, store, _, transfer := newTestExecutor(t, syncRoot)

	// Simulate a scanner-originated row with empty ItemID.
	scannerItem := &Item{
		DriveID:  "d1",
		ItemID:   "",
		ParentID: "parent-id",
		Name:     "new.txt",
		ItemType: ItemTypeFile,
		Path:     "new.txt",
	}
	store.items[store.storeKey("d1", "")] = scannerItem

	// Upload returns a server-assigned ID.
	transfer.uploadedItem = &graph.Item{ID: "server-assigned-id", ETag: "etag-new"}

	action := Action{
		Type:    ActionUpload,
		DriveID: "d1",
		ItemID:  "",
		Path:    "new.txt",
		Item:    scannerItem,
	}

	plan := &ActionPlan{Uploads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.Uploaded)

	// New row should exist with server-assigned ID.
	newRow := store.items[store.storeKey("d1", "server-assigned-id")]
	require.NotNil(t, newRow, "new row with server ID should exist")
	assert.Equal(t, "server-assigned-id", newRow.ItemID)

	// Stale scanner row should be deleted.
	staleRow := store.items[store.storeKey("d1", "")]
	assert.Nil(t, staleRow, "stale scanner row should be deleted")
}

// TestExecutor_FolderCreateRemote_CleansStaleRow verifies that after remote folder
// creation assigns a new ItemID, the stale scanner row is deleted.
func TestExecutor_FolderCreateRemote_CleansStaleRow(t *testing.T) {
	syncRoot := t.TempDir()
	exec, store, items, _ := newTestExecutor(t, syncRoot)

	// Simulate a scanner-originated folder row with empty ItemID.
	scannerItem := &Item{
		DriveID:  "d1",
		ItemID:   "",
		ParentID: "root-id",
		Name:     "docs",
		ItemType: ItemTypeFolder,
	}
	store.items[store.storeKey("d1", "")] = scannerItem

	items.createFolderResult = &graph.Item{ID: "server-folder-id", Name: "docs", ETag: "etag-f", IsFolder: true}

	action := Action{
		Type:       ActionFolderCreate,
		DriveID:    "d1",
		ItemID:     "",
		Path:       "docs",
		CreateSide: FolderCreateRemote,
		Item:       scannerItem,
	}

	plan := &ActionPlan{FolderCreates: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, 1, report.FoldersCreated)

	// New row should exist with server-assigned ID.
	newRow := store.items[store.storeKey("d1", "server-folder-id")]
	require.NotNil(t, newRow, "new row with server folder ID should exist")

	// Stale scanner row should be deleted.
	staleRow := store.items[store.storeKey("d1", "")]
	assert.Nil(t, staleRow, "stale scanner row should be deleted")
}

// TestDispatchPhase_ErrorRetryable_RecordedAsSkipped verifies that ErrorRetryable errors
// (e.g., graph.ErrThrottled) are handled as skips in the current implementation.
// Full retry logic is deferred to Phase 5 (B-048).
func TestDispatchPhase_ErrorRetryable_RecordedAsSkipped(t *testing.T) {
	syncRoot := t.TempDir()
	exec, _, _, transfer := newTestExecutor(t, syncRoot)

	transfer.downloadErr = graph.ErrThrottled

	action := Action{
		Type:    ActionDownload,
		DriveID: "d1",
		ItemID:  "throttled-item",
		Path:    "throttled.txt",
		Item:    &Item{DriveID: "d1", ItemID: "throttled-item", Name: "throttled.txt", ItemType: ItemTypeFile},
	}

	plan := &ActionPlan{Downloads: []Action{action}}
	report, err := exec.Execute(context.Background(), plan)

	// ErrorRetryable is not fatal — handled as skip until Phase 5 adds retry.
	require.NoError(t, err)
	assert.Equal(t, 0, report.Downloaded)
	assert.Equal(t, 1, report.Skipped)
	require.Len(t, report.Errors, 1)
	assert.Equal(t, ErrorRetryable, report.Errors[0].Tier)
}
