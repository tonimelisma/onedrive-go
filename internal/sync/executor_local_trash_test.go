package sync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

func localDeleteActionFor(t *testing.T, path, content string) *Action {
	t.Helper()

	hash := hashContentQuickXor(t, content)
	return &Action{
		Type:    ActionLocalDelete,
		Path:    path,
		ItemID:  "item-" + path,
		DriveID: driveid.New(synctest.TestDriveID),
		View: &pathView{
			Path: path,
			Baseline: &BaselineEntry{
				Path: path, ItemID: "item-" + path, ItemType: ItemTypeFile,
				LocalHash: hash, RemoteHash: hash,
				LocalSize: int64(len(content)), LocalSizeKnown: true,
			},
		},
	}
}

// Validates: R-6.4.9
func TestDeleteLocalFile_MovesTheFileToTheTrash(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
	cfg.SetTrash(trash)
	writeExecTestFile(t, syncRoot, "docs/a.txt", "synced\n")

	outcome := newExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "docs/a.txt", "synced\n"))

	requireOutcomeSuccess(t, &outcome)
	assert.NoFileExists(t, filepath.Join(syncRoot, "docs", "a.txt"))
	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "docs/a.txt", entries[0].Path)
	assert.Equal(t, localtrash.ReasonDeleted, entries[0].Reason)
	assert.Equal(t, int64(len("synced\n")), entries[0].Size)
}

// The user saves a new version (an editor's write-then-rename) while sync is
// hashing the old one for a delete. The old shape hashed by path and then
// removed by path, unlinking the new content (D-010).
//
// Validates: R-6.2.4
func TestDeleteLocalFile_FileReplacedMidHashSurvives(t *testing.T) {
	t.Parallel()

	for _, withTrash := range []bool{true, false} {
		t.Run(map[bool]string{true: "trash", false: "no trash"}[withTrash], func(t *testing.T) {
			t.Parallel()

			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
			var trash *localtrash.Trash
			if withTrash {
				trash = localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
				cfg.SetTrash(trash)
			}
			target := writeExecTestFile(t, syncRoot, "a.txt", "synced\n")
			cfg.hashReader = func(r io.Reader) (string, error) {
				hash, err := driveops.ComputeQuickXorHashFromReader(r)
				replacement := writeExecTestFile(t, syncRoot, "a.txt.save", "new edit\n")
				require.NoError(t, os.Rename(replacement, target))
				if err != nil {
					return "", fmt.Errorf("hashing in test: %w", err)
				}
				return hash, nil
			}

			outcome := newExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "a.txt", "synced\n"))

			requireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
			data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
			require.NoError(t, err)
			assert.Equal(t, "new edit\n", string(data), "the new content must survive")
			if trash != nil {
				entries, listErr := trash.List()
				require.NoError(t, listErr)
				assert.Empty(t, entries)
			}
		})
	}
}

// A run that died after setting a file aside leaves it under a hidden name in
// the sync folder. It is user content: every full scan reports it, nothing
// uploads it, and housekeeping never deletes it.
//
// Validates: R-6.4.9
func TestFullScan_LeftoverSetAsideFileIsReportedNotDeleted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	leftover := filepath.Join(syncRoot, ".onedrive-go.0123456789abcdef.detach")
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	runScenarioPass(t, eng)

	assert.FileExists(t, leftover, "an interrupted removal is never cleaned up automatically")
	assert.NotContains(t, remote.tree(), ".onedrive-go.0123456789abcdef.detach", "it is never uploaded")
	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	found := false
	for i := range issues {
		if issues[i].Path == ".onedrive-go.0123456789abcdef.detach" && issues[i].IssueType == issueLocalInterruptedRemoval {
			found = true
		}
	}
	assert.True(t, found, "the leftover is reported as a durable issue naming its path: %+v", issues)
}
