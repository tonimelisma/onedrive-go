package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sort"
	stdsync "sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The release tests run a removed shared folder's final drain on a synced
// tree: Docs/a.txt, Docs/sub/b.txt and top.txt, five baseline rows.
const (
	releaseFileA        = "Docs/a.txt"
	releaseFileB        = "Docs/sub/b.txt"
	releaseFileTop      = "top.txt"
	releaseContentA     = "a\n"
	releaseSyncedRows   = 5
	releaseSyncedFolder = "Docs"
)

// newReleaseRemote is the shared folder of the release tests.
func newReleaseRemote(t *testing.T) *moveScenarioRemote {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", releaseSyncedFolder)
	remote.addFolder("item-sub", "item-docs", "sub")
	remote.addFile(t, "item-a", "item-docs", "a.txt", releaseContentA)
	remote.addFile(t, "item-b", "item-sub", "b.txt", "b\n")
	remote.addFile(t, "item-top", "root", releaseFileTop, "top\n")

	return remote
}

func newReleaseScenario(t *testing.T, configure func(*engineInputs)) (*moveScenarioRemote, *testEngine, string) {
	t.Helper()

	remote := newReleaseRemote(t)
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), remote.client(t), t.TempDir(), configure)
	runScenarioPass(t, eng)

	return remote, eng, syncRoot
}

// rewriteBaselineLocalHash makes a synced file's content disagree with its
// baseline row while every stat fact still agrees, as a damaged block does:
// the row's local hash is rewritten and nothing else.
func rewriteBaselineLocalHash(t *testing.T, eng *testEngine, storedPath string) {
	t.Helper()

	result, err := eng.baseline.rawDB().ExecContext(t.Context(),
		`UPDATE baseline SET local_hash = ? WHERE path = ?`, testQuickXor("not what the file holds\n"), storedPath)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	require.NoError(t, eng.baseline.reloadBaselineCache(t.Context()))
}

// changeContentUnseen rewrites a synced file to content of the same size and
// makes its baseline row agree with every stat fact the file now has, as a
// change no stat fact shows does: the modification time is put back, and the
// row takes the new change time. Only reading the content tells it apart.
func changeContentUnseen(t *testing.T, eng *testEngine, syncRoot, storedPath, content string) {
	t.Helper()

	full := filepath.Join(syncRoot, filepath.FromSlash(storedPath))
	before, err := os.Stat(full)
	require.NoError(t, err)
	require.Len(t, content, int(before.Size()), "the change keeps the size")
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	require.NoError(t, os.Chtimes(full, before.ModTime(), before.ModTime()))
	after, err := os.Stat(full)
	require.NoError(t, err)
	ctime, ok := synctree.ChangeTimeFromFileInfo(after)
	require.True(t, ok)
	result, err := eng.baseline.rawDB().ExecContext(t.Context(),
		`UPDATE baseline SET local_ctime = ? WHERE path = ?`, ctime, storedPath)
	require.NoError(t, err)
	rows, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)
	require.NoError(t, eng.baseline.reloadBaselineCache(t.Context()))
}

// settleReleaseScenario moves the engine's clock well past the synced files'
// times, so a scan may trust a row whose stat facts all match (the
// racily-clean window has passed), and runs a pass that records those facts
// as the files have them now.
func settleReleaseScenario(t *testing.T, eng *testEngine) {
	t.Helper()

	installManualClockForTestEngine(eng, newManualClock(time.Now().Add(time.Hour)))
	runScenarioPass(t, eng)
}

// releaseBaselineRow is the baseline row at storedPath.
func releaseBaselineRow(t *testing.T, eng *testEngine, storedPath string) *BaselineEntry {
	t.Helper()

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	row, ok := bl.GetByPath(storedPath)
	require.True(t, ok, "baseline row for %s", storedPath)

	return row
}

// countingDownloads counts, per item, the downloads client serves.
func countingDownloads(client *engineMockClient) *releaseDownloadCounts {
	counts := &releaseDownloadCounts{byItem: map[string]int{}}
	download := client.downloadFn
	client.downloadFn = func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
		counts.add(itemID)
		return download(ctx, driveID, itemID, w)
	}

	return counts
}

// releaseDownloadCounts is a download count per item, safe for the transfer
// workers.
type releaseDownloadCounts struct {
	mu     stdsync.Mutex
	byItem map[string]int
}

func (c *releaseDownloadCounts) add(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byItem[key]++
}

func (c *releaseDownloadCounts) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return maps.Clone(c.byItem)
}

// runReleaseDrain runs the final drain multisync runs for a removed shared
// folder.
func runReleaseDrain(t *testing.T, eng *testEngine) *Report {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{FullReconcile: true, ReleaseLocalTree: true})
	require.NoError(t, err)

	return report
}

func releaseBaselinePaths(t *testing.T, eng *testEngine) []string {
	t.Helper()

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	var paths []string
	bl.ForEachPath(func(p string, entry *BaselineEntry) {
		if p != "" && entry.ItemType != ItemTypeRoot {
			paths = append(paths, p)
		}
	})
	sort.Strings(paths)

	return paths
}

func releaseKeptIssuePaths(t *testing.T, eng *testEngine) []string {
	t.Helper()

	issues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	var paths []string
	for i := range issues {
		if issues[i].IssueType == issueRemovedFolderKept {
			paths = append(paths, issues[i].Path)
		}
	}
	sort.Strings(paths)

	return paths
}

func releaseRootEntries(t *testing.T, syncRoot string) []string {
	t.Helper()

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// Validates: R-2.4.8, R-6.4.9
//
// A clean final drain releases the tree by the rule of a folder deleted on
// OneDrive: every synced file leaves for the trash, verified, every synced
// folder once empty, every baseline row in the commit of its delete. The
// alias root stays, empty, for the parent to remove; nothing is deleted on
// OneDrive.
func TestRunOnce_FinalDrainReleasesASyncedTreeToTheTrash(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot := newReleaseScenario(t, nil)
	before := remote.tree()

	report := runReleaseDrain(t, eng)

	assert.Equal(t, releaseSyncedRows, report.Released, "three files and two folders")
	assert.False(t, report.LocalTreeHolds().Any(), "nothing holds a released tree: %+v", report.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot), "the root is empty and stays for the parent")
	assert.Empty(t, releaseBaselinePaths(t, eng), "no baseline row outlives its file")
	assert.Empty(t, remote.deletes, "nothing is deleted on OneDrive")
	assert.Equal(t, before, remote.tree())
	assert.Empty(t, releaseKeptIssuePaths(t, eng))

	entries := listEngineTrash(t, eng)
	paths := make([]string, 0, len(entries))
	for i := range entries {
		assert.Equal(t, localtrash.ReasonDeleted, entries[i].Reason)
		paths = append(paths, entries[i].Path)
	}
	sort.Strings(paths)
	assert.Equal(t, []string{releaseFileA, releaseFileB, releaseFileTop}, paths, "every released file is in the trash")
}

// releaseKeptCase is one kind of entry under a released folder that is not
// in the baseline with matching content.
type releaseKeptCase struct {
	name      string
	configure func(*engineInputs)
	// place puts the entry in the synced tree; path is its store path.
	place func(t *testing.T, syncRoot string)
	path  string
	// keptNotOnOneDrive: the release keeps it as content sync does not
	// sync, records it and blocks; otherwise the run holds the tree by an
	// interrupted removal and the release does not run at all.
	keptNotOnOneDrive bool
}

func releaseKeptCases() []releaseKeptCase {
	return []releaseKeptCase{
		{
			name:      "a file the content filter hides",
			configure: func(inputs *engineInputs) { inputs.ContentFilter = ContentFilterConfig{IgnoreDotfiles: true} },
			place: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeLocalFile(t, syncRoot, "Docs/.notes", "mine\n")
			},
			path: "Docs/.notes", keptNotOnOneDrive: true,
		},
		{
			name: "a symbolic link sorted after the folder's files",
			place: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(syncRoot, "Docs", "z-link")))
			},
			path: "Docs/z-link", keptNotOnOneDrive: true,
		},
		{
			name: "a FIFO",
			place: func(t *testing.T, syncRoot string) {
				t.Helper()
				require.NoError(t, syscall.Mkfifo(filepath.Join(syncRoot, "Docs", "sub", "pipe"), 0o600))
			},
			path: "Docs/sub/pipe", keptNotOnOneDrive: true,
		},
		{
			name: "a restore copy, a name sync reserves",
			place: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeLocalFile(t, syncRoot, ".onedrive-go-restore-1234", "half a restore\n")
			},
			path: ".onedrive-go-restore-1234", keptNotOnOneDrive: true,
		},
		{
			name: "a file an interrupted removal set aside",
			place: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeLocalFile(t, syncRoot, "Docs/.onedrive-go.0123456789abcdef.detach", "set aside\n")
			},
			path: "Docs/.onedrive-go.0123456789abcdef.detach",
		},
	}
}

// Validates: R-2.4.8
//
// An entry the local delete rule does not remove keeps the whole tree while
// the shared folder can be read: nothing is removed, and a second drain with
// nothing changed does the same nothing. Content sync does not sync is
// counted, named and recorded
// for status; a file an interrupted removal set aside holds the tree before
// the release runs.
func TestRunOnce_FinalDrainKeepsTheWholeTreeForWhatTheDeleteRuleDoesNotRemove(t *testing.T) {
	t.Parallel()

	for _, tc := range releaseKeptCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote, eng, syncRoot := newReleaseScenario(t, tc.configure)
			tc.place(t, syncRoot)

			for pass := range 2 {
				report := runReleaseDrain(t, eng)

				assert.Zero(t, report.Released, "pass %d removes nothing", pass)
				assert.Zero(t, report.Downloads+report.Uploads, "pass %d transfers nothing", pass)
				holds := report.LocalTreeHolds()
				require.True(t, holds.Any(), "pass %d: the tree is kept", pass)
				if tc.keptNotOnOneDrive {
					assert.Equal(t, KeptEntries{Count: 1, Examples: []string{tc.path}}, holds.KeptNotOnOneDrive)
					assert.Equal(t, []string{tc.path}, releaseKeptIssuePaths(t, eng), "status shows what is kept")
					require.Len(t, holds.keptReasons(), 1)
					assert.Contains(t, holds.keptReasons()[0], "folder still contains files that are not on OneDrive ("+tc.path+")")
				} else {
					assert.Equal(t, 1, holds.SetAsideFiles)
					assert.Empty(t, releaseKeptIssuePaths(t, eng))
				}
			}
			assert.Len(t, releaseBaselinePaths(t, eng), releaseSyncedRows, "every row stays")
			for _, synced := range []string{releaseFileA, releaseFileB, releaseFileTop} {
				assert.FileExists(t, filepath.Join(syncRoot, filepath.FromSlash(synced)))
			}
			assert.Empty(t, remote.deletes)
			assert.Empty(t, listEngineTrash(t, eng))
		})
	}
}

// Validates: R-2.4.8
//
// A visible file the drain's last scan did not see, a new one or one whose
// content changed, is ordinary left work, and the next final drain uploads it
// and releases the whole tree. What the survey sees keeps every entry and
// row, and it sees an edit that put the modification time back by the change
// time, as the scan's own fast path does. (A change no stat fact shows is
// left to the local delete's content check:
// TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain.)
func TestReleaseLocalTree_ANewOrChangedFileIsLeftForTheNextDrain(t *testing.T) {
	t.Parallel()

	allSynced := []string{releaseSyncedFolder, releaseFileA, "Docs/sub", releaseFileB, releaseFileTop}
	allFiles := []string{releaseFileA, releaseFileB, releaseFileTop}
	for _, tc := range []struct {
		name      string
		change    func(t *testing.T, syncRoot string)
		path      string
		wantFiles []string
	}{
		{
			name: "a new file", path: "Docs/new.txt",
			change: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeLocalFile(t, syncRoot, "Docs/new.txt", "written after the scan\n")
			},
			wantFiles: []string{releaseFileA, "Docs/new.txt", releaseFileB, releaseFileTop},
		},
		{
			name: "a file edited to a new size", path: releaseFileA,
			change: func(t *testing.T, syncRoot string) {
				t.Helper()
				writeLocalFile(t, syncRoot, releaseFileA, "edited after the scan\n")
			},
			wantFiles: allFiles,
		},
		{
			name: "a file edited in place with its time put back", path: releaseFileA,
			change: func(t *testing.T, syncRoot string) {
				t.Helper()
				full := filepath.Join(syncRoot, filepath.FromSlash(releaseFileA))
				info, err := os.Stat(full)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(full, []byte("A\n"), 0o600))
				require.NoError(t, os.Chtimes(full, info.ModTime(), info.ModTime()))
			},
			wantFiles: allFiles,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote, eng, syncRoot := newReleaseScenario(t, nil)
			tc.change(t, syncRoot)

			report := &Report{Mode: SyncBidirectional}
			require.NoError(t, eng.releaseLocalTree(t.Context(), report))

			assert.Zero(t, report.Released, "the survey sees the change: nothing leaves")
			assert.Equal(t, 1, report.LeftWork().WillRetry, "the file is work for the next drain")
			assert.Zero(t, report.KeptNotOnOneDrive.Count)
			assert.Empty(t, releaseKeptIssuePaths(t, eng), "left work is not kept")
			assert.Equal(t, tc.wantFiles, releaseLocalFiles(t, syncRoot))
			assert.Equal(t, allSynced, releaseBaselinePaths(t, eng), "every entry keeps its row")
			assert.Empty(t, remote.deletes)

			next := runReleaseDrain(t, eng)
			assert.False(t, next.LocalTreeHolds().Any(), "the next drain uploads it and releases: %+v", next.LocalTreeHolds())
			assert.Empty(t, releaseRootEntries(t, syncRoot))
			assert.Empty(t, releaseBaselinePaths(t, eng))
			assert.NotEqual(t, releaseContentA, remote.tree()[tc.path], "the change reached OneDrive")
			assert.Empty(t, remote.deletes)
		})
	}
}

// Validates: R-2.4.8, R-2.2.4, R-6.2.4
//
// A file whose content no longer matches its baseline hash while its size,
// times and identity all agree with its row passes the survey; the local
// delete's content check refuses it. That refusal proves the change, so the
// row comes back without its change time, the next drain's scan reads the
// file instead of trusting the row, uploads it, and the release completes.
// What the first release took before the refusal is never downloaded back.
func TestRunOnce_AReleaseRefusedForAnUnseenChangeUploadsItOnTheNextDrain(t *testing.T) {
	t.Parallel()

	remote := newReleaseRemote(t)
	client := remote.client(t)
	downloads := countingDownloads(client)
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), client, t.TempDir(), nil)
	runScenarioPass(t, eng)
	synced := downloads.snapshot()
	settleReleaseScenario(t, eng)
	changeContentUnseen(t, eng, syncRoot, releaseFileA, "A\n")

	first := runReleaseDrain(t, eng)
	assert.True(t, first.LocalTreeHolds().Any(), "the refused file holds the tree")
	assert.FileExists(t, filepath.Join(syncRoot, filepath.FromSlash(releaseFileA)))
	assert.Zero(t, releaseBaselineRow(t, eng, releaseFileA).LocalCtime,
		"the proven change leaves the row without a change time")

	second := runReleaseDrain(t, eng)
	assert.Equal(t, 1, second.Uploads, "the next scan reads the file and uploads it")
	assert.Equal(t, "A\n", remote.tree()[releaseFileA])
	assert.False(t, second.LocalTreeHolds().Any(), "%+v", second.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot))
	assert.Empty(t, releaseBaselinePaths(t, eng))
	assert.Empty(t, remote.deletes)
	assert.Equal(t, synced, downloads.snapshot(), "neither drain downloads anything")
}

// Validates: R-2.2.4, R-6.2.4
//
// The same proven change on the ordinary path: OneDrive deletes a file whose
// content changed unseen, the local delete's content check refuses it, and the
// next pass reads the file and plans what a local edit beside a remote delete
// gets, an upload that puts it back, instead of the same delete again until it
// is parked.
func TestRunOnce_ALocalDeleteRefusedForAnUnseenChangePlansTheLocalEditNext(t *testing.T) {
	t.Parallel()

	remote := newReleaseRemote(t)
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	settleReleaseScenario(t, eng)
	rewriteBaselineLocalHash(t, eng, releaseFileTop)
	remote.deleteRemotely("item-top")

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, report.Superseded, "the delete is refused once")
	assert.Equal(t, 1, report.Uploads, "the next pass uploads the changed file in place of the deleted item")
	assert.FileExists(t, filepath.Join(syncRoot, releaseFileTop))
	assert.Equal(t, "top\n", remote.tree()[releaseFileTop], "the file is on OneDrive again")
	retry, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, retry, "nothing is parked")
}

// Validates: R-2.4.8
//
// While the shared folder can be read, the release stops at the first entry
// the local delete refuses: the rest of that batch gets its rows back and no
// later batch starts, so nothing after the refused file is touched. Every
// entry that remains has its row.
func TestReleaseLocalTree_AReadableFolderStopsAtTheFirstRefusal(t *testing.T) {
	t.Parallel()

	const (
		files   = releaseBatchSize + 44
		refused = 100
	)
	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-docs", "root", releaseSyncedFolder)
	for i := range files {
		remote.addFile(t, fmt.Sprintf("item-%03d", i), "item-docs", fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("file %d\n", i))
	}
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	rewriteBaselineLocalHash(t, eng, fmt.Sprintf("Docs/f%03d.txt", refused))

	report := &Report{Mode: SyncBidirectional}
	require.NoError(t, eng.releaseLocalTree(t.Context(), report))

	assert.Equal(t, refused, report.Released, "what sorts before the refusal leaves")
	assert.Equal(t, 1, report.LeftWork().WillRetry, "the refused file is work for the next drain")
	rows := releaseBaselinePaths(t, eng)
	assert.Contains(t, rows, releaseSyncedFolder)
	for i := range files {
		storedPath := fmt.Sprintf("Docs/f%03d.txt", i)
		if i < refused {
			assert.NoFileExists(t, filepath.Join(syncRoot, filepath.FromSlash(storedPath)))
			assert.NotContains(t, rows, storedPath)
			continue
		}
		assert.FileExists(t, filepath.Join(syncRoot, filepath.FromSlash(storedPath)), "nothing after the refusal is removed")
		assert.Contains(t, rows, storedPath, "every entry that remains keeps its row")
	}
	assert.Empty(t, remote.deletes)
}

// Validates: R-2.4.8, R-6.6.30
//
// Junk the released root could not lose is kept and named, and the reason it
// could not go is said at Warn with the error, once: the next drain that
// meets the same junk says it at Debug.
func TestReleaseLocalTree_JunkThatCannotBeRemovedIsKeptAndItsCauseWarned(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only folder")
	}

	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), &engineMockClient{}, t.TempDir(), func(inputs *engineInputs) {
		inputs.ContentFilter = ContentFilterConfig{IgnoreJunkFiles: true}
	})
	capture, logger := synctest.NewLogCapture()
	eng.logger = logger
	writeLocalFile(t, syncRoot, ".DS_Store", "finder\n")
	require.NoError(t, os.Chmod(syncRoot, 0o500)) //nolint:gosec // the test makes its own folder read-only
	t.Cleanup(func() {
		assert.NoError(t, os.Chmod(syncRoot, 0o700)) //nolint:gosec // and gives the folder back
	})
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	for drain := range 2 {
		survey, emptyErr := eng.emptyReleasedRoot(bl)
		require.NoError(t, emptyErr)
		assert.Equal(t, []string{".DS_Store"}, survey.notOnOneDrive, "drain %d: the junk is kept and named", drain)
	}

	warnings := capture.AtLeast(slog.LevelWarn)
	require.Len(t, warnings, 1, "the second drain does not warn again")
	assert.Contains(t, warnings[0].Message, "Could not remove junk from the local folder of a removed shared folder")
	assert.NotEmpty(t, warnings[0].Attrs["error"], "the warning carries the cause")
	assert.True(t, capture.Contains("junk in the local folder of a removed shared folder still cannot be removed"),
		"the repeat is said at Debug")
}

// releaseLocalFiles lists the regular files under syncRoot by store path.
func releaseLocalFiles(t *testing.T, syncRoot string) []string {
	t.Helper()

	var files []string
	require.NoError(t, filepath.WalkDir(syncRoot, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			rel, relErr := filepath.Rel(syncRoot, p)
			if relErr != nil {
				return fmt.Errorf("relative path of %s: %w", p, relErr)
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	}))
	sort.Strings(files)

	return files
}

// Validates: R-2.4.8
//
// Bundled junk is disposable under the configured rule, in a released folder
// and directly in the root alike.
func TestRunOnce_FinalDrainRemovesBundledJunkUnderTheConfiguredRule(t *testing.T) {
	t.Parallel()

	_, eng, syncRoot := newReleaseScenario(t, func(inputs *engineInputs) {
		inputs.ContentFilter = ContentFilterConfig{IgnoreJunkFiles: true}
	})
	writeLocalFile(t, syncRoot, "Docs/.DS_Store", "finder\n")
	writeLocalFile(t, syncRoot, ".DS_Store", "finder\n")

	report := runReleaseDrain(t, eng)

	assert.False(t, report.LocalTreeHolds().Any(), "%+v", report.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot))
}

// Validates: R-2.4.8
//
// A download partial an earlier run left is the transfer machinery's own, and
// the post-sync artifact cleanup removes it before the release, even in a
// drain that executed nothing; otherwise it would keep the tree on every
// drain as a name sync reserves.
func TestRunOnce_FinalDrainRemovesALeftoverDownloadPartialFirst(t *testing.T) {
	t.Parallel()

	_, eng, syncRoot := newReleaseScenario(t, nil)
	writeLocalFile(t, syncRoot, "Docs/.onedrive-go.a.txt.partial", "half a download\n")

	report := runReleaseDrain(t, eng)

	assert.Zero(t, report.Succeeded+report.Failed, "the drain itself had nothing to do")
	assert.False(t, report.LocalTreeHolds().Any(), "%+v", report.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot))
}

// Validates: R-2.4.8
//
// A synced file already gone from the tree, deleted after the drain's last
// scan or the local copy of a delete the hold kept from OneDrive, has nothing
// to remove: its row leaves with the release and nothing is deleted on
// OneDrive.
func TestReleaseLocalTree_ASyncedFileAlreadyGoneLeavesWithItsRow(t *testing.T) {
	t.Parallel()

	remote, eng, syncRoot := newReleaseScenario(t, nil)
	require.NoError(t, os.Remove(filepath.Join(syncRoot, releaseFileTop)))

	report := &Report{Mode: SyncBidirectional}
	require.NoError(t, eng.releaseLocalTree(t.Context(), report))

	assert.False(t, report.LocalTreeHolds().Any(), "%+v", report.LocalTreeHolds())
	assert.Empty(t, releaseRootEntries(t, syncRoot))
	assert.Empty(t, releaseBaselinePaths(t, eng))
	assert.Empty(t, remote.deletes)
	assert.Contains(t, remote.tree(), releaseFileTop, "the file stays on OneDrive")
}

// releaseDeniedClient is the scenario remote's client whose listing of the
// whole folder answers 403 while denied is set: the shared folder can no
// longer be read.
func releaseDeniedClient(t *testing.T, remote *moveScenarioRemote, denied *atomic.Bool) *engineMockClient {
	t.Helper()

	client := remote.client(t)
	listDelta := client.deltaFn
	client.deltaFn = func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
		if denied.Load() {
			return nil, graph.ErrForbidden
		}
		return listDelta(ctx, driveID, token)
	}

	return client
}

// Validates: R-2.4.8
//
// When the shared folder can no longer be read, its drain can never finish,
// and the same rule still holds: what matches the baseline leaves, a change
// that cannot be uploaded stays, named as such, and the folder that holds it
// is not reported as an entry the delete could not remove. When the folder is shared
// again at the same path, the next sync plans no delete in it: the released
// files' rows left with them, so they are downloaded again, and the kept
// changes are uploaded.
func TestRunOnce_UnreadableSharedFolderReleasesWhatMatchesAndTheReAddPlansNoDelete(t *testing.T) {
	t.Parallel()

	remote := newReleaseRemote(t)
	var denied atomic.Bool
	capture, logger := synctest.NewLogCapture()
	eng, syncRoot := newTestEngineWithLogger(t, releaseDeniedClient(t, remote, &denied), logger)
	runScenarioPass(t, eng)
	writeLocalFile(t, syncRoot, releaseFileA, "edited, never uploaded\n")
	writeLocalFile(t, syncRoot, "Docs/new.txt", "new, never uploaded\n")

	denied.Store(true)
	for pass := range 2 {
		report := runReleaseDrain(t, eng)
		holds := report.LocalTreeHolds()
		assert.True(t, holds.Any(), "pass %d: the drain cannot finish", pass)
		assert.Equal(t, KeptEntries{Count: 2, Examples: []string{releaseFileA, "Docs/new.txt"}}, holds.KeptNotUploaded)
		require.NotEmpty(t, holds.keptReasons())
		assert.Contains(t, holds.keptReasons()[0], "2 changes could not be uploaded")
		assert.Zero(t, holds.KeptNotRemoved.Count, "pass %d: the folder that holds them is not a refusal of its own", pass)
	}
	for _, record := range capture.AtLeast(slog.LevelWarn) {
		assert.NotContains(t, record.Message, refusalMessage, "a folder kept for the changes in it is not warned about")
	}
	assert.Equal(t, []string{releaseFileA, "Docs/new.txt"}, releaseKeptIssuePaths(t, eng))
	assert.NoFileExists(t, filepath.Join(syncRoot, filepath.FromSlash(releaseFileB)), "a matching file leaves")
	assert.NoFileExists(t, filepath.Join(syncRoot, releaseFileTop))
	assert.Equal(t, []string{releaseSyncedFolder, releaseFileA}, releaseBaselinePaths(t, eng),
		"only the kept file and its folder keep their rows")
	assert.Empty(t, remote.deletes)

	// The folder is shared again: an ordinary sync.
	denied.Store(false)
	runScenarioPass(t, eng)

	assert.Empty(t, remote.deletes, "no file missing locally is deleted in the shared folder")
	assert.Equal(t, "edited, never uploaded\n", remote.tree()[releaseFileA])
	assert.Equal(t, "new, never uploaded\n", remote.tree()["Docs/new.txt"])
	assert.FileExists(t, filepath.Join(syncRoot, filepath.FromSlash(releaseFileB)), "released files come back down")
	assert.FileExists(t, filepath.Join(syncRoot, releaseFileTop))
	assert.Empty(t, releaseKeptIssuePaths(t, eng), "a sync of the shared folder clears what the release kept")
}

// Validates: R-2.4.8, R-6.5.2
//
// A crash between any two steps of the release, in either crash model,
// resumes to the same end with no delete in the shared folder: a row leaves
// the store (checkpointed) before its file leaves the tree, so a crash can
// leave a file without its row, which the next run adopts or keeps as a
// conflict copy, but never a row without its file. A file the crashed release
// already took is not downloaded back. The next drain then releases the whole
// tree.
func TestRunOnce_ACrashDuringTheReleaseResumesWithNoRemoteDelete(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		point      string
		occurrence int
	}{
		{point: crashPointCommitOutcome, occurrence: 1},
		{point: crashPointCommitOutcome, occurrence: 3},
		{point: crashPointLocalDeleteTrashed, occurrence: 1},
		{point: crashPointLocalDeleteTrashed, occurrence: 2},
	} {
		for _, powerLoss := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%d", crashCaseName(tc.point, powerLoss), tc.occurrence), func(t *testing.T) {
				t.Parallel()

				capture, recovered := crashDuringRelease(t, tc.point, tc.occurrence, powerLoss)

				assert.Empty(t, capture.remote.deletes, "the crashed release deleted nothing on OneDrive")
				report := runReleaseDrain(t, recovered.eng)
				assert.Empty(t, recovered.remote.deletes, "the recovered drain deletes nothing on OneDrive")
				assert.Zero(t, report.Downloads, "nothing the crashed release took is downloaded back")
				assert.False(t, report.LocalTreeHolds().Any(), "the release finishes: %+v", report.LocalTreeHolds())
				assert.Empty(t, releaseRootEntries(t, recovered.syncRoot))
				assert.Empty(t, releaseBaselinePaths(t, recovered.eng))
				for _, content := range []string{releaseContentA, "b\n", "top\n"} {
					assert.Contains(t, harnessTrashContents(t, recovered.eng), content, "nothing is lost")
				}
			})
		}
	}
}

// crashDuringRelease syncs the release scenario, then runs its final drain
// with a capture at the occurrence-th time the release reaches point, and
// reopens the snapshot.
func crashDuringRelease(t *testing.T, point string, occurrence int, powerLoss bool) (*crashCapture, *syncHarness) {
	t.Helper()

	remote := newReleaseRemote(t)
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	_, err := h.eng.baseline.rawDB().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)

	capture := &crashCapture{
		point: point, powerLoss: powerLoss,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	var seen atomic.Int32
	hook := func(reached string) error {
		if reached == point && int(seen.Add(1)) == occurrence {
			capture.once.Do(func() { capture.err = capture.capture() })
		}
		return nil
	}
	h.eng.execCfg.crashAt = hook
	h.eng.baseline.crashAt = hook
	runReleaseDrain(t, h.eng)
	recovered := capture.reopen(t)

	return capture, recovered
}

// Validates: R-2.4.8
//
// The release runs after a drain that holds nothing, and, when the shared
// folder can no longer be read, after one held only by the work that cannot
// finish; a dry run, a held delete of a local file or a set-aside file still
// stops it.
func TestReleaseLocalTree_RunsOnlyWhenTheDrainLetsIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		holds       LocalTreeHolds
		unreachable bool
		want        bool
	}{
		{name: "nothing holds the tree", want: true},
		{name: "work left", holds: LocalTreeHolds{Left: LeftWork{StillOpen: 1}}},
		{name: "work left while unreadable", holds: LocalTreeHolds{Left: LeftWork{StillOpen: 1}}, unreachable: true, want: true},
		{name: "a dry run while unreadable", holds: LocalTreeHolds{DryRun: true}, unreachable: true},
		{name: "a held local delete while unreadable", holds: LocalTreeHolds{HeldLocalDeletes: 1}, unreachable: true},
		{name: "a set-aside file while unreadable", holds: LocalTreeHolds{SetAsideFiles: 1}, unreachable: true},
		{name: "no report while unreadable", holds: LocalTreeHolds{NoReport: true}, unreachable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, releaseMayRun(&tc.holds, tc.unreachable))
		})
	}
}
