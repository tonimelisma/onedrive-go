package sync

import (
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
				return hash, err
			}

			outcome := newExecution(cfg, emptyBaseline()).ExecuteLocalDelete(t.Context(), localDeleteActionFor(t, "a.txt", "synced\n"))

			requireOutcomeFailure(t, &outcome)
			require.ErrorIs(t, outcome.Error, errActionPreconditionChanged)
			data, err := os.ReadFile(target)
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
