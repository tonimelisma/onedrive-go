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

// A download may replace a local file only when that file is still the
// synced version. The plan's local hash is checked when it has one; when it
// has none, the baseline's local hash stands in, so an edit that kept size and
// mtime (a `cp -p` or `touch -r` edit) is never overwritten (D-066).
//
// Validates: R-2.2.5
func TestExecutor_Download_LocalContentDiffersFromBaselineIsSuperseded(t *testing.T) {
	t.Parallel()

	const (
		synced = "synced version"
		edited = "EDITED version"
		remote = "remote version"
	)
	for _, tt := range []struct {
		name        string
		withTrash   bool
		plannedHash bool
	}{
		{name: "planned hash, no trash", plannedHash: true},
		{name: "planned hash, trash", plannedHash: true, withTrash: true},
		{name: "no planned hash, no trash"},
		{name: "no planned hash, trash", withTrash: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dl := &executorMockDownloader{
				downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
					n, err := w.Write([]byte(remote))
					return int64(n), err
				},
			}
			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, dl, &executorMockUploader{})
			if tt.withTrash {
				cfg.SetTrash(localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil))
			}
			target := writeExecTestFile(t, syncRoot, "doc.txt", edited)
			info, err := os.Stat(target)
			require.NoError(t, err)

			syncedHash := testQuickXor(synced)
			planned := &localState{ItemType: ItemTypeFile, Size: info.Size(), Mtime: info.ModTime().UnixNano()}
			if tt.plannedHash {
				// The scan reused the stale baseline hash for the edited file.
				planned.Hash = syncedHash
			}
			action := &Action{
				Type:    ActionDownload,
				Path:    "doc.txt",
				ItemID:  "item-doc",
				DriveID: driveid.New(synctest.TestDriveID),
				View: &pathView{
					Local: planned,
					Remote: &remoteState{
						ItemID: "item-doc", ItemType: ItemTypeFile, Hash: testQuickXor(remote),
						Size: int64(len(remote)), Mtime: 1,
					},
					Baseline: &BaselineEntry{
						Path: "doc.txt", ItemID: "item-doc", ItemType: ItemTypeFile,
						LocalHash: syncedHash, LocalSize: info.Size(), LocalSizeKnown: true,
						LocalMtime: info.ModTime().UnixNano(),
					},
				},
			}

			o := newExecution(cfg, emptyBaseline()).ExecuteDownload(t.Context(), action)

			requireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, errActionPreconditionChanged)
			data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
			require.NoError(t, err)
			assert.Equal(t, edited, string(data), "the unsynced local edit survives")
		})
	}
}

// Validates: R-2.17.9
//
// A replacing local move checks its source before it touches the destination.
// A source that is no longer the synced item supersedes the move, and the
// verified file at the destination stays where it is instead of having been
// moved to the trash first for a move that then never ran.
func TestExecutor_LocalMove_StaleSourceLeavesTheReplacedDestinationInPlace(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
	cfg.SetTrash(trash)
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "a.txt", "alpha\n")
	writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
	plannedHash, err := e.hashFunc(filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	source, err := e.syncTree.IdentityNoFollow("a.txt")
	require.NoError(t, err)

	action := &Action{
		Type:               ActionLocalMove,
		Path:               "b.txt",
		OldPath:            "a.txt",
		ReplaceLocalTarget: true,
		View: &pathView{
			Path:  "b.txt",
			Local: &localState{ItemType: ItemTypeFile, Hash: plannedHash, Size: info.Size()},
			// The synced item had another inode: a.txt is not it any more.
			Baseline: &BaselineEntry{
				Path: "a.txt", ItemType: ItemTypeFile,
				LocalDevice: source.Device, LocalInode: source.Inode + 1, LocalHasIdentity: true,
			},
		},
	}

	o := e.ExecuteMove(t.Context(), action)
	requireOutcomeFailure(t, &o)
	require.ErrorIs(t, o.Error, errActionPreconditionChanged)
	data, err := os.ReadFile(filepath.Join(syncRoot, "b.txt")) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "bravo\n", string(data), "the destination is untouched")
	entries, err := trash.List()
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing went to the trash for a move that did not run")
}
