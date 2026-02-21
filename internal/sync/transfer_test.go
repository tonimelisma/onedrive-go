package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// --- Transfer-specific mock types (prefixed to avoid collision) ---

// transferMockTransfer implements TransferClient with configurable delay and call counting.
type transferMockTransfer struct {
	downloadContent []byte
	downloadDelay   time.Duration
	downloadErr     error
	downloadCalls   atomic.Int32

	uploadedItem *graph.Item
	uploadDelay  time.Duration
	uploadErr    error
	uploadCalls  atomic.Int32

	sessionErr error
	chunkItem  *graph.Item
	chunkErr   error
}

func (m *transferMockTransfer) Download(ctx context.Context, _, _ string, w io.Writer) (int64, error) {
	m.downloadCalls.Add(1)

	if m.downloadDelay > 0 {
		select {
		case <-time.After(m.downloadDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	if m.downloadErr != nil {
		return 0, m.downloadErr
	}

	n, err := w.Write(m.downloadContent)

	return int64(n), err
}

func (m *transferMockTransfer) SimpleUpload(_ context.Context, _, _, _ string, r io.Reader, _ int64) (*graph.Item, error) {
	m.uploadCalls.Add(1)

	if m.uploadDelay > 0 {
		time.Sleep(m.uploadDelay)
	}

	if m.uploadErr != nil {
		return nil, m.uploadErr
	}

	_, _ = io.Copy(io.Discard, r)

	if m.uploadedItem != nil {
		return m.uploadedItem, nil
	}

	return &graph.Item{ID: "uploaded-id", ETag: "etag-up"}, nil
}

func (m *transferMockTransfer) CreateUploadSession(
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

func (m *transferMockTransfer) UploadChunk(
	_ context.Context, _ *graph.UploadSession, chunk io.Reader, _, _, _ int64,
) (*graph.Item, error) {
	if m.chunkErr != nil {
		return nil, m.chunkErr
	}

	_, _ = io.Copy(io.Discard, chunk)

	return m.chunkItem, nil
}

// newTransferTestExecutor creates an executor wired with transfer-specific mocks.
func newTransferTestExecutor(
	t *testing.T, syncRoot string, transfer *transferMockTransfer,
) *Executor {
	t.Helper()

	store := newExecutorMockStore()
	items := &executorMockItems{}

	return NewExecutor(store, items, transfer, syncRoot, nil, nil, testLogger(t))
}

// transferHash computes QuickXorHash of b and returns base64.
func transferHash(b []byte) string {
	h := quickxorhash.New()
	_, _ = h.Write(b)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// makeDownloadActions creates n download actions with content seeded into the mock.
func makeDownloadActions(n int, content []byte) []Action {
	hash := transferHash(content)
	size := int64(len(content))
	actions := make([]Action, n)

	for i := range n {
		actions[i] = Action{
			Type:    ActionDownload,
			DriveID: "d1",
			ItemID:  fmt.Sprintf("item-%d", i),
			Path:    fmt.Sprintf("file-%d.txt", i),
			Item: &Item{
				DriveID:      "d1",
				ItemID:       fmt.Sprintf("item-%d", i),
				Name:         fmt.Sprintf("file-%d.txt", i),
				QuickXorHash: hash,
				Size:         &size,
				RemoteMtime:  Int64Ptr(NowNano()),
				ItemType:     ItemTypeFile,
			},
		}
	}

	return actions
}

// --- Tests ---

func TestTransferManager_DownloadAll_Empty(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	report := &SyncReport{}
	err = tm.DownloadAll(context.Background(), nil, report)

	require.NoError(t, err)
	assert.Equal(t, 0, report.Downloaded)
}

func TestTransferManager_DownloadAll_Sequential(t *testing.T) {
	syncRoot := t.TempDir()
	content := []byte("download content")
	transfer := &transferMockTransfer{downloadContent: content}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	cfg := &config.TransfersConfig{
		ParallelDownloads: 1,
		ParallelUploads:   1,
		BandwidthLimit:    "0",
		TransferOrder:     "default",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := makeDownloadActions(3, content)
	report := &SyncReport{}
	err = tm.DownloadAll(context.Background(), actions, report)

	require.NoError(t, err)
	assert.Equal(t, 3, report.Downloaded)
	assert.Equal(t, int64(len(content))*3, report.BytesDownloaded)
	assert.Equal(t, int32(3), transfer.downloadCalls.Load())
}

func TestTransferManager_DownloadAll_Parallel(t *testing.T) {
	syncRoot := t.TempDir()
	content := []byte("parallel content")

	// Each download takes 100ms. With 4 workers and 8 items, parallel should
	// finish in ~200ms vs ~800ms serial. We check wall-clock < 600ms.
	transfer := &transferMockTransfer{
		downloadContent: content,
		downloadDelay:   100 * time.Millisecond,
	}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	cfg := &config.TransfersConfig{
		ParallelDownloads: 4,
		BandwidthLimit:    "0",
		TransferOrder:     "default",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := makeDownloadActions(8, content)
	report := &SyncReport{}

	start := time.Now()
	err = tm.DownloadAll(context.Background(), actions, report)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, 8, report.Downloaded)
	assert.Less(t, elapsed, 600*time.Millisecond, "parallel downloads should be faster than serial")
}

func TestTransferManager_DownloadAll_FatalAborts(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{
		downloadContent: []byte("content"),
		downloadErr:     graph.ErrUnauthorized, // fatal
	}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := makeDownloadActions(5, []byte("content"))
	report := &SyncReport{}
	err = tm.DownloadAll(context.Background(), actions, report)

	require.Error(t, err)
	assert.ErrorIs(t, err, graph.ErrUnauthorized)
}

func TestTransferManager_DownloadAll_SkipContinues(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{
		downloadContent: []byte("content"),
		downloadErr:     graph.ErrForbidden, // skip-tier
	}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	cfg := &config.TransfersConfig{
		ParallelDownloads: 1,
		BandwidthLimit:    "0",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := makeDownloadActions(3, []byte("content"))
	report := &SyncReport{}
	err = tm.DownloadAll(context.Background(), actions, report)

	require.NoError(t, err)
	assert.Equal(t, 0, report.Downloaded)
	assert.Equal(t, 3, report.Skipped)
	assert.Len(t, report.Errors, 3)
}

func TestTransferManager_DownloadAll_ContextCancel(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{
		downloadContent: []byte("content"),
		downloadDelay:   500 * time.Millisecond,
	}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	actions := makeDownloadActions(10, []byte("content"))
	report := &SyncReport{}
	err = tm.DownloadAll(ctx, actions, report)

	// Should error due to context cancellation (fatal error).
	require.Error(t, err)
}

func TestTransferManager_UploadAll_Simple(t *testing.T) {
	syncRoot := t.TempDir()

	// Create small files for upload.
	content := []byte("upload me")
	for i := range 3 {
		require.NoError(t, os.WriteFile(
			filepath.Join(syncRoot, fmt.Sprintf("up-%d.txt", i)), content, 0o644,
		))
	}

	transfer := &transferMockTransfer{}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	cfg := &config.TransfersConfig{
		ParallelUploads: 2,
		BandwidthLimit:  "0",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := make([]Action, 3)
	for i := range 3 {
		actions[i] = Action{
			Type:    ActionUpload,
			DriveID: "d1",
			ItemID:  fmt.Sprintf("up-item-%d", i),
			Path:    fmt.Sprintf("up-%d.txt", i),
			Item: &Item{
				DriveID:  "d1",
				ItemID:   fmt.Sprintf("up-item-%d", i),
				ParentID: "parent",
				Name:     fmt.Sprintf("up-%d.txt", i),
				ItemType: ItemTypeFile,
			},
		}
	}

	report := &SyncReport{}
	err = tm.UploadAll(context.Background(), actions, report)

	require.NoError(t, err)
	assert.Equal(t, 3, report.Uploaded)
	assert.Equal(t, int64(len(content))*3, report.BytesUploaded)
}

func TestTransferManager_UploadAll_FatalAborts(t *testing.T) {
	syncRoot := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "f.txt"), []byte("x"), 0o644))

	transfer := &transferMockTransfer{uploadErr: graph.ErrUnauthorized}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := []Action{{
		Type:    ActionUpload,
		DriveID: "d1",
		ItemID:  "item",
		Path:    "f.txt",
		Item:    &Item{DriveID: "d1", ItemID: "item", ParentID: "p", Name: "f.txt", ItemType: ItemTypeFile},
	}}

	report := &SyncReport{}
	err = tm.UploadAll(context.Background(), actions, report)

	require.Error(t, err)
	assert.ErrorIs(t, err, graph.ErrUnauthorized)
}

func TestTransferManager_UploadAll_Empty(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	report := &SyncReport{}
	err = tm.UploadAll(context.Background(), nil, report)

	require.NoError(t, err)
	assert.Equal(t, 0, report.Uploaded)
}

func TestTransferManager_ReportThreadSafety(t *testing.T) {
	// Verify that concurrent report mutations don't race.
	// Run with -race to detect data races.
	syncRoot := t.TempDir()
	content := []byte("race test")

	transfer := &transferMockTransfer{
		downloadContent: content,
		downloadDelay:   10 * time.Millisecond,
	}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	cfg := &config.TransfersConfig{
		ParallelDownloads: 8,
		BandwidthLimit:    "0",
	}

	tm, err := NewTransferManager(exec, cfg, testLogger(t))
	require.NoError(t, err)
	defer tm.Close()

	actions := makeDownloadActions(16, content)
	report := &SyncReport{}
	err = tm.DownloadAll(context.Background(), actions, report)

	require.NoError(t, err)
	assert.Equal(t, 16, report.Downloaded)
}

func TestNewTransferManager_NilConfig(t *testing.T) {
	syncRoot := t.TempDir()
	transfer := &transferMockTransfer{}
	exec := newTransferTestExecutor(t, syncRoot, transfer)

	tm, err := NewTransferManager(exec, nil, testLogger(t))
	require.NoError(t, err)
	require.NotNil(t, tm)

	assert.Equal(t, defaultDownloadWorkers, tm.downloadWorkers)
	assert.Equal(t, defaultUploadWorkers, tm.uploadWorkers)
	assert.Equal(t, "default", tm.transferOrder)
	assert.Nil(t, tm.limiter, "default bandwidth should be unlimited")
}

// --- Sort tests ---

func TestSortActions_SizeAsc(t *testing.T) {
	s1, s2, s3 := int64(300), int64(100), int64(200)
	actions := []Action{
		{Path: "big", Item: &Item{Size: &s1}},
		{Path: "small", Item: &Item{Size: &s2}},
		{Path: "mid", Item: &Item{Size: &s3}},
	}

	sortActions(actions, "size_asc")

	assert.Equal(t, "small", actions[0].Path)
	assert.Equal(t, "mid", actions[1].Path)
	assert.Equal(t, "big", actions[2].Path)
}

func TestSortActions_SizeDesc(t *testing.T) {
	s1, s2, s3 := int64(300), int64(100), int64(200)
	actions := []Action{
		{Path: "big", Item: &Item{Size: &s1}},
		{Path: "small", Item: &Item{Size: &s2}},
		{Path: "mid", Item: &Item{Size: &s3}},
	}

	sortActions(actions, "size_desc")

	assert.Equal(t, "big", actions[0].Path)
	assert.Equal(t, "mid", actions[1].Path)
	assert.Equal(t, "small", actions[2].Path)
}

func TestSortActions_NameAsc(t *testing.T) {
	actions := []Action{
		{Path: "charlie.txt"},
		{Path: "alpha.txt"},
		{Path: "bravo.txt"},
	}

	sortActions(actions, "name_asc")

	assert.Equal(t, "alpha.txt", actions[0].Path)
	assert.Equal(t, "bravo.txt", actions[1].Path)
	assert.Equal(t, "charlie.txt", actions[2].Path)
}

func TestSortActions_NameDesc(t *testing.T) {
	actions := []Action{
		{Path: "alpha.txt"},
		{Path: "charlie.txt"},
		{Path: "bravo.txt"},
	}

	sortActions(actions, "name_desc")

	assert.Equal(t, "charlie.txt", actions[0].Path)
	assert.Equal(t, "bravo.txt", actions[1].Path)
	assert.Equal(t, "alpha.txt", actions[2].Path)
}

func TestSortActions_Default(t *testing.T) {
	actions := []Action{
		{Path: "c.txt"},
		{Path: "a.txt"},
		{Path: "b.txt"},
	}

	sortActions(actions, "default")

	// Default = no reordering.
	assert.Equal(t, "c.txt", actions[0].Path)
	assert.Equal(t, "a.txt", actions[1].Path)
	assert.Equal(t, "b.txt", actions[2].Path)
}

func TestSortActions_NilSize(t *testing.T) {
	s1 := int64(100)
	actions := []Action{
		{Path: "with-size", Item: &Item{Size: &s1}},
		{Path: "nil-item"},
		{Path: "nil-size", Item: &Item{}},
	}

	// Should not panic on nil Item or nil Size.
	sortActions(actions, "size_asc")

	// nil sizes treated as 0, so they come first.
	assert.Equal(t, int64(0), actionSize(&actions[0]))
	assert.Equal(t, int64(0), actionSize(&actions[1]))
	assert.Equal(t, int64(100), actionSize(&actions[2]))
}
