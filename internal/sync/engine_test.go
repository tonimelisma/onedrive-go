package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Composite mock implementing DeltaFetcher + ItemClient + Downloader + Uploader
//
// Engine requires all 4 interfaces (unlike Executor, which takes them
// individually), so a single mock is pragmatic here. Executor tests split
// mocks by interface because each test exercises only 1-2 interfaces.
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ DeltaFetcher = (*engineMockClient)(nil)
	_ ItemClient   = (*engineMockClient)(nil)
	_ Downloader   = (*engineMockClient)(nil)
	_ Uploader     = (*engineMockClient)(nil)
)

type engineMockClient struct {
	// DeltaFetcher
	deltaFn func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)

	// ItemClient
	getItemFn      func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn     func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn   func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *engineMockClient) Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
	if m.deltaFn != nil {
		return m.deltaFn(ctx, driveID, token)
	}

	return &graph.DeltaPage{DeltaLink: "delta-token-1"}, nil
}

func (m *engineMockClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *engineMockClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *engineMockClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return &graph.Item{ID: "new-folder-id"}, nil
}

func (m *engineMockClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *engineMockClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *engineMockClient) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	// Write some content so the file has data.
	n, err := w.Write([]byte("downloaded-content"))

	return int64(n), err
}

func (m *engineMockClient) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return &graph.Item{
		ID:           "uploaded-item-id",
		Name:         name,
		Size:         size,
		QuickXorHash: "abc123hash",
	}, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

const engineTestDriveID = "0000000000000001"

// newTestEngine creates an Engine backed by a temp dir with real SQLite
// and the given mock client. Returns the engine and sync root path.
func newTestEngine(t *testing.T, mock *engineMockClient) (*Engine, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	if err := os.MkdirAll(syncRoot, 0o755); err != nil {
		t.Fatalf("creating sync root: %v", err)
	}

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(&EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	// Override executor sleep to be instant in tests.
	eng.execCfg.sleepFunc = func(_ context.Context, _ time.Duration) error { return nil }

	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("Engine.Close: %v", err)
		}
	})

	return eng, syncRoot
}

// deltaPageWithItems returns a DeltaPage with the given items and a delta link.
func deltaPageWithItems(items []graph.Item, deltaLink string) *graph.DeltaPage {
	return &graph.DeltaPage{
		Items:     items,
		DeltaLink: deltaLink,
	}
}

// writeLocalFile creates a file in syncRoot for local observer to find.
func writeLocalFile(t *testing.T, syncRoot, relPath, content string) {
	t.Helper()

	absPath := filepath.Join(syncRoot, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// seedBaseline commits outcomes and an optional delta token to the baseline,
// using the per-outcome CommitOutcome API (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *BaselineManager, ctx context.Context, outcomes []Outcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		if err := mgr.CommitOutcome(ctx, &outcomes[i], 0); err != nil {
			t.Fatalf("seed CommitOutcome[%d]: %v", i, err)
		}
	}

	if deltaToken != "" {
		if err := mgr.CommitDeltaToken(ctx, deltaToken, engineTestDriveID); err != nil {
			t.Fatalf("seed CommitDeltaToken: %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRunOnce_NoChanges(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			// Return root only — no content changes.
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if report.Mode != SyncBidirectional {
		t.Errorf("mode = %v, want bidirectional", report.Mode)
	}

	total := report.Downloads + report.Uploads + report.LocalDeletes +
		report.RemoteDeletes + report.FolderCreates + report.Moves +
		report.Conflicts + report.SyncedUpdates + report.Cleanups
	if total != 0 {
		t.Errorf("expected zero actions, got total = %d", total)
	}

	if report.Succeeded != 0 || report.Failed != 0 {
		t.Errorf("succeeded=%d failed=%d, want both 0", report.Succeeded, report.Failed)
	}
}

func TestRunOnce_DownloadOnly_SkipsLocalScan(t *testing.T) {
	t.Parallel()

	// Place a local file that would generate an upload event if scanned.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveid.New(engineTestDriveID)},
			}, "token-1"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	writeLocalFile(t, syncRoot, "local-only.txt", "should not be uploaded")

	ctx := context.Background()

	report, err := eng.RunOnce(ctx, SyncDownloadOnly, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The local file should not appear in uploads because local scan was skipped.
	if report.Uploads != 0 {
		t.Errorf("uploads = %d, want 0 (local scan should be skipped in download-only mode)", report.Uploads)
	}
}

func TestRunOnce_UploadOnly_SkipsDelta(t *testing.T) {
	t.Parallel()

	deltaCalled := false
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			deltaCalled = true
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	_, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if deltaCalled {
		t.Error("Delta was called in upload-only mode; should have been skipped")
	}
}

func TestRunOnce_Bidirectional_FullCycle(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "remote-file-1", Name: "remote.txt", ParentID: "root",
					DriveID: driveID, Size: 42, QuickXorHash: "remotehash1",
				},
			}, "token-after"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("remote-content"))
			return int64(n), err
		},
		uploadFn: func(_ context.Context, _ driveid.ID, _ string, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{
				ID: "uploaded-id", Name: name, Size: 13, QuickXorHash: "localhash1",
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)

	// Create a local-only file.
	writeLocalFile(t, syncRoot, "local.txt", "local-content")

	ctx := context.Background()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Expect at least one download (remote.txt) and one upload (local.txt).
	if report.Downloads < 1 {
		t.Errorf("downloads = %d, want >= 1", report.Downloads)
	}

	if report.Uploads < 1 {
		t.Errorf("uploads = %d, want >= 1", report.Uploads)
	}

	if report.Failed != 0 {
		t.Errorf("failed = %d, want 0; errors: %v", report.Failed, report.Errors)
	}

	// Verify baseline was updated: reload and check entries exist.
	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		t.Fatalf("Load baseline: %v", err)
	}

	if _, ok := bl.ByPath["remote.txt"]; !ok {
		t.Error("remote.txt not in baseline after sync")
	}

	if _, ok := bl.ByPath["local.txt"]; !ok {
		t.Error("local.txt not in baseline after sync")
	}
}

func TestRunOnce_DryRun_NoExecution(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	executorCalled := false

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "newfile.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash1",
				},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, _ io.Writer) (int64, error) {
			executorCalled = true
			return 0, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{DryRun: true})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if !report.DryRun {
		t.Error("report.DryRun = false, want true")
	}

	if report.Downloads < 1 {
		t.Errorf("downloads = %d, want >= 1 (plan should be computed)", report.Downloads)
	}

	if executorCalled {
		t.Error("executor was called during dry-run")
	}

	if report.Succeeded != 0 || report.Failed != 0 {
		t.Errorf("succeeded=%d failed=%d, want both 0 for dry-run", report.Succeeded, report.Failed)
	}

	// Verify baseline is unchanged (no commit in dry-run).
	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		t.Fatalf("Load baseline: %v", err)
	}

	if len(bl.ByPath) != 0 {
		t.Errorf("baseline has %d entries, want 0 (dry-run should not commit)", len(bl.ByPath))
	}
}

func TestRunOnce_BigDelete_WithoutForce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Upload-only mode with no local files → local observer sees all baseline
	// entries as deleted → EF6 → ActionRemoteDelete. 20 remote deletes on a
	// 20-entry baseline = 100% > 50% threshold → ErrBigDeleteTriggered.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	seedOutcomes := make([]Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	_, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{})
	if !errors.Is(err, ErrBigDeleteTriggered) {
		t.Errorf("expected ErrBigDeleteTriggered, got: %v", err)
	}
}

func TestRunOnce_BigDelete_WithForce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Same scenario as WithoutForce: upload-only, no local files, 20 baseline
	// entries → 20 RemoteDeletes. Force bypasses the safety threshold.
	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	seedOutcomes := make([]Outcome, 20)
	for i := range 20 {
		seedOutcomes[i] = Outcome{
			Action:     ActionDownload,
			Success:    true,
			Path:       fmt.Sprintf("file%02d.txt", i),
			DriveID:    driveID,
			ItemID:     fmt.Sprintf("item-%02d", i),
			ItemType:   ItemTypeFile,
			RemoteHash: fmt.Sprintf("hash%02d", i),
			LocalHash:  fmt.Sprintf("hash%02d", i),
			Size:       100,
		}
	}

	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	report, err := eng.RunOnce(ctx, SyncUploadOnly, RunOpts{Force: true})
	if err != nil {
		t.Fatalf("RunOnce with force: %v", err)
	}

	if report.RemoteDeletes < 1 {
		t.Errorf("remote_deletes = %d, want >= 1 (force should bypass big-delete)", report.RemoteDeletes)
	}
}

func TestRunOnce_ExecutorPartialFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "good.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash1",
				},
				{
					ID: "f2", Name: "bad.txt", ParentID: "root",
					DriveID: driveID, Size: 10, QuickXorHash: "hash2",
				},
			}, "token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			if itemID == "f2" {
				return 0, context.Canceled
			}

			n, err := w.Write([]byte("good"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	// DAG executor handles individual failures gracefully — RunOnce succeeds
	// but reports the failure in Stats.
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// At least 1 succeeded and at least 1 failed.
	if report.Succeeded < 1 {
		t.Errorf("succeeded = %d, want >= 1", report.Succeeded)
	}

	if report.Failed < 1 {
		t.Errorf("failed = %d, want >= 1", report.Failed)
	}

	// Verify the successful file is in baseline.
	bl, loadErr := eng.baseline.Load(ctx)
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}

	if _, ok := bl.ByPath["good.txt"]; !ok {
		t.Error("good.txt not in baseline after partial commit")
	}
}

func TestRunOnce_ContextCancellation(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return nil, context.Canceled
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
}

func TestRunOnce_DeltaTokenPersisted(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "file.txt", ParentID: "root",
					DriveID: driveID, Size: 5, QuickXorHash: "hash1",
				},
			}, "new-delta-token"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("data"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Verify delta token was saved.
	token, err := eng.baseline.GetDeltaToken(ctx, engineTestDriveID)
	if err != nil {
		t.Fatalf("GetDeltaToken: %v", err)
	}

	if token != "new-delta-token" {
		t.Errorf("delta token = %q, want %q", token, "new-delta-token")
	}
}

func TestRunOnce_BaselineUpdatedAfterCycle(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "item-a", Name: "alpha.txt", ParentID: "root",
					DriveID: driveID, Size: 7, QuickXorHash: "alphahash",
				},
			}, "token-v2"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write([]byte("alpha!!"))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	_, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Reload and verify.
	bl, err := eng.baseline.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entry, ok := bl.ByPath["alpha.txt"]
	if !ok {
		t.Fatal("alpha.txt not in baseline")
	}

	if entry.ItemID != "item-a" {
		t.Errorf("ItemID = %q, want %q", entry.ItemID, "item-a")
	}

	if entry.DriveID != driveID {
		t.Errorf("DriveID = %v, want %v", entry.DriveID, driveID)
	}
}

func TestNewEngine_InvalidDBPath(t *testing.T) {
	t.Parallel()

	logger := testLogger(t)

	_, err := NewEngine(&EngineConfig{
		DBPath:    "/nonexistent/deeply/nested/path/test.db",
		SyncRoot:  t.TempDir(),
		DriveID:   driveid.New(engineTestDriveID),
		Fetcher:   &engineMockClient{},
		Items:     &engineMockClient{},
		Downloads: &engineMockClient{},
		Uploads:   &engineMockClient{},
		Logger:    logger,
	})

	if err == nil {
		t.Fatal("expected error for invalid DB path, got nil")
	}
}

func TestRunOnce_DeltaExpired_AutoRetry(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	callCount := 0

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			callCount++
			// First call (with saved token) returns expired.
			if callCount == 1 {
				return nil, graph.ErrGone
			}

			// Second call (empty token) succeeds.
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
			}, "fresh-token"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a stale delta token.
	seedOutcomes := []Outcome{{
		Action:  ActionDownload,
		Success: true,
		Path:    "seed.txt",
		DriveID: driveID,
		ItemID:  "seed-1",
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "stale-token")

	report, err := eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Delta should have been called twice (expired + retry).
	if callCount != 2 {
		t.Errorf("delta call count = %d, want 2", callCount)
	}

	// Report should reflect no content changes (only root in delta).
	total := report.Downloads + report.Uploads
	if total != 0 {
		t.Errorf("downloads+uploads = %d, want 0", total)
	}
}

// TestRunOnce_EmptyPlan_NoPanic verifies that when changes exist but all
// classify to no-op actions (producing an empty plan), the engine does not
// deadlock. Regression test for: empty plan caused NewDepTracker with total=0,
// Done() channel never closed, pool.Wait() blocked forever.
func TestRunOnce_EmptyPlan_NoPanic(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	// Seed a baseline entry that matches the delta response exactly.
	// The planner will see no diff → all changes classify to EF1/ED1 (no-op)
	// → empty action plan.
	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID: "f1", Name: "unchanged.txt", ParentID: "root",
					DriveID: driveID, Size: 5, QuickXorHash: "matchhash",
				},
			}, "token-empty"), nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed baseline so the file appears as already synced with matching hash.
	seedOutcomes := []Outcome{{
		Action:     ActionDownload,
		Success:    true,
		Path:       "unchanged.txt",
		DriveID:    driveID,
		ItemID:     "f1",
		ItemType:   ItemTypeFile,
		RemoteHash: "matchhash",
		LocalHash:  "matchhash",
		Size:       5,
	}}
	seedBaseline(t, eng.baseline, ctx, seedOutcomes, "old-token")

	// Write a matching local file so the local observer also sees no change.
	writeLocalFile(t, syncRoot, "unchanged.txt", "hello")

	// This should complete without deadlock — use a timeout to detect hangs.
	done := make(chan struct{})
	var report *SyncReport
	var runErr error

	go func() {
		report, runErr = eng.RunOnce(ctx, SyncBidirectional, RunOpts{})
		close(done)
	}()

	select {
	case <-done:
		// Good — completed.
	case <-time.After(10 * time.Second):
		t.Fatal("RunOnce deadlocked on empty action plan")
	}

	if runErr != nil {
		t.Fatalf("RunOnce: %v", runErr)
	}

	if report.Failed != 0 {
		t.Errorf("failed = %d, want 0", report.Failed)
	}
}

func TestResolveSafetyConfig_Default(t *testing.T) {
	t.Parallel()

	eng := &Engine{}
	cfg := eng.resolveSafetyConfig(RunOpts{})

	def := DefaultSafetyConfig()
	if cfg.BigDeleteMaxCount != def.BigDeleteMaxCount {
		t.Errorf("BigDeleteMaxCount = %d, want %d", cfg.BigDeleteMaxCount, def.BigDeleteMaxCount)
	}

	if cfg.BigDeleteMaxPercent != def.BigDeleteMaxPercent {
		t.Errorf("BigDeleteMaxPercent = %f, want %f", cfg.BigDeleteMaxPercent, def.BigDeleteMaxPercent)
	}
}

func TestResolveSafetyConfig_Force(t *testing.T) {
	t.Parallel()

	eng := &Engine{}
	cfg := eng.resolveSafetyConfig(RunOpts{Force: true})

	if cfg.BigDeleteMaxCount != forceSafetyMax {
		t.Errorf("BigDeleteMaxCount = %d, want %d", cfg.BigDeleteMaxCount, forceSafetyMax)
	}
}

// ---------------------------------------------------------------------------
// Conflict resolution tests
// ---------------------------------------------------------------------------

func TestResolveConflict_KeepBoth(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "conflict-file.txt",
		DriveID:      driveID,
		ItemID:       "item-c",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Get conflict ID.
	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	// Resolve as keep_both.
	if resolveErr := eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepBoth); resolveErr != nil {
		t.Fatalf("ResolveConflict: %v", resolveErr)
	}

	// Verify it's no longer unresolved.
	remaining, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts after resolve: %v", err)
	}

	if len(remaining) != 0 {
		t.Errorf("expected 0 unresolved conflicts, got %d", len(remaining))
	}
}

func TestResolveConflict_NotFound(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	err := eng.ResolveConflict(ctx, "nonexistent-id", ResolutionKeepBoth)
	if err == nil {
		t.Fatal("expected error for nonexistent conflict, got nil")
	}
}

func TestResolveConflict_UnknownStrategy(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "bad-strategy.txt",
		DriveID:      driveID,
		ItemID:       "item-x",
		ItemType:     ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	err = eng.ResolveConflict(ctx, conflicts[0].ID, "invalid_strategy")
	if err == nil {
		t.Fatal("expected error for unknown strategy, got nil")
	}
}

func TestListConflicts_Engine(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(nil, "token-1"), nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	ctx := context.Background()

	// Empty initially.
	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 0 {
		t.Errorf("expected 0 conflicts, got %d", len(conflicts))
	}
}

func TestResolveConflict_KeepLocal(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	uploadCalled := false

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			uploadCalled = true

			return &graph.Item{
				ID:           "uploaded-resolve",
				Name:         name,
				ETag:         "etag-resolved",
				QuickXorHash: "resolve-hash",
				Size:         5,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "keep-local.txt",
		DriveID:      driveID,
		ItemID:       "item-kl",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that will be uploaded.
	writeLocalFile(t, syncRoot, "keep-local.txt", "local")

	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	if resolveErr := eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal); resolveErr != nil {
		t.Fatalf("ResolveConflict: %v", resolveErr)
	}

	if !uploadCalled {
		t.Error("expected Upload to be called for keep_local resolution")
	}

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts after resolve: %v", err)
	}

	if len(remaining) != 0 {
		t.Errorf("expected 0 unresolved conflicts, got %d", len(remaining))
	}
}

func TestResolveConflict_KeepRemote(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	downloadContent := "remote-version"

	mock := &engineMockClient{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, writeErr := w.Write([]byte(downloadContent))
			return int64(n), writeErr
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "keep-remote.txt",
		DriveID:      driveID,
		ItemID:       "item-kr",
		ItemType:     ItemTypeFile,
		LocalHash:    "local-h",
		RemoteHash:   "remote-h",
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	if resolveErr := eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepRemote); resolveErr != nil {
		t.Fatalf("ResolveConflict: %v", resolveErr)
	}

	// Conflict should be resolved.
	remaining, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts after resolve: %v", err)
	}

	if len(remaining) != 0 {
		t.Errorf("expected 0 unresolved conflicts, got %d", len(remaining))
	}

	// Verify the local file has remote content.
	data, readErr := os.ReadFile(filepath.Join(syncRoot, "keep-remote.txt"))
	if readErr != nil {
		t.Fatalf("reading resolved file: %v", readErr)
	}

	if string(data) != downloadContent {
		t.Errorf("expected %q, got %q", downloadContent, string(data))
	}
}

func TestResolveConflict_KeepLocal_TransferFails(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)

	mock := &engineMockClient{
		uploadFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return nil, fmt.Errorf("upload failed: network error")
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	ctx := context.Background()

	// Seed a conflict.
	outcomes := []Outcome{{
		Action:       ActionConflict,
		Success:      true,
		Path:         "fail-upload.txt",
		DriveID:      driveID,
		ItemID:       "item-fu",
		ItemType:     ItemTypeFile,
		ConflictType: "edit_edit",
	}}

	seedBaseline(t, eng.baseline, ctx, outcomes, "")

	// Write the local file that would be uploaded.
	writeLocalFile(t, syncRoot, "fail-upload.txt", "local-data")

	conflicts, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts: %v", err)
	}

	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(conflicts))
	}

	resolveErr := eng.ResolveConflict(ctx, conflicts[0].ID, ResolutionKeepLocal)
	if resolveErr == nil {
		t.Fatal("expected error from failed upload, got nil")
	}

	// Conflict should remain unresolved.
	remaining, err := eng.ListConflicts(ctx)
	if err != nil {
		t.Fatalf("ListConflicts after failed resolve: %v", err)
	}

	if len(remaining) != 1 {
		t.Errorf("expected 1 unresolved conflict, got %d", len(remaining))
	}
}
