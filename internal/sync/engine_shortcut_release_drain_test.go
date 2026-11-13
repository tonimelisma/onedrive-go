package sync

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// leftOutMessage is the Debug line a plan that releases its local tree logs
// when it leaves new items out.
const leftOutMessage = "plan of a tree being released leaves out items new to it"

// countedRelease is a synced scenario whose downloads are counted and whose
// log records are kept.
type countedRelease struct {
	remote    *moveScenarioRemote
	eng       *testEngine
	syncRoot  string
	downloads *releaseDownloadCounts
	capture   *synctest.LogCapture
}

// newCountedRelease syncs remote into a new engine that counts its downloads
// and keeps its log records.
func newCountedRelease(t *testing.T, remote *moveScenarioRemote) *countedRelease {
	t.Helper()

	client := remote.client(t)
	downloads := countingDownloads(client)
	capture, logger := synctest.NewLogCapture()
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), client, t.TempDir(), func(inputs *engineInputs) {
		inputs.Logger = logger
	})
	runScenarioPass(t, eng)

	return &countedRelease{remote: remote, eng: eng, syncRoot: syncRoot, downloads: downloads, capture: capture}
}

// passes is how many sync passes the engine has started.
func (c *countedRelease) passes() int {
	n := 0
	for _, record := range c.capture.Records() {
		if record.Message == "sync pass starting" {
			n++
		}
	}

	return n
}

// leftOut is the Debug lines saying how many new items a plan left out.
func (c *countedRelease) leftOut() []synctest.CapturedRecord {
	var out []synctest.CapturedRecord
	for _, record := range c.capture.Records() {
		if record.Message == leftOutMessage {
			out = append(out, record)
		}
	}

	return out
}

// Validates: R-2.4.8
//
// A final drain creates nothing new locally: a file or a folder OneDrive has
// and the drain never synced is not downloaded and gets no local entry. That
// is neither follow-up work nor work left, so the drain makes one pass, holds
// nothing, and the release takes the tree. The plan says how many items it
// left out once, at Debug.
func TestRunOnce_FinalDrainCreatesNothingNewLocally(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		add     func(t *testing.T, remote *moveScenarioRemote)
		leftOut string
	}{
		{
			name: "a file",
			add: func(t *testing.T, remote *moveScenarioRemote) {
				t.Helper()
				remote.addFile(t, "item-new", "item-docs", "new.txt", "new\n")
			},
			leftOut: "1",
		},
		{
			name: "a folder with a file in it",
			add: func(t *testing.T, remote *moveScenarioRemote) {
				t.Helper()
				remote.addFolder("item-new", "item-docs", "New")
				remote.addFile(t, "item-new-file", "item-new", "n.txt", "n\n")
			},
			leftOut: "2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			drain := newCountedRelease(t, newReleaseRemote(t))
			tc.add(t, drain.remote)
			synced, passes, before := drain.downloads.snapshot(), drain.passes(), drain.remote.tree()

			report := runReleaseDrain(t, drain.eng)

			assert.Zero(t, report.Downloads+report.FolderCreates, "nothing is planned for it")
			assert.Equal(t, synced, drain.downloads.snapshot(), "nothing is downloaded")
			assert.Equal(t, 1, drain.passes()-passes, "it is no follow-up work")
			assert.False(t, report.LocalTreeHolds().Any(), "nor work left: %+v", report.LocalTreeHolds())
			assert.Equal(t, releaseSyncedRows, report.Released, "the release runs")
			assert.Empty(t, releaseRootEntries(t, drain.syncRoot), "no entry is made for it")
			assert.Equal(t, before, drain.remote.tree())
			assert.Empty(t, drain.remote.deletes)
			leftOut := drain.leftOut()
			require.Len(t, leftOut, 1, "said once per plan")
			assert.Equal(t, slog.LevelDebug, leftOut[0].Level)
			assert.Equal(t, tc.leftOut, leftOut[0].Attrs["left_out"])
		})
	}
}

// Validates: R-2.4.8
//
// Only a final drain leaves new items out: an ordinary run plans as before
// and downloads a file new on OneDrive.
func TestRunOnce_AnOrdinaryRunStillDownloadsANewRemoteFile(t *testing.T) {
	t.Parallel()

	drain := newCountedRelease(t, newReleaseRemote(t))
	drain.remote.addFile(t, "item-new", "item-docs", "new.txt", "new\n")

	report := runScenarioPass(t, drain.eng)

	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, 1, drain.downloads.snapshot()["item-new"])
	assert.FileExists(t, filepath.Join(drain.syncRoot, "Docs", "new.txt"))
	assert.Empty(t, drain.leftOut())
}

// Validates: R-2.4.8
//
// A synced file OneDrive moved into a folder the drain never synced moves
// there: the folder is created because the admitted move needs it, the new
// file beside it is still left out, and the release then takes the moved file
// and its folder with the rest of the tree. Nothing is downloaded, nothing
// holds the tree, and the moved file is in the trash.
func TestRunOnce_FinalDrainMovesASyncedFileIntoANewFolderAndReleasesBoth(t *testing.T) {
	t.Parallel()

	drain := newCountedRelease(t, newReleaseRemote(t))
	drain.remote.addFolder("item-new", "root", "New")
	drain.remote.addFile(t, "item-other", "item-new", "other.txt", "other\n")
	drain.remote.move("item-a", "item-new", "a.txt")
	synced := drain.downloads.snapshot()

	report := runReleaseDrain(t, drain.eng)

	assert.Equal(t, 1, report.Moves, "the synced file moves")
	assert.Equal(t, 1, report.FolderCreates, "into the folder the move needs")
	assert.Equal(t, synced, drain.downloads.snapshot(), "the new file beside it is not downloaded")
	assert.False(t, report.LocalTreeHolds().Any(), "%+v", report.LocalTreeHolds())
	assert.Equal(t, releaseSyncedRows+1, report.Released, "the moved file, its new folder and the rest of the tree")
	assert.Empty(t, releaseRootEntries(t, drain.syncRoot))
	assert.Empty(t, releaseBaselinePaths(t, drain.eng))
	assert.Empty(t, drain.remote.deletes)
	assert.Contains(t, harnessTrashContents(t, drain.eng), releaseContentA, "the moved file is in the trash")
}

// Validates: R-2.4.8
//
// A refusal that persists does not loop. The release takes the first two of
// three synced files and the local delete refuses the third, so the drain is
// kept and reports it, and every row from the refused entry on stays. The
// next drain downloads nothing back. A trash that no longer takes files
// refuses the third again: that drain makes no trash move either, and both
// drains keep the third as an item the release could not remove, named in
// the report and in status, its cause warned once. A content change no stat
// fact shows is not that kind of refusal: the refusal proves it, the file is
// work for the next drain, which uploads it and releases it.
func TestRunOnce_ARefusalThatPersistsNeverDownloadsWhatTheReleaseTook(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only trash")
	}

	for _, tc := range []struct {
		name string
		// trashFailsAfter makes the trash refuse every file after that many
		// moves; zero leaves it working.
		trashFailsAfter int32
		// changeUnseen changes the third file's content as no stat fact
		// shows.
		changeUnseen bool
	}{
		{name: "a trash that no longer takes files", trashFailsAfter: 2},
		{name: "content changed under equal size and times", changeUnseen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			drain := newThreeFileRelease(t)
			var trashed atomic.Int32
			trashDir := drain.eng.trash.Dir()
			drain.eng.execCfg.crashAt = func(point string) error {
				if point == crashPointLocalDeleteTrashed && trashed.Add(1) == tc.trashFailsAfter {
					return os.Chmod(trashDir, 0o500) //nolint:gosec // the test makes the trash refuse new entries
				}
				return nil
			}
			t.Cleanup(func() {
				assert.NoError(t, os.Chmod(trashDir, 0o700)) //nolint:gosec // and gives the trash back
			})
			if tc.changeUnseen {
				changeContentUnseen(t, drain.eng, drain.syncRoot, "Docs/c.txt", "C\n")
			}
			synced := drain.downloads.snapshot()

			first := runReleaseDrain(t, drain.eng)

			assert.Equal(t, 2, first.Released, "the first two files leave")
			assert.Equal(t, int32(2), trashed.Load())
			assertReleaseKeptTheThird(t, drain, first, tc.changeUnseen)
			assert.Equal(t, []string{"Docs/c.txt"}, releaseLocalFiles(t, drain.syncRoot))
			assert.Equal(t, []string{"Docs", "Docs/c.txt"}, releaseBaselinePaths(t, drain.eng),
				"every row from the refused entry on stays")

			second := runReleaseDrain(t, drain.eng)

			assert.Zero(t, second.Downloads)
			assert.Equal(t, synced, drain.downloads.snapshot(), "no drain downloads what the release took")
			for _, gone := range []string{"a.txt", "b.txt"} {
				assert.NoFileExists(t, filepath.Join(drain.syncRoot, releaseSyncedFolder, gone), "the first two stay gone")
			}
			if tc.changeUnseen {
				assert.Equal(t, 1, second.Uploads, "the proven change is uploaded")
				assert.Equal(t, int32(3), trashed.Load(), "and the file released")
				assert.False(t, second.LocalTreeHolds().Any(), "%+v", second.LocalTreeHolds())
				assert.Empty(t, releaseRootEntries(t, drain.syncRoot))
				assert.Empty(t, releaseBaselinePaths(t, drain.eng))
			} else {
				assert.Equal(t, int32(2), trashed.Load(), "the second drain moves nothing to the trash")
				assert.Zero(t, second.Released)
				assertReleaseKeptTheThird(t, drain, second, false)
				assert.Equal(t, []string{"Docs/c.txt"}, releaseLocalFiles(t, drain.syncRoot))
				assert.Equal(t, []string{"Docs", "Docs/c.txt"}, releaseBaselinePaths(t, drain.eng))
			}
			assert.Empty(t, drain.remote.deletes)
			wantWarnings := 1
			if tc.changeUnseen {
				wantWarnings = 0
			}
			assert.Len(t, drain.refusalWarnings(), wantWarnings, "the cause is warned once over both drains")
		})
	}
}

// refusalMessage starts the Warn a release gives when its local delete could
// not remove a synced entry.
const refusalMessage = "Could not move a synced entry of a removed shared folder to the trash or delete it"

// refusalWarnings is the Warn lines saying the release could not remove a
// synced entry.
func (c *countedRelease) refusalWarnings() []synctest.CapturedRecord {
	var out []synctest.CapturedRecord
	for _, record := range c.capture.AtLeast(slog.LevelWarn) {
		if strings.HasPrefix(record.Message, refusalMessage) {
			out = append(out, record)
		}
	}

	return out
}

// assertReleaseKeptTheThird checks how a drain of newThreeFileRelease reports
// Docs/c.txt, which the release kept: a proven content change is work for the
// next drain; any other refusal is an item the release could not remove,
// counted, named and recorded for status.
func assertReleaseKeptTheThird(t *testing.T, drain *countedRelease, report *Report, changeUnseen bool) {
	t.Helper()

	holds := report.LocalTreeHolds()
	if changeUnseen {
		assert.Equal(t, 1, holds.Left.WillRetry, "the proven change is work for the next drain")
		assert.Zero(t, holds.KeptNotRemoved.Count)
		assert.Empty(t, releaseKeptIssuePaths(t, drain.eng))
		return
	}
	assert.Zero(t, holds.Left.WillRetry, "a refusal no drain cures is not work to retry")
	assert.Equal(t, KeptEntries{Count: 1, Examples: []string{"Docs/c.txt"}}, holds.KeptNotRemoved)
	assert.Contains(t, holds.Reasons(), "1 synced item could not be moved to the trash or deleted (Docs/c.txt); "+
		"remove it by hand or fix what keeps it, then sync again")
	assert.Equal(t, []string{"Docs/c.txt"}, releaseKeptIssuePaths(t, drain.eng), "status shows it")
}

// newThreeFileRelease is a synced folder of three files, Docs/a.txt,
// Docs/b.txt and Docs/c.txt, settled so a scan trusts their rows.
func newThreeFileRelease(t *testing.T) *countedRelease {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", releaseSyncedFolder)
	for _, name := range []string{"a", "b", "c"} {
		remote.addFile(t, "item-"+name, "item-docs", name+".txt", name+"\n")
	}
	drain := newCountedRelease(t, remote)
	settleReleaseScenario(t, drain.eng)

	return drain
}

// Validates: R-2.4.8
//
// A synced folder, or the sync folder itself, that cannot be written to
// keeps the whole tree before anything is removed: nothing in it could
// leave, and a release that took what sorts before it would leave the rest
// for every later drain. The drain is kept and names the folder, which
// `status` shows (the sync folder itself only the report names), and a
// second drain does the same and downloads nothing.
func TestRunOnce_FinalDrainKeepsATreeWithAFolderThatCannotBeWrittenTo(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only folder")
	}

	for _, tc := range []struct {
		name   string
		folder string
		named  string
		issues []string
	}{
		{name: "a synced folder", folder: releaseSyncedFolder, named: "(Docs)", issues: []string{releaseSyncedFolder}},
		{name: "the sync folder itself", named: "(the shared folder itself)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			drain := newCountedRelease(t, newReleaseRemote(t))
			synced := drain.downloads.snapshot()
			dir := filepath.Join(drain.syncRoot, filepath.FromSlash(tc.folder))
			require.NoError(t, os.Chmod(dir, 0o555)) //nolint:gosec // the test makes its own folder read-only
			t.Cleanup(func() {
				assert.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // and gives the folder back
			})

			for pass := range 2 {
				report := runReleaseDrain(t, drain.eng)

				assert.Zero(t, report.Released, "pass %d removes nothing", pass)
				holds := report.LocalTreeHolds()
				assert.Equal(t, KeptEntries{Count: 1, Examples: []string{tc.folder}}, holds.KeptNotWritable)
				assert.Contains(t, holds.Reasons(), "1 folder cannot be written to "+tc.named+"; make it writable to finish")
				assert.Equal(t, tc.issues, releaseKeptIssuePaths(t, drain.eng), "pass %d", pass)
			}
			assert.Equal(t, []string{releaseFileA, releaseFileB, releaseFileTop}, releaseLocalFiles(t, drain.syncRoot))
			assert.Len(t, releaseBaselinePaths(t, drain.eng), releaseSyncedRows, "every row stays")
			assert.Equal(t, synced, drain.downloads.snapshot(), "nothing is downloaded")
			assert.Empty(t, listEngineTrash(t, drain.eng))
			assert.Empty(t, drain.remote.deletes)
		})
	}
}

// Validates: R-2.4.8
//
// A synced row without a modification time reads as changed to the survey:
// the release removes nothing and leaves the file to the next drain, whose
// scan records the time, and that drain releases the tree.
func TestReleaseLocalTree_ARowWithoutAModificationTimeWaitsForTheNextDrain(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot := newReleaseScenario(t, nil)
	result, err := eng.baseline.rawDB().ExecContext(t.Context(),
		`UPDATE baseline SET local_mtime = 0 WHERE path = ?`, releaseFileTop)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	require.NoError(t, eng.baseline.reloadBaselineCache(t.Context()))

	report := &Report{Mode: SyncBidirectional}
	require.NoError(t, eng.releaseLocalTree(t.Context(), report))

	assert.Zero(t, report.Released, "nothing leaves")
	assert.Equal(t, 1, report.LeftWork().WillRetry, "the file is work for the next drain")
	assert.Equal(t, []string{releaseFileA, releaseFileB, releaseFileTop}, releaseLocalFiles(t, syncRoot))

	next := runReleaseDrain(t, eng)

	assert.Positive(t, next.BaselineUpdates, "the scan records the time")
	assert.False(t, next.LocalTreeHolds().Any(), "%+v", next.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot))
	assert.Empty(t, remote.deletes)
}

// Validates: R-2.4.8
//
// When the refused entry's row cannot be put back, the rest of its batch
// still gets its rows back, in the same restore, and the release fails with
// the cause joined. The refused file stays on disk without its row, which
// the next run adopts.
func TestReleaseLocalTree_ARestoreThatFailsStillPutsBackTheRestOfTheBatch(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot := newReleaseScenario(t, nil)
	rewriteBaselineLocalHash(t, eng, releaseFileA)
	errRestore := errors.New("restore refused")
	var commits atomic.Int32
	eng.baseline.crashAt = func(point string) error {
		// The five rows are forgotten first; the next commit restores the
		// refused Docs/a.txt (Docs/sub/b.txt left before it).
		if point == crashPointCommitOutcome && commits.Add(1) == releaseSyncedRows+1 {
			return errRestore
		}
		return nil
	}

	report := &Report{Mode: SyncBidirectional}
	err := eng.releaseLocalTree(t.Context(), report)

	require.ErrorIs(t, err, errRestore)
	assert.Contains(t, err.Error(), "restoring the baseline row of "+releaseFileA)
	assert.Equal(t, 1, report.Released, "Docs/sub/b.txt left before the refusal")
	assert.Equal(t, []string{releaseSyncedFolder, "Docs/sub", releaseFileTop}, releaseBaselinePaths(t, eng),
		"the rest of the batch has its rows back")
	assert.Equal(t, []string{releaseFileA, releaseFileTop}, releaseLocalFiles(t, syncRoot))
	assert.Empty(t, remote.deletes)
}
