package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.12.5
func TestExecutor_Upload_NFDRemoteSpellingIsNotAMove(t *testing.T) {
	t.Parallel()

	// OneDrive keeps an NFD name as uploaded (L-04); the action path is NFC.
	// The preflight read the raw NFD path as "moved to café.txt" and
	// superseded the overwrite on every pass.
	ul := &executorMockUploader{
		uploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
			return &graph.Item{ID: "nfd-item"}, nil
		},
	}
	items := &executorMockItemClient{
		getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
			item := defaultMockGetItem(driveID, itemID)
			item.Name = "café.txt"
			item.ParentPath = ""
			item.ParentPathKnown = true
			return item, nil
		},
	}
	cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "café.txt", "content")

	o := e.ExecuteUpload(t.Context(), &Action{
		Type: ActionUpload, Path: "café.txt", ItemID: "nfd-item",
		DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "café.txt"},
	})
	requireOutcomeSuccess(t, &o)
}

// Validates: R-6.7.3, R-6.7.23
func TestExecutor_Upload_PreflightLocatesTheItemByParentIDAndName(t *testing.T) {
	t.Parallel()

	// The parent folder is literally named "a%20b" (L-32: parentReference.path
	// is not encoded). The old preflight decoded the live path to "a b/x.txt"
	// and superseded the overwrite as "moved".
	baseline := baselineWith(&BaselineEntry{
		Path: "a%20b", ItemID: "folder-a", DriveID: driveid.New(synctest.TestDriveID), ItemType: ItemTypeFolder,
	})
	live := func(parentID string) *executorMockItemClient {
		return &executorMockItemClient{
			getItemFn: func(_ context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
				item := defaultMockGetItem(driveID, itemID)
				item.Name = "x.txt"
				item.ParentID = parentID
				item.ParentPath = "a%20b"
				item.ParentPathKnown = true
				return item, nil
			},
		}
	}
	upload := func(t *testing.T, items *executorMockItemClient) actionOutcome {
		t.Helper()
		ul := &executorMockUploader{
			uploadToItemFn: func(_ context.Context, _ driveid.ID, _, _ string, _ io.ReaderAt, _ int64, _ time.Time, _ graph.ProgressFunc) (*graph.Item, error) {
				return &graph.Item{ID: "x-item"}, nil
			},
		}
		cfg, syncRoot := newTestExecutorConfig(t, items, &executorMockDownloader{}, ul)
		e := newExecution(cfg, baseline)
		require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "a%20b"), 0o700))
		writeExecTestFile(t, syncRoot, "a%20b/x.txt", "content")

		return e.ExecuteUpload(t.Context(), &Action{
			Type: ActionUpload, Path: "a%20b/x.txt", ItemID: "x-item",
			DriveID: driveid.New(synctest.TestDriveID), View: &pathView{Path: "a%20b/x.txt"},
		})
	}

	o := upload(t, live("folder-a"))
	requireOutcomeSuccess(t, &o)

	// Same name and same verbatim path, but under another folder ID: moved.
	o = upload(t, live("another-folder"))
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
}
