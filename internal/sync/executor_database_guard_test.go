package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func databaseDownloadAction() *Action {
	return &Action{
		Type:    ActionDownload,
		Path:    "app.db",
		ItemID:  "item-db",
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Remote: &remoteState{ItemID: "item-db", ETag: "etag-db"},
			Local:  &localState{Name: "app.db", ItemType: ItemTypeFile, Size: 3},
		},
	}
}

// Validates: R-2.4.15
func TestExecutor_Download_RefusesToReplaceDatabaseWithLiveWAL(t *testing.T) {
	t.Parallel()

	for _, sidecar := range []string{"-wal", "-journal"} {
		t.Run(sidecar, func(t *testing.T) {
			t.Parallel()

			downloads := 0
			dl := &executorMockDownloader{
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					downloads++
					n, err := w.Write([]byte("new"))
					return int64(n), err
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"), []byte("old"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"+sidecar), []byte("pages"), 0o600))

			o := newExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), databaseDownloadAction())

			require.False(t, o.Success)
			require.ErrorIs(t, o.Error, errDatabaseInUse)
			assert.Zero(t, downloads, "nothing is fetched for an open database")
			data, err := localpath.ReadFile(filepath.Join(syncRoot, "app.db"))
			require.NoError(t, err)
			assert.Equal(t, "old", string(data))
			assert.Equal(t, issueLocalDatabaseOpen, classifyResult(&actionCompletion{Err: o.Error}).ConditionType)
		})
	}
}

// Validates: R-2.4.15
func TestExecutor_Download_DatabaseOpenedDuringDownloadIsNotReplaced(t *testing.T) {
	t.Parallel()

	var syncRoot string
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			// The application opens the database while the new copy streams.
			if err := os.WriteFile(filepath.Join(syncRoot, "app.db-wal"), []byte("pages"), 0o600); err != nil {
				return 0, fmt.Errorf("open the database: %w", err)
			}
			n, err := w.Write([]byte("new"))
			return int64(n), err
		},
	}
	cfg, root := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	syncRoot = root
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db"), []byte("old"), 0o600))
	// A -shm index by itself does not block (it is not uncommitted data);
	// only the -wal that appears mid-download does.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db-shm"), []byte("idx"), 0o600))

	o := newExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), databaseDownloadAction())

	require.False(t, o.Success)
	require.ErrorIs(t, o.Error, errDatabaseInUse, "the check before the rename catches it")
	data, err := localpath.ReadFile(filepath.Join(syncRoot, "app.db"))
	require.NoError(t, err)
	assert.Equal(t, "old", string(data))
}

// Validates: R-2.4.15
func TestExecutor_Download_RefusesNewDatabaseBesideALiveWAL(t *testing.T) {
	t.Parallel()

	downloads := 0
	dl := &executorMockDownloader{
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			downloads++
			n, err := w.Write([]byte("new"))
			return int64(n), err
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
	// No app.db yet, but the application has a live WAL for it.
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "app.db-wal"), []byte("pages"), 0o600))
	action := databaseDownloadAction()
	action.View.Local = nil

	o := newExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), action)

	require.False(t, o.Success)
	require.ErrorIs(t, o.Error, errDatabaseInUse)
	assert.Zero(t, downloads)
	assert.NoFileExists(t, filepath.Join(syncRoot, "app.db"))
}
