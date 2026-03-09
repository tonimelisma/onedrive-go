package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

// ---------------------------------------------------------------------------
// B-318: Fault injection test suite
// ---------------------------------------------------------------------------

// TestFault_ContextCancel_WorkerPool verifies that canceling a context during
// active worker pool execution results in graceful shutdown without panics
// or orphaned goroutines.
func TestFault_ContextCancel_WorkerPool(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t,
		&executorMockItemClient{},
		&executorMockDownloader{},
		&executorMockUploader{},
	)
	cfg.sleepFunc = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}

	// Create a local file so the upload has something to read.
	writeExecTestFile(t, syncRoot, "file.txt", "content")

	tracker := NewDepTracker(1, testLogger(t))
	mgr := newTestManager(t)
	pool := NewWorkerPool(cfg, tracker, mgr, testLogger(t), 10)

	ctx, cancel := context.WithCancel(t.Context())

	pool.Start(ctx, 2)

	action := &Action{
		Type:   ActionUpload,
		Path:   "file.txt",
		ItemID: "item-1",
		View:   &PathView{Remote: &RemoteState{ItemID: "parent", ParentID: "root"}},
	}
	tracker.Add(action, 0, nil)

	// Cancel immediately — should not panic or hang.
	// pool.Wait() is not called here because it blocks on tracker.Done(),
	// which never closes when the worker exits via ctx.Done() before
	// picking up the action. pool.Stop() calls wp.wg.Wait() internally,
	// which is sufficient for clean shutdown.
	cancel()
	pool.Stop()
}

// TestFault_BaselineCommitError verifies that an error during baseline commit
// (simulated by closing the DB) returns an error without crashing.
func TestFault_BaselineCommitError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	mgr, err := NewSyncStore(dbPath, testLogger(t))
	require.NoError(t, err)

	ctx := t.Context()

	// Seed a baseline entry.
	require.NoError(t, mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true, Path: "file.txt",
		DriveID: driveid.New(engineTestDriveID), ItemID: "item-1",
		ParentID: "root", ItemType: ItemTypeFile, RemoteHash: "hash1",
		Size: 100,
	}))

	// Close the DB to simulate a fault.
	require.NoError(t, mgr.Close())

	// CommitOutcome should return an error, not panic.
	err = mgr.CommitOutcome(ctx, &Outcome{
		Action: ActionDownload, Success: true, Path: "file2.txt",
		DriveID: driveid.New(engineTestDriveID), ItemID: "item-2",
		ParentID: "root", ItemType: ItemTypeFile,
	})
	assert.Error(t, err)
}

// TestFault_PartialFileCleanup verifies that .partial files are cleaned up
// even if the download is interrupted (context cancel).
func TestFault_PartialFileCleanup(t *testing.T) {
	t.Parallel()

	syncRoot := t.TempDir()

	// Create a .partial file simulating an interrupted download.
	partialPath := filepath.Join(syncRoot, "doc.txt.partial")
	require.NoError(t, os.WriteFile(partialPath, []byte("partial data"), 0o644))

	// CleanTransferArtifacts should remove it.
	driveops.CleanTransferArtifacts(syncRoot, nil, testLogger(t))

	_, err := os.Stat(partialPath)
	assert.True(t, os.IsNotExist(err), ".partial file should be cleaned up")
}
