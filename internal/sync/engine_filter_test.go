package sync

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// Validates: R-2.4.1, R-2.4.2, R-2.4.3
func TestRunOnce_LocalFilterConfigSuppressesConfiguredUploads(t *testing.T) {
	t.Parallel()

	var uploaded []string
	mock := &engineMockClient{
		uploadFn: func(
			_ context.Context,
			_ driveid.ID,
			_ string,
			name string,
			_ io.ReaderAt,
			_ int64,
			_ time.Time,
			_ graph.ProgressFunc,
		) (*graph.Item, error) {
			uploaded = append(uploaded, name)
			return &graph.Item{
				ID:           "uploaded-" + name,
				Name:         name,
				QuickXorHash: "hash-" + name,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	eng.localFilter = synctypes.LocalFilterConfig{
		SkipDotfiles: true,
		SkipDirs:     []string{"vendor"},
		SkipFiles:    []string{"*.log"},
	}

	writeLocalFile(t, syncRoot, ".env", "secret")
	writeLocalFile(t, syncRoot, "vendor/lib.txt", "vendored")
	writeLocalFile(t, syncRoot, "debug.log", "noise")
	writeLocalFile(t, syncRoot, "keep.txt", "keep")

	report, err := eng.RunOnce(t.Context(), synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)

	assert.Equal(t, []string{"keep.txt"}, uploaded)
	assert.Equal(t, 1, report.Uploads)

	issues, issueErr := eng.baseline.ListSyncFailures(t.Context())
	require.NoError(t, issueErr)
	assert.Empty(t, issues, "configured exclusions should not record actionable failures")
}

// Validates: R-2.4.4, R-2.4.5
func TestRunOnce_SyncScopeSuppressesConfiguredUploads(t *testing.T) {
	t.Parallel()

	var uploaded []string
	mock := &engineMockClient{
		uploadFn: func(
			_ context.Context,
			_ driveid.ID,
			_ string,
			name string,
			_ io.ReaderAt,
			_ int64,
			_ time.Time,
			_ graph.ProgressFunc,
		) (*graph.Item, error) {
			uploaded = append(uploaded, name)
			return &graph.Item{
				ID:           "uploaded-" + name,
				Name:         name,
				QuickXorHash: "hash-" + name,
			}, nil
		},
	}

	eng, syncRoot := newTestEngine(t, mock)
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths:    []string{"/docs/keep.txt"},
		IgnoreMarker: ".odignore",
	}

	writeLocalFile(t, syncRoot, "docs/keep.txt", "keep")
	writeLocalFile(t, syncRoot, "docs/drop.txt", "drop")
	writeLocalFile(t, syncRoot, "blocked/.odignore", "")
	writeLocalFile(t, syncRoot, "blocked/secret.txt", "blocked")

	report, err := eng.RunOnce(t.Context(), synctypes.SyncUploadOnly, synctypes.RunOpts{})
	require.NoError(t, err)

	assert.Equal(t, []string{"keep.txt"}, uploaded)
	assert.Equal(t, 1, report.Uploads)

	issues, issueErr := eng.baseline.ListSyncFailures(t.Context())
	require.NoError(t, issueErr)
	assert.Empty(t, issues, "scope exclusions should remain silent and not surface as failures")
}

// Validates: R-2.4.5
func TestRunOnce_DownloadScopeSuppressesOutOfScopeRemoteItems(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	var downloaded []string
	contents := map[string]string{
		"keep-item": "keep-data",
		"drop-item": "drop-data",
	}
	mock := newTwoFileDownloadDeltaMock(t, driveID, contents, &downloaded, "delta-token-1")

	eng, _ := newTestEngine(t, mock)
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/keep.txt"},
	}

	report, err := eng.RunOnce(t.Context(), synctypes.SyncDownloadOnly, synctypes.RunOpts{})
	require.NoError(t, err)

	assert.Equal(t, []string{"keep-item"}, downloaded)
	assert.Equal(t, 1, report.Downloads)

	keepRow, keepFound, keepErr := eng.baseline.GetRemoteStateByPath(t.Context(), "keep.txt", driveID)
	require.NoError(t, keepErr)
	require.True(t, keepFound)
	assert.Equal(t, synctypes.SyncStatusSynced, keepRow.SyncStatus)

	dropRow, dropFound, dropErr := eng.baseline.GetRemoteStateByPath(t.Context(), "drop.txt", driveID)
	require.NoError(t, dropErr)
	require.True(t, dropFound)
	assert.Equal(t, synctypes.SyncStatusFiltered, dropRow.SyncStatus)
}
