package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func seedUnavailableRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	remote.addFile(t, "item-a", "root", "a.txt", "alpha\n")
	remote.addFolder("item-dir", "root", "Dir")
	remote.addFile(t, "item-d1", "item-dir", "one.txt", "one\n")
	remote.addFile(t, "item-d2", "item-dir", "two.txt", "two\n")

	return remote
}

func requireUnavailableIssue(t *testing.T, eng *testEngine, path string) {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	for i := range issues {
		if issues[i].Path == path && issues[i].IssueType == syncdomain.IssueLocalUnsyncableReplacement {
			return
		}
	}
	require.Failf(t, "missing issue", "no %s issue at %s in %+v", syncdomain.IssueLocalUnsyncableReplacement, path, issues)
}

// Validates: R-6.2.14
func TestRunOnce_SyncedItemsMadeUnrepresentableAreNotDeletedOnline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		mutate     func(t *testing.T, syncRoot string)
		syncedPath string
		filter     syncdomain.ContentFilterConfig
	}{
		{
			name: "file renamed to a name OneDrive refuses",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, "a:b.txt")))
			},
			syncedPath: "a.txt",
		},
		{
			name: "populated folder renamed to a trailing-dot name",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "Dir"), filepath.Join(syncRoot, "Dir.")))
			},
			syncedPath: "Dir",
		},
		{
			name: "folder replaced by a symbolic link to outside the sync folder",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				outside := filepath.Join(filepath.Dir(syncRoot), "outside")
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "Dir"), outside))
				require.NoError(t, os.Symlink(outside, filepath.Join(syncRoot, "Dir")))
			},
			syncedPath: "Dir",
		},
		{
			name: "file replaced by a FIFO",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(syncRoot, "a.txt")))
				require.NoError(t, syscall.Mkfifo(filepath.Join(syncRoot, "a.txt"), 0o600))
			},
			syncedPath: "a.txt",
		},
		{
			name: "file renamed into an excluded dotfile name",
			mutate: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Rename(filepath.Join(syncRoot, "a.txt"), filepath.Join(syncRoot, ".a.txt")))
			},
			syncedPath: "a.txt",
			filter:     syncdomain.ContentFilterConfig{IgnoreDotfiles: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := seedUnavailableRemote(t)
			eng, syncRoot := newTestEngineInDirWith(t, t.Context(), remote.client(t), t.TempDir(), func(in *engineInputs) {
				in.ContentFilter = tc.filter
			})
			runScenarioPass(t, eng)

			tc.mutate(t, syncRoot)
			report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)

			assert.Empty(t, remote.deletes, "an unrepresentable synced item must not be deleted on OneDrive")
			assert.Zero(t, report.RemoteDeletes)
			requireUnavailableIssue(t, eng, tc.syncedPath)
		})
	}
}

// A disk mounted inside the sync folder was crossed under cross_filesystems
// and synced; later it is not mounted, leaving an empty folder on the parent
// filesystem. Three files are far below the delete-safety hold, so only the
// recorded mount boundary keeps them from being deleted on OneDrive (D-003).
//
// Validates: R-6.2.15
func TestRunOnce_UnmountedNestedDiskWithFewFilesIsNotDeleted(t *testing.T) {
	t.Parallel()

	remote, eng, _, _ := syncedCrossedDisksNowUnmounted(t, 3, "nas")

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Empty(t, remote.deletes, "an unmounted disk's synced files must not be deleted on OneDrive")
	assert.Zero(t, report.RemoteDeletes)
	requireIssueOfType(t, eng, "nas", syncdomain.IssueLocalMountMissing)
}

// diskFile is a file a disk held, by its path under the sync folder, with
// its content and modification time.
type diskFile struct {
	relPath string
	content []byte
	mtime   time.Time
}

// diskFileItemID is the OneDrive item of the i-th file on the disk mounted at
// disk (syncedCrossedDisksNowUnmounted).
func diskFileItemID(disk string, i int) string {
	return fmt.Sprintf("item-%s-%d", disk, i)
}

// syncedCrossedDisksNowUnmounted is a disk mounted at each of disks inside the
// sync folder, crossed under cross_filesystems and synced with files files
// each beside keep.txt, and then not mounted: the scan that crossed into it
// recorded the mount point, and the mount point is an empty folder now. It
// returns the sync folder and the files the disks held, so that a test can
// mount a disk again (remountDisk).
func syncedCrossedDisksNowUnmounted(
	t *testing.T,
	files int,
	disks ...string,
) (*moveScenarioRemote, *testEngine, string, []diskFile) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	for _, disk := range disks {
		remote.addFolder("item-"+disk, "root", disk)
		for i := range files {
			remote.addFile(t, diskFileItemID(disk, i), "item-"+disk, fmt.Sprintf("f%d.txt", i), "disk file\n")
		}
	}
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), remote.client(t), t.TempDir(), func(inputs *engineInputs) {
		inputs.ContentFilter = syncdomain.ContentFilterConfig{CrossFilesystems: true}
	})
	runScenarioPass(t, eng)

	var held []diskFile
	for _, disk := range disks {
		_, err := syncstoretest.RawDB(t, eng.baseline).ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES (?)`, disk)
		require.NoError(t, err)
		for i := range files {
			relPath := disk + "/" + fmt.Sprintf("f%d.txt", i)
			absPath := filepath.Join(syncRoot, filepath.FromSlash(relPath))
			info, statErr := os.Stat(absPath)
			require.NoError(t, statErr)
			content, readErr := os.ReadFile(absPath) //nolint:gosec // test reads its own temp tree
			require.NoError(t, readErr)
			held = append(held, diskFile{relPath: relPath, content: content, mtime: info.ModTime()})
			require.NoError(t, os.Remove(absPath))
		}
	}

	return remote, eng, syncRoot, held
}

// remountDisk puts back every file the disk mounted at disk held, with its
// content and modification time, as mounting the disk again does. A disk
// mounted again is on its own device, so the scan crosses into its folder
// rather than find a vanished mount point; on the one filesystem a test has,
// forgetting the recorded mount point stands in for that.
func remountDisk(t *testing.T, eng *testEngine, syncRoot, disk string, held []diskFile) {
	t.Helper()

	for _, f := range held {
		if !strings.HasPrefix(f.relPath, disk+"/") {
			continue
		}
		absPath := filepath.Join(syncRoot, filepath.FromSlash(f.relPath))
		require.NoError(t, os.WriteFile(absPath, f.content, 0o600))
		require.NoError(t, os.Chtimes(absPath, f.mtime, f.mtime))
	}
	_, err := syncstoretest.RawDB(t, eng.baseline).ExecContext(t.Context(), `DELETE FROM mount_boundaries WHERE path = ?`, disk)
	require.NoError(t, err)
}

// `drive rebind-root` accepts the sync folder as it is now and keeps every
// recorded mount point: a disk that is not mounted stays "Disk not mounted"
// after the rebind, so the next sync deletes none of its content on OneDrive
// and holds nothing for confirmation, however few files it held and however
// many disks are not mounted. Once each disk is mounted again, the next pass
// transfers, deletes and moves nothing and the condition clears. The scan's
// warning says what clears the condition and that the rebind is not it.
//
// Validates: R-6.2.15
func TestRunOnce_ARebindKeepsEveryDiskThatIsNotMounted(t *testing.T) {
	t.Parallel()

	detail := syncobserve.VanishedMountSkippedItem("nas").Detail
	assert.Contains(t, detail, "its synced content is kept until the disk is back")
	assert.Contains(t, detail, "'onedrive-go drive rebind-root' keeps it")
	assert.Contains(t, detail, "remove this empty folder")

	for _, tc := range []struct {
		name  string
		disks []string
		files int
	}{
		{name: "a few files", disks: []string{"nas"}, files: 3},
		{name: "more files than the hold lets through", disks: []string{"nas"}, files: syncplan.DeleteSafetyMinItems + 1},
		{name: "every disk that is not mounted", disks: []string{"nas", "usb"}, files: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote, eng, syncRoot, held := syncedCrossedDisksNowUnmounted(t, tc.files, tc.disks...)
			_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			require.Empty(t, remote.deletes, "while the disk is not mounted nothing is deleted")
			uploads, downloads := remote.uploads, remote.downloads

			require.NoError(t, eng.baseline.ClearSyncRootIdentity(t.Context()))
			report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assert.Empty(t, remote.deletes, "a rebind deletes nothing of a disk that is not mounted")
			assert.Zero(t, report.RemoteDeletes+report.HeldRemoteDeletes+report.LocalDeletes, "%+v", report)
			for _, disk := range tc.disks {
				requireIssueOfType(t, eng, disk, syncdomain.IssueLocalMountMissing)
			}

			for _, disk := range tc.disks {
				remountDisk(t, eng, syncRoot, disk, held)
			}
			report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
			require.NoError(t, err)
			assert.Empty(t, remote.deletes)
			assert.Empty(t, remote.moves)
			assert.Equal(t, uploads, remote.uploads, "nothing is uploaded")
			assert.Equal(t, downloads, remote.downloads, "nothing is downloaded")
			assert.Zero(t, report.LocalDeletes+report.RemoteDeletes+report.ConflictCopies+report.Failed, "%+v", report)
			for _, disk := range tc.disks {
				requireNoIssueOfType(t, eng, disk, syncdomain.IssueLocalMountMissing)
			}
			for _, f := range held {
				requireScenarioLocalFile(t, syncRoot, f.relPath, string(f.content))
			}
		})
	}
}

// A disk gone for good is let go of by removing its empty mount point: the
// next sync takes its content as deleted and holds every one of those deletes
// toward OneDrive for confirmation, however few. Once confirmed they run, the
// sync after that forgets the recorded mount point, and a folder made at that
// path again is an ordinary folder that syncs.
//
// Validates: R-6.2.5, R-6.2.15
func TestRunOnce_ADiskGoneForGoodIsForgottenOnceItsFolderIsRemoved(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot, _ := syncedCrossedDisksNowUnmounted(t, 3, "nas")
	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Empty(t, remote.deletes, "while the disk is not mounted nothing is deleted")

	require.NoError(t, os.Remove(filepath.Join(syncRoot, "nas")))
	requireMissingMountPointDeletesHeldThenConfirmed(t, remote, eng, "nas", 3)

	_, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	flags, err := eng.baseline.ListMountBoundaries(t.Context())
	require.NoError(t, err)
	assert.Empty(t, flags, "the mount point of a removed folder is forgotten")

	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "nas"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "nas", "new.txt"), []byte("new\n"), 0o600))
	uploads := remote.uploads
	_, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.Greater(t, remote.uploads, uploads, "a folder made there again syncs as an ordinary folder")
	requireNoIssueOfType(t, eng, "nas", syncdomain.IssueLocalMountMissing)
}

// A rebind onto a folder that lacks a recorded mount point's folder (a
// restored or re-pointed sync folder without the empty mount point) deletes
// nothing of that disk on OneDrive unconfirmed: its deletes are held,
// however few, until `sync --confirm-deletes`.
//
// Validates: R-6.2.5, R-6.2.15
func TestRunOnce_ARebindOntoAFolderWithoutAMountPointHoldsItsDeletes(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot, _ := syncedCrossedDisksNowUnmounted(t, 3, "nas")
	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "nas")))
	require.NoError(t, eng.baseline.ClearSyncRootIdentity(t.Context()))

	requireMissingMountPointDeletesHeldThenConfirmed(t, remote, eng, "nas", 3)
}

// requireMissingMountPointDeletesHeldThenConfirmed runs a sync whose local
// side lacks the folder of the disk mounted at disk, which held files files:
// every delete of the disk's folder and files toward OneDrive is held, a
// second sync still holds them, and a sync with --confirm-deletes runs them.
func requireMissingMountPointDeletesHeldThenConfirmed(
	t *testing.T,
	remote *moveScenarioRemote,
	eng *testEngine,
	disk string,
	files int,
) {
	t.Helper()

	deletes := files + 1
	for range 2 {
		report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
		assert.Empty(t, remote.deletes, "a missing mount point's deletes are held, however few")
		assert.Equal(t, deletes, report.HeldRemoteDeletes)
	}

	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{ConfirmDeletes: true})
	require.NoError(t, err)
	assert.Zero(t, report.HeldRemoteDeletes)
	assert.Len(t, remote.deletes, deletes, "confirmed, they are deleted on OneDrive")
	assert.Contains(t, remote.deletes, "item-"+disk)
	for i := range files {
		assert.Contains(t, remote.deletes, diskFileItemID(disk, i))
	}
}

// "Mount the disk again; that clears it and loses nothing": while a crossed
// disk is not mounted, the passes delete nothing on either side, and once it
// is back with what it held, the next pass transfers, deletes and moves
// nothing, the condition clears, and a dry run afterwards plans nothing.
//
// Validates: R-6.2.15
func TestRunOnce_ADiskMountedAgainLosesNothing(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot, held := syncedCrossedDisksNowUnmounted(t, 3, "nas")
	uploads, downloads := remote.uploads, remote.downloads
	for range 3 {
		_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}
	require.Empty(t, remote.deletes, "while the disk is not mounted nothing is deleted")

	remountDisk(t, eng, syncRoot, "nas", held)
	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Empty(t, remote.deletes)
	assert.Empty(t, remote.moves)
	assert.Equal(t, uploads, remote.uploads, "nothing is uploaded")
	assert.Equal(t, downloads, remote.downloads, "nothing is downloaded")
	assert.Zero(t, report.LocalDeletes+report.RemoteDeletes+report.ConflictCopies+report.Failed, "%+v", report)
	requireNoIssueOfType(t, eng, "nas", syncdomain.IssueLocalMountMissing)
	for _, f := range held {
		requireScenarioLocalFile(t, syncRoot, f.relPath, string(f.content))
	}

	report, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.Moves,
		"a dry run afterwards plans nothing: %+v", report)
}

// A file edited on OneDrive while its disk is not mounted is not downloaded
// into the empty mount point. Once the disk is back, the edit is downloaded
// over the copy the disk held, with nothing uploaded and no conflict copy.
//
// Validates: R-6.2.15
func TestRunOnce_AnEditOnOneDriveWhileItsDiskIsNotMountedIsDownloadedOnceItIsBack(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot, held := syncedCrossedDisksNowUnmounted(t, 3, "nas")
	uploads := remote.uploads
	edited := diskFileItemID("nas", 0)
	editedPath := filepath.Join(syncRoot, "nas", "f0.txt")
	remote.setContent(t, edited, "edited on OneDrive\n")

	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	assert.NoFileExists(t, editedPath, "nothing is downloaded into the empty mount point")
	require.Empty(t, remote.deletes)

	remountDisk(t, eng, syncRoot, "nas", held)
	report, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	requireScenarioLocalFile(t, syncRoot, "nas/f0.txt", "edited on OneDrive\n")
	assert.Equal(t, "edited on OneDrive\n", remote.contents[edited], "the copy the disk held is not uploaded over it")
	assert.Equal(t, uploads, remote.uploads)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, report.ConflictCopies)
	entries, err := os.ReadDir(filepath.Join(syncRoot, "nas"))
	require.NoError(t, err)
	assert.Len(t, entries, len(held), "no conflict copy beside the files")
}

// A rebind clears the sync-folder fingerprint and nothing else: the recorded
// mount points stay, so a disk that is not mounted is still told apart from
// deleted content (TestRunOnce_ARebindKeepsEveryDiskThatIsNotMounted).
//
// Validates: R-6.2.12, R-6.2.15
func TestClearSyncRootIdentity_KeepsRecordedMountPoints(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &syncfixture.EngineMockClient{})
	_, err := syncstoretest.RawDB(t, eng.baseline).ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES ('nas')`)
	require.NoError(t, err)

	require.NoError(t, eng.baseline.ClearSyncRootIdentity(t.Context()))

	flags, err := eng.baseline.ListMountBoundaries(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"nas"}, flags, "a rebind keeps every recorded mount point")
}

// Validates: R-6.4.9
func TestContentFilter_RestoreCopiesAreNeverObserved(t *testing.T) {
	t.Parallel()

	filter := syncdomain.NewContentFilter(syncdomain.ContentFilterConfig{})
	assert.False(t, filter.Visible("Docs/.onedrive-go-restore-123", syncdomain.ItemTypeFile))
	assert.False(t, filter.Visible("Docs/.onedrive-go.0123456789abcdef.detach", syncdomain.ItemTypeFile))
}

// Validates: R-6.2.12
func TestCountSyncFolderFiles_MissingFolderHoldsNoFiles(t *testing.T) {
	t.Parallel()

	count, err := syncdomain.CountSyncFolderFiles(filepath.Join(t.TempDir(), "missing"))
	require.NoError(t, err)
	assert.Zero(t, count)
}

func requireIssueOfType(t *testing.T, eng *testEngine, path string, issueType syncdomain.IssueType) {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	for i := range issues {
		if issues[i].Path == path && issues[i].IssueType == issueType {
			return
		}
	}
	require.Failf(t, "missing issue", "no %s issue at %s: %+v", issueType, path, issues)
}

func requireNoIssueOfType(t *testing.T, eng *testEngine, path string, issueType syncdomain.IssueType) {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	for i := range issues {
		require.False(t, issues[i].Path == path && issues[i].IssueType == issueType,
			"stale %s issue at %s: %+v", issueType, path, issues)
	}
}

// A synced file replaced by a symlink is unavailable truth and reported; once
// an ordinary file is back at the path, the report clears and the file syncs
// again. Found live on 2026-11-04: the stale report blocked the restored
// file's uploads for good.
//
// Validates: R-6.2.14
func TestRunOnce_ReplacedSyncedFileSyncsAgainOnceRestored(t *testing.T) {
	t.Parallel()

	remote := seedUnavailableRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	target := filepath.Join(syncRoot, "a.txt")
	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Symlink(filepath.Join(syncRoot, "keep.txt"), target))
	_, err := eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	requireUnavailableIssue(t, eng, "a.txt")

	require.NoError(t, os.Remove(target))
	require.NoError(t, os.WriteFile(target, []byte("alpha, edited\n"), 0o600))
	runScenarioPass(t, eng)

	item, ok := remote.itemAt("a.txt")
	require.True(t, ok)
	assert.Equal(t, "alpha, edited\n", remote.contents[item.ID], "the restored file's edit is uploaded")
	requireNoIssueOfType(t, eng, "a.txt", syncdomain.IssueLocalUnsyncableReplacement)
}

// A file sync set aside while removing it, left in the sync folder because the
// run was interrupted, is reported by every sync that finds it. Once the user
// deals with it (renames it back or deletes it), the next sync's full scan no
// longer finds it and the report clears.
//
// Validates: R-6.4.9
func TestRunOnce_InterruptedRemovalReportClearsOnceTheLeftoverIsGone(t *testing.T) {
	t.Parallel()

	const leftoverPath = "Dir/.onedrive-go.0123456789abcdef.detach"
	remote := seedUnavailableRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	leftover := filepath.Join(syncRoot, filepath.FromSlash(leftoverPath))
	require.NoError(t, os.WriteFile(leftover, []byte("half-removed\n"), 0o600))

	runScenarioPass(t, eng)
	assert.Equal(t, []string{leftoverPath}, interruptedRemovalStatusPaths(t, eng.baseline))
	runScenarioPass(t, eng)
	assert.Equal(t, []string{leftoverPath}, interruptedRemovalStatusPaths(t, eng.baseline),
		"a sync that still finds the leftover keeps reporting it")

	require.NoError(t, os.Remove(leftover))
	runScenarioPass(t, eng)

	requireNoIssueOfType(t, eng, leftoverPath, syncdomain.IssueLocalInterruptedRemoval)
	assert.Empty(t, interruptedRemovalStatusPaths(t, eng.baseline), "status no longer lists it")
}

// A folder a scan found to be the mount point of another filesystem is a read
// boundary while it is one. When the next scan finds it an ordinary folder on
// the sync folder's filesystem again, the boundary clears and changes under
// it sync.
//
// Validates: R-6.2.15
func TestRunOnce_MountBoundaryClearsWhenTheFolderIsPlainAgain(t *testing.T) {
	t.Parallel()

	remote := seedUnavailableRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	batch := localObservationFindingsBatchFromSkippedItems(eng.driveID, []syncdomain.SkippedItem{syncobserve.MountBoundarySkippedItem("Dir")})
	require.NoError(t, eng.baseline.ReconcileObservationFindings(t.Context(), &batch))

	require.NoError(t, os.Remove(filepath.Join(syncRoot, "Dir", "two.txt")))
	report := runScenarioPass(t, eng)

	assert.Equal(t, 1, report.RemoteDeletes, "a delete in a folder that is no longer a mount point is an ordinary delete")
	requireNoIssueOfType(t, eng, "Dir", syncdomain.IssueLocalMountBoundary)
}

// A crossed mount point that is not mounted now is a condition of its own,
// not the mount point sync does not cross, whose status text asks for the
// cross_filesystems the user already set. A row an earlier scan stored for it
// under the shared type is replaced by the next full scan, because the scan
// manages both types.
//
// Validates: R-6.2.15
func TestRunOnce_AVanishedCrossedMountIsMissingNotABoundary(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-nas", "root", "nas")
	remote.addFile(t, "item-n0", "item-nas", "n0.txt", "nas file\n")
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), remote.client(t), t.TempDir(), func(inputs *engineInputs) {
		inputs.ContentFilter = syncdomain.ContentFilterConfig{CrossFilesystems: true}
	})
	runScenarioPass(t, eng)

	_, err := syncstoretest.RawDB(t, eng.baseline).ExecContext(t.Context(), `INSERT INTO mount_boundaries (path) VALUES ('nas')`)
	require.NoError(t, err)
	stored := localObservationFindingsBatchFromSkippedItems(eng.driveID, []syncdomain.SkippedItem{syncobserve.MountBoundarySkippedItem("nas")})
	require.NoError(t, eng.baseline.ReconcileObservationFindings(t.Context(), &stored))
	require.NoError(t, os.Remove(filepath.Join(syncRoot, "nas", "n0.txt")))

	_, err = eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Empty(t, remote.deletes, "the synced content of a disk that is not mounted is kept on OneDrive")
	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	require.Len(t, issues, 1, "%+v", issues)
	assert.Equal(t, "nas", issues[0].Path)
	assert.Equal(t, syncdomain.IssueLocalMountMissing, issues[0].IssueType)
	assert.Equal(t, syncdomain.SKPermLocalRead("nas"), issues[0].ScopeKey, "still a read boundary")
}

// The condition of a crossed mount point that is not mounted clears when a
// full scan no longer finds it, as the mount being back makes it: the scan
// manages the type, and the folder's changes sync again.
//
// Validates: R-6.2.15
func TestRunOnce_MountMissingClearsWhenTheScanNoLongerFindsIt(t *testing.T) {
	t.Parallel()

	remote := seedUnavailableRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	missing := syncobserve.VanishedMountSkippedItem("Dir")
	require.Equal(t, syncdomain.IssueLocalMountMissing, missing.Reason)
	batch := localObservationFindingsBatchFromSkippedItems(eng.driveID, []syncdomain.SkippedItem{missing})
	require.NoError(t, eng.baseline.ReconcileObservationFindings(t.Context(), &batch))

	require.NoError(t, os.Remove(filepath.Join(syncRoot, "Dir", "two.txt")))
	report := runScenarioPass(t, eng)

	assert.Equal(t, 1, report.RemoteDeletes, "a delete in a folder that is a plain folder again is an ordinary delete")
	requireNoIssueOfType(t, eng, "Dir", missing.Reason)
}
