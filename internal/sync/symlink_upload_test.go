package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// newFollowSymlinksTestEngine is newTestEngine with follow_symlinks enabled,
// which is the only configuration where content beneath an alias is observed.
func newFollowSymlinksTestEngine(t *testing.T, mock *syncfixture.EngineMockClient) (*testEngine, string) {
	t.Helper()

	ctx := t.Context()
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	syncRoot := filepath.Join(tmpDir, "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	require.NoError(t, os.MkdirAll(dataDir, 0o750))

	accountCID := testEngineAccountCanonicalID()
	eng, err := newEngine(ctx, &engineInputs{
		DBPath:             filepath.Join(tmpDir, "test.db"),
		SyncRoot:           syncRoot,
		DataDir:            dataDir,
		DriveID:            driveid.New(syncfixture.EngineTestDriveID),
		AccountEmail:       engineTestEmail,
		AccountCanonicalID: accountCID,
		Fetcher:            mock,
		SocketIOFetcher:    mock,
		Items:              mock,
		Downloads:          mock,
		Uploads:            mock,
		PathConvergence:    &enginePathConvergenceStub{},
		FolderDelta:        mock,
		RecursiveLister:    mock,
		PermChecker:        mock,
		Logger:             syncfixture.TestLogger(t),
		ContentFilter:      syncdomain.ContentFilterConfig{FollowSymlinks: true},
	})
	require.NoError(t, err)
	eng.hooks.assertInvariants = true
	testEng := &testEngine{Engine: eng, flow: newEngineFlow(eng)}
	seedTestEngineAccount(t, dataDir, accountCID)
	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(context.WithoutCancel(ctx)))
	})

	return testEng, syncRoot
}

// Validates: R-2.4.6
//
// The whole pass, not just the executor: with follow_symlinks on, a folder
// symlinked to a directory outside the sync root is created remotely and the
// file inside it uploads with the target's content. Before the fix the upload
// failed its source precondition because the rooted sync tree refused to stat
// a name that resolves outside the root.
func TestRunOnce_FollowSymlinks_UploadsFileInsideDirectoryAliasOutsideRoot(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.EngineTestDriveID)
	var (
		mu       gosync.Mutex
		uploaded = map[string]string{}
	)
	mock := &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{{ID: "root", IsRoot: true, DriveID: driveID}}, "token-1"), nil
		},
		CreateFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			assert.Equal(t, "root", parentID)
			assert.Equal(t, "alias", name)
			return &graph.Item{ID: "alias-folder-id", Name: name, IsFolder: true, DriveID: driveID}, nil
		},
		UploadFn: func(_ context.Context, _ driveid.ID, parentID, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			buf := make([]byte, size)
			if _, err := content.ReadAt(buf, 0); err != nil && size > 0 {
				return nil, fmt.Errorf("reading upload content: %w", err)
			}
			mu.Lock()
			uploaded[parentID+"/"+name] = string(buf)
			mu.Unlock()
			return &graph.Item{ID: "uploaded-" + name, Name: name, Size: size, DriveID: driveID}, nil
		},
	}

	eng, syncRoot := newFollowSymlinksTestEngine(t, mock)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "photo.txt"), []byte("PRECIOUS"), 0o600))
	if err := os.Symlink(outside, filepath.Join(syncRoot, "alias")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, report.Failed, "failed; errors: %v", report.Errors)
	assert.Equal(t, 1, report.Uploads)

	mu.Lock()
	assert.Equal(t, map[string]string{"alias-folder-id/photo.txt": "PRECIOUS"}, uploaded)
	mu.Unlock()

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath("alias/photo.txt")
	require.True(t, ok, "the uploaded file must be in the baseline at its alias path")
	assert.Equal(t, "uploaded-photo.txt", entry.ItemID)

	//nolint:gosec // test reads the file it created under t.TempDir.
	got, err := os.ReadFile(filepath.Join(outside, "photo.txt"))
	require.NoError(t, err)
	assert.Equal(t, "PRECIOUS", string(got), "uploading must not modify the target")
}
