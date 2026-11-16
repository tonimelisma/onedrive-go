package driveops

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	stdsync "sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// wideTree is a tree of folders folders under the root, each holding
// filesPerFolder files of one byte.
func wideTree(folders, filesPerFolder int) *fakeTree {
	tree := newFakeTree()
	for i := range folders {
		folder := tree.addFolder(treeRootID, fmt.Sprintf("folder-%04d", i))
		for j := range filesPerFolder {
			tree.addFile(folder, fmt.Sprintf("file-%04d.bin", j), 1)
		}
	}

	return tree
}

// A folder of 10,000 files runs exactly TreeWorkers transfers at once: the
// pool is the bound, not the number of files.
//
// Validates: R-1.2.1
func TestDownloadTree_InFlightNeverExceedsWorkers(t *testing.T) {
	t.Parallel()

	const files = 10_000
	tree := newFakeTree()
	for i := range files {
		tree.addFile(treeRootID, fmt.Sprintf("file-%05d.bin", i), 1)
	}
	transfers := newGatedFiles(files)

	run := startTreeDownload(t.Context(), tree, transfers, t.TempDir())
	transfers.waitWorkersBusy(t)
	transfers.openGate()
	run.wait(t)

	require.NoError(t, run.err)
	assert.Equal(t, int64(TreeWorkers), transfers.peak.Load(), "transfers running at once")
	assert.Equal(t, files, run.summary.Files)
	assert.Zero(t, run.summary.Failed)
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// oneFilePerFolder is a tree of folders folders under the root with one file
// in each, the folders' IDs in listing order, and a signal closed when the
// walk lists the folder at index waitsAt.
func oneFilePerFolder(folders, waitsAt int) (tree *fakeTree, ids []string, reached <-chan struct{}) {
	tree = wideTree(folders, 1)
	ids = make([]string, folders)
	for i := range folders {
		ids[i] = treeChildID(treeRootID, fmt.Sprintf("folder-%04d", i))
	}
	listed := make(chan struct{})
	var listedOnce stdsync.Once
	tree.onList = func(folderID string) {
		if folderID == ids[waitsAt] {
			listedOnce.Do(func() { close(listed) })
		}
	}

	return tree, ids, listed
}

// downloadUntilTheQueueIsFull downloads tree through transfers, whose gate
// stays closed, until started transfers have begun and the walk has listed
// the folder whose file finds the queue full (reached); then it cancels the
// download and returns once the download has.
func downloadUntilTheQueueIsFull(
	t *testing.T, tree *fakeTree, transfers *gatedFiles, started int, reached <-chan struct{},
) *treeRun {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	run := startTreeDownload(ctx, tree, transfers, t.TempDir())
	transfers.waitStarted(t, started)
	awaitClosed(t, reached, "the walk did not reach the folder of the file that finds the queue full")
	cancel()
	run.wait(t)

	return run
}

// While every worker is busy and the queue is full, the walk waits: it does
// not list the next folder. With a file in each folder, the folder holding
// file 2×TreeWorkers+1 is the last one listed.
//
// Validates: R-1.2.1
func TestDownloadTree_WalkBlocksWhileWorkersAreBusy(t *testing.T) {
	t.Parallel()

	const folders = 20
	const queued = 2 * TreeWorkers
	tree, ids, reached := oneFilePerFolder(folders, queued)

	run := downloadUntilTheQueueIsFull(t, tree, newGatedFiles(folders), TreeWorkers, reached)

	assert.Equal(t, append([]string{treeRootID}, ids[:queued+1]...), tree.listedFolders(),
		"no folder past the one whose file waits for the queue is listed")
	require.ErrorIs(t, run.err, context.Canceled)
	assertEveryListedFileAccounted(t, tree, run)
}

// A folder get or put transfers four files at once, the number R-1.2.1 and
// R-1.3.1 state: in a tree of many more files, four transfers start while
// every one of them waits, and no fifth.
//
// Validates: R-1.2.1, R-1.3.1
func TestTreeWorkers_FourFilesTransferAtOnce(t *testing.T) {
	t.Parallel()

	const required = 4
	require.Equal(t, required, TreeWorkers, "the files a folder get or put transfers at once")

	const folders = 5 * required
	tree, _, reached := oneFilePerFolder(folders, 2*required)
	transfers := newGatedFiles(folders)

	downloadUntilTheQueueIsFull(t, tree, transfers, required, reached)

	assert.Equal(t, int64(required), transfers.peak.Load(), "transfers in flight at the peak")
	assert.Len(t, transfers.transferred(), required, "transfers that started while the first ones waited")
}

// stackDumpStart is the first buffer size for a dump of every goroutine.
const stackDumpStart = 1 << 20

// treePoolGoroutines counts the goroutines of the process that run a tree
// transfer's worker or collector loop.
func treePoolGoroutines() int {
	dump := make([]byte, stackDumpStart)
	for {
		n := runtime.Stack(dump, true)
		if n < len(dump) {
			dump = dump[:n]
			break
		}
		dump = make([]byte, 2*len(dump))
	}

	count := 0
	for _, stack := range strings.Split(string(dump), "\n\n") {
		if strings.Contains(stack, "driveops.(*treePool).work(") || strings.Contains(stack, "driveops.(*treePool).collect(") {
			count++
		}
	}

	return count
}

// The transfer runs TreeWorkers workers and one collector for a tree of a
// thousand folders and ten thousand files, and all of them have ended when it
// returns.
//
// Validates: R-1.2.1
func TestDownloadTree_GoroutinesDoNotScaleWithFilesOrFolders(t *testing.T) {
	// Not parallel: the count covers the whole process, and no other test
	// runs beside one that is not parallel.
	const folders, filesPerFolder = 1_000, 10
	tree := wideTree(folders, filesPerFolder)
	transfers := newGatedFiles(folders * filesPerFolder)
	ctx, cancel := context.WithCancel(t.Context())

	run := startTreeDownload(ctx, tree, transfers, t.TempDir())
	transfers.waitWorkersBusy(t)
	awaitClosed(t, run.reported, "the collector delivered no event")
	assert.Equal(t, TreeWorkers+1, treePoolGoroutines(), "the workers and the collector, whatever the tree holds")
	cancel()
	run.wait(t)

	assert.Zero(t, treePoolGoroutines(), "every goroutine of the transfer has ended when it returns")
}

// assertFailedAreAPrefixOfTheListing holds the files a download canceled in
// the folder of listing to the first of that listing, in listing order: the
// ones it had started, the ones it had queued (the queue is first in, first
// out, so a queued file left out would leave a gap) and the one the walk was
// queueing when the cancel came.
func assertFailedAreAPrefixOfTheListing(t *testing.T, listing []graph.Item, failed []TreeEvent) {
	t.Helper()

	position := make(map[string]int, len(listing))
	for i := range listing {
		position[treeRelPath(listing[i].ID)] = i
	}
	reported := relPaths(failed)
	for _, rel := range reported {
		require.Contains(t, position, rel, "%s is in the first folder", rel)
	}
	slices.SortFunc(reported, func(a, b string) int { return position[a] - position[b] })

	require.GreaterOrEqual(t, len(reported), TreeWorkers+1, "the started files and the one being queued")
	require.LessOrEqual(t, len(reported), 2*TreeWorkers+1, "no more than the workers, the queue and one")
	for i, rel := range reported {
		assert.Equal(t, treeRelPath(listing[i].ID), rel, "reported file %d is the listing's", i)
	}
}

// A canceled download reports every file it queued and did not finish as
// failed with the context's error, and every folder whose walk it cut short;
// what it never listed is covered by those folders.
//
// Validates: R-6.3.6, R-1.2.1
func TestDownloadTree_CancellationReportsEveryUnstartedFile(t *testing.T) {
	t.Parallel()

	const folders, filesPerFolder = 10, 20
	tree := wideTree(folders, filesPerFolder)
	transfers := newGatedFiles(folders * filesPerFolder)
	ctx, cancel := context.WithCancel(t.Context())

	run := startTreeDownload(ctx, tree, transfers, t.TempDir())
	transfers.waitWorkersBusy(t)
	cancel()
	run.wait(t)

	require.ErrorIs(t, run.err, context.Canceled)
	assert.Empty(t, eventsOf(run.events, TreeFileDone))
	failed := eventsOf(run.events, TreeFileFailed)
	assertFailedAreAPrefixOfTheListing(t, tree.filesOf(treeChildID(treeRootID, "folder-0000")), failed)
	failedPaths := map[string]bool{}
	for _, file := range failed {
		require.ErrorIs(t, file.Err, context.Canceled)
		assert.True(t, strings.HasPrefix(file.Err.Error(), file.LocalPath+": "), "%q names its file", file.Err)
		failedPaths[file.LocalPath] = true
	}
	for _, started := range transfers.transferred() {
		assert.True(t, failedPaths[started], "%s was started and is reported", started)
	}
	cutShort := eventsOf(run.events, TreeFolderFailed)
	require.NotEmpty(t, cutShort)
	for _, folder := range cutShort {
		require.ErrorIs(t, folder.Err, context.Canceled)
	}
	assert.Empty(t, cutShort[len(cutShort)-1].RelPath, "the root's walk was cut short too")
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// A remote name that is not one plain local name is refused with the reason:
// nothing is written for it, a refused folder is not listed, and its siblings
// download.
//
// Validates: R-1.2.7
func TestDownloadTree_RefusesUnsafeRemoteNames(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	refusedFiles := map[string]string{
		"..": nameIsDotOrDotDot, "a/b": nameHoldsSeparator, "": nameIsEmpty, ".": nameIsDotOrDotDot, `a\b`: nameHoldsSeparator,
	}
	for name := range refusedFiles {
		tree.addFile(treeRootID, name, 1)
	}
	escape := tree.addFolder(treeRootID, "..")
	tree.addFile(escape, "planted.txt", 1)
	tree.addFile(treeRootID, "kept.txt", 1)
	tree.addFile(tree.addFolder(treeRootID, "Sub"), "inner.txt", 1)
	transfers := newGatedFiles(len(refusedFiles) + 2)
	localRoot := filepath.Join(t.TempDir(), "target")

	run := downloadTreeNow(t, tree, transfers, localRoot)

	require.NoError(t, run.err)
	refusedNames := map[string]string{}
	for _, failure := range append(eventsOf(run.events, TreeFileFailed), eventsOf(run.events, TreeFolderFailed)...) {
		var refused *RefusedNameError
		require.ErrorAs(t, failure.Err, &refused)
		assert.Equal(t, localRoot, failure.LocalPath, "a refused name is reported in the folder it would have gone into")
		refusedNames[fmt.Sprintf("%d:%s", failure.Kind, refused.Name)] = refused.Reason
	}
	want := map[string]string{fmt.Sprintf("%d:..", TreeFolderFailed): nameIsDotOrDotDot}
	for name, reason := range refusedFiles {
		want[fmt.Sprintf("%d:%s", TreeFileFailed, name)] = reason
	}
	assert.Equal(t, want, refusedNames)
	assert.ElementsMatch(t, []string{filepath.Join(localRoot, "kept.txt"), filepath.Join(localRoot, "Sub", "inner.txt")},
		transfers.transferred(), "nothing is written for a refused name")
	assert.NotContains(t, tree.listedFolders(), escape, "a refused folder is not walked")
	assertDirHolds(t, filepath.Dir(localRoot), "target")
	assertDirHolds(t, localRoot, "Sub")
	assertSummaryCountsEvents(t, run)
}

// assertDirHolds holds the local folder dir to exactly names.
func assertDirHolds(t *testing.T, dir string, names ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	held := make([]string, 0, len(entries))
	for _, entry := range entries {
		held = append(held, entry.Name())
	}
	assert.ElementsMatch(t, names, held, "the entries of %s", dir)
}

// A folder that cannot be listed is one failure naming it; its siblings and
// the rest of the tree download.
//
// Validates: R-1.2.1
func TestDownloadTree_ListFailureIsReportedAndSiblingsContinue(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	bad := tree.addFolder(treeRootID, "Bad")
	tree.addFile(bad, "never.txt", 1)
	tree.failListing(bad, errTreeTestRefused)
	tree.addFile(tree.addFolder(treeRootID, "Good"), "g.txt", 2)
	tree.addFile(treeRootID, "top.txt", 3)
	localRoot := t.TempDir()

	run := downloadTreeNow(t, tree, newGatedFiles(2), localRoot)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "Bad")
	assert.Equal(t, TreeFolderFailed, failure.Kind)
	require.ErrorIs(t, failure.Err, errTreeTestRefused)
	assert.Equal(t, `listing the OneDrive folder "`+treeRemoteRoot+`/Bad" to fill `+filepath.Join(localRoot, "Bad")+
		": "+errTreeTestRefused.Error(), failure.Err.Error())
	assert.NoDirExists(t, filepath.Join(localRoot, "Bad"), "a folder that could not be listed is not created")
	assert.Equal(t, TreeFileDone, eventFor(t, run.events, "Good/g.txt").Kind)
	assert.Equal(t, TreeFileDone, eventFor(t, run.events, "top.txt").Kind)
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// A file that fails to download is one failure naming it; the other files
// download.
//
// Validates: R-1.2.1
func TestDownloadTree_TransferFailureIsReportedAndOthersContinue(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		tree.addFile(treeRootID, name, 5)
	}
	localRoot := t.TempDir()
	transfers := newGatedFiles(3)
	transfers.failTransfer(filepath.Join(localRoot, "b.txt"), errTreeTestRefused)

	run := downloadTreeNow(t, tree, transfers, localRoot)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "b.txt")
	assert.Equal(t, TreeFileFailed, failure.Kind)
	require.ErrorIs(t, failure.Err, errTreeTestRefused)
	assert.Equal(t, filepath.Join(localRoot, "b.txt")+": "+errTreeTestRefused.Error(), failure.Err.Error())
	for _, name := range []string{"a.txt", "c.txt"} {
		done := eventFor(t, run.events, name)
		assert.Equal(t, TreeFileDone, done.Kind)
		assert.Equal(t, int64(5), done.Size)
		assert.True(t, done.HashVerified)
		assert.Equal(t, filepath.Join(localRoot, name), done.LocalPath)
	}
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// A root that cannot be listed is the returned error, before anything is
// reported or created.
//
// Validates: R-1.2.1
func TestDownloadTree_UnlistableRootIsTheReturnedError(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.failListing(treeRootID, errTreeTestRefused)
	localRoot := filepath.Join(t.TempDir(), "target")

	run := downloadTreeNow(t, tree, newGatedFiles(0), localRoot)

	var rootErr *TreeRootError
	require.ErrorAs(t, run.err, &rootErr)
	require.ErrorIs(t, run.err, errTreeTestRefused)
	assert.Empty(t, run.events)
	assert.Equal(t, TreeSummary{}, run.summary)
	assert.NoDirExists(t, localRoot)
}

// The walk is done with a folder's page, the whole walk of a subfolder on it
// included, before it asks for the folder's next page: it holds one page per
// level of depth, never a whole listing.
//
// Validates: R-1.2.1, R-1.1.3
func TestDownloadTree_WalksAPageBeforeAskingForTheNext(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.pageSize = 2
	sub := tree.addFolder(treeRootID, "Sub")
	tree.addFile(sub, "s.txt", 1)
	tree.addFile(treeRootID, "a.txt", 1)
	tree.addFile(treeRootID, "b.txt", 1)

	run := downloadTreeNow(t, tree, newGatedFiles(3), t.TempDir())

	require.NoError(t, run.err)
	assert.Equal(t, []string{
		fakePage{folderID: treeRootID, page: 1}.String(),
		fakePage{folderID: sub, page: 1}.String(),
		fakePage{folderID: treeRootID, page: 2}.String(),
	}, tree.askedPages())
	assert.Equal(t, 3, run.summary.Files)
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// A page that fails after a folder's first one is that folder's failure: what
// the pages before it held downloads, its siblings go on, and for the root it
// is a failure of the root folder, not the run's error.
//
// Validates: R-1.2.1
func TestDownloadTree_AFailedLaterPageFailsOnlyItsFolder(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.pageSize = 2
	sub := tree.addFolder(treeRootID, "Sub")
	for _, name := range []string{"s1.txt", "s2.txt", "s3.txt"} {
		tree.addFile(sub, name, 1)
	}
	tree.addFile(treeRootID, "a.txt", 1)
	tree.addFile(treeRootID, "never.txt", 1)
	tree.failPage(sub, 2, errTreeTestRefused)
	tree.failPage(treeRootID, 2, errTreeTestRefused)
	localRoot := t.TempDir()

	run := downloadTreeNow(t, tree, newGatedFiles(3), localRoot)

	require.NoError(t, run.err, "a later page of the root is not the run's error")
	for _, done := range []string{"Sub/s1.txt", "Sub/s2.txt", "a.txt"} {
		assert.Equal(t, TreeFileDone, eventFor(t, run.events, done).Kind, done)
	}
	failed := eventsOf(run.events, TreeFolderFailed)
	assert.Equal(t, []string{"Sub", ""}, relPaths(failed))
	require.Len(t, failed, 2)
	assert.Equal(t, `listing the OneDrive folder "`+treeRemoteRoot+`/Sub" to fill `+filepath.Join(localRoot, "Sub")+
		": "+errTreeTestRefused.Error(), failed[0].Err.Error())
	assert.Equal(t, `listing the OneDrive folder "`+treeRemoteRoot+`" to fill `+localRoot+
		": "+errTreeTestRefused.Error(), failed[1].Err.Error())
	assertSummaryCountsEvents(t, run)
}

// A download canceled while the walk waits for the queue asks for no further
// page: the walk stops in the page it was in.
//
// Validates: R-6.3.6, R-1.2.1
func TestDownloadTree_CancellationAsksForNoFurtherPage(t *testing.T) {
	t.Parallel()

	const files, pageSize = 6 * TreeWorkers, 2
	tree := newFakeTree()
	tree.pageSize = pageSize
	for i := range files {
		tree.addFile(treeRootID, fmt.Sprintf("file-%02d.bin", i), 1)
	}
	transfers := newGatedFiles(files)
	ctx, cancel := context.WithCancel(t.Context())

	run := startTreeDownload(ctx, tree, transfers, t.TempDir())
	transfers.waitWorkersBusy(t)
	cancel()
	run.wait(t)

	require.ErrorIs(t, run.err, context.Canceled)
	// The workers and the queue take 2×TreeWorkers files; the walk waits on
	// the first file of the page after them, and asks for nothing more.
	assert.LessOrEqual(t, len(tree.askedPages()), 2*TreeWorkers/pageSize+1, "pages asked for: %v", tree.askedPages())
	assertEveryListedFileAccounted(t, tree, run)
	assertSummaryCountsEvents(t, run)
}

// A later page that fails because the download was canceled is not a listing
// failure of its folder: the folder is reported as cut short, with the
// context's error, and the run ends with that error.
//
// Validates: R-6.3.6, R-1.2.1
func TestDownloadTree_CancellationAtALaterPageCutsItsFolderShort(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.pageSize = 1
	sub := tree.addFolder(treeRootID, "Sub")
	tree.addFile(treeRootID, "never.txt", 1)
	tree.failPage(treeRootID, 2, context.Canceled)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The download is canceled while the root's first page is walked, once
	// the walk has gone into Sub.
	tree.onList = func(folderID string) {
		if folderID == sub {
			cancel()
		}
	}
	localRoot := t.TempDir()
	transfers := newGatedFiles(1)
	transfers.openGate()

	run := startTreeDownload(ctx, tree, transfers, localRoot)
	run.wait(t)

	require.ErrorIs(t, run.err, context.Canceled)
	failed := eventsOf(run.events, TreeFolderFailed)
	require.Len(t, failed, 1)
	assert.Empty(t, failed[0].RelPath, "the root is the folder cut short")
	assert.Equal(t, localRoot+": stopped before the folder was finished: "+context.Canceled.Error(), failed[0].Err.Error())
	assertSummaryCountsEvents(t, run)
}

// Every folder exists locally before a file is downloaded into it: the walk
// creates it before it queues the folder's files.
//
// Validates: R-1.2.1
func TestDownloadTree_FolderExistsBeforeItsFilesDownload(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	deep := tree.addFolder(tree.addFolder(tree.addFolder(treeRootID, "a"), "b"), "c")
	tree.addFile(deep, "deep.txt", 1)
	tree.addFile(treeRootID, "top.txt", 1)
	var mu stdsync.Mutex // guards missing
	var missing []string
	files := downloadFunc(func(_ context.Context, targetPath string, _ DownloadOpts) (*DownloadResult, error) {
		if info, err := os.Stat(filepath.Dir(targetPath)); err != nil || !info.IsDir() {
			mu.Lock()
			missing = append(missing, targetPath)
			mu.Unlock()
		}
		return &DownloadResult{}, nil
	})

	run := startTreeDownload(t.Context(), tree, files, filepath.Join(t.TempDir(), "target"))
	run.wait(t)

	require.NoError(t, run.err)
	assert.Equal(t, 2, run.summary.Files)
	assert.Equal(t, 4, run.summary.Folders)
	assert.Empty(t, missing, "files downloaded before their folder existed")
}

// folderOnlyTree is a tree of folders folders under the root, without files.
func folderOnlyTree(folders int) *fakeTree {
	return wideTree(folders, 0)
}

// folders_created counts the folders the download made: a folder that was
// already there is used as it is, keeps its mode, and is not counted.
//
// Validates: R-1.2.4
func TestDownloadTree_CountsOnlyTheFoldersItCreates(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	existing := filepath.Join(localRoot, "folder-0000")
	require.NoError(t, os.Mkdir(existing, 0o700))

	run := downloadTreeNow(t, folderOnlyTree(2), newGatedFiles(0), localRoot)

	require.NoError(t, run.err)
	assert.Empty(t, eventsOf(run.events, TreeFolderFailed))
	assert.Equal(t, 1, run.summary.Folders, "only folder-0001 is new")
	assert.Equal(t, "folder-0001", eventsOf(run.events, TreeFolderCreated)[0].RelPath)
	assert.DirExists(t, filepath.Join(localRoot, "folder-0001"))
	info, err := os.Stat(existing)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "an existing folder keeps its mode")
}

// A file where the download needs a folder is reported once, naming the
// path, and nothing is walked below it.
//
// Validates: R-1.2.1
func TestDownloadTree_FileWhereAFolderBelongsIsReported(t *testing.T) {
	t.Parallel()

	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o600))

	run := downloadTreeNow(t, folderOnlyTree(1), newGatedFiles(0), target)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "")
	assert.Equal(t, TreeFolderFailed, failure.Kind)
	assert.Equal(t, "creating directory "+target+": it is not a folder", failure.Err.Error())
	assert.Equal(t, TreeSummary{Failed: 1}, run.summary)
}

// A folder the download creates asks for 0777 and gets the umask, like the
// files downloaded into it.
//
// Validates: R-5.10.2, R-1.2.1
func TestDownloadTree_CreatedFolderFollowsTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	target := filepath.Join(t.TempDir(), "target")

	run := downloadTreeNow(t, folderOnlyTree(1), newGatedFiles(0), target)

	require.NoError(t, run.err)
	assert.Empty(t, eventsOf(run.events, TreeFolderFailed))
	for _, dir := range []string{target, filepath.Join(target, "folder-0000")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "%s is 0777 minus the umask", dir)
	}
}

// A panic in one file's transfer is that file's failure, listed with the
// others, while the rest of the tree downloads.
//
// Validates: R-6.3.10
func TestDownloadTree_APanickingTransferFailsOnlyThatFile(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.addFile(treeRootID, "boom.txt", 1)
	tree.addFile(treeRootID, "fine.txt", 1)
	files := downloadFunc(func(_ context.Context, targetPath string, _ DownloadOpts) (*DownloadResult, error) {
		if filepath.Base(targetPath) == "boom.txt" {
			panic("transfer exploded")
		}
		return &DownloadResult{}, nil
	})

	run := startTreeDownload(t.Context(), tree, files, t.TempDir())
	run.wait(t)

	require.NoError(t, run.err)
	boom := eventFor(t, run.events, "boom.txt")
	assert.Equal(t, TreeFileFailed, boom.Kind)
	require.ErrorIs(t, boom.Err, panicguard.ErrPanic)
	assert.Equal(t, TreeFileDone, eventFor(t, run.events, "fine.txt").Kind)
}

// A panic in the walk itself fails the run with that panic once the files it
// queued are reported, instead of leaving the workers waiting on a queue
// nobody closes.
//
// Validates: R-6.3.10
func TestDownloadTree_APanickingWalkFailsTheRun(t *testing.T) {
	t.Parallel()

	tree := newFakeTree()
	tree.addFile(treeRootID, "first.txt", 1)
	broken := tree.addFolder(treeRootID, "Broken")
	tree.onList = func(folderID string) {
		if folderID == broken {
			panic("listing exploded")
		}
	}

	run := downloadTreeNow(t, tree, newGatedFiles(1), t.TempDir())

	require.ErrorIs(t, run.err, panicguard.ErrPanic)
	assert.Equal(t, TreeFileDone, eventFor(t, run.events, "first.txt").Kind)
}

// A panic while an event is reported fails the run with that panic: the
// transfer still finishes and counts every event, so no goroutine waits on a
// collector that stopped.
//
// Validates: R-6.3.10
func TestDownloadTree_APanickingReportFailsTheRun(t *testing.T) {
	t.Parallel()

	const files = 3 * TreeWorkers
	tree := newFakeTree()
	for i := range files {
		tree.addFile(treeRootID, fmt.Sprintf("file-%02d.bin", i), 1)
	}
	transfers := newGatedFiles(files)
	transfers.openGate()

	summary, err := DownloadTree(t.Context(), TreeDownload{
		Lister: tree, Files: transfers, DriveID: driveid.New("0000000000000001"), FolderID: treeRootID,
		LocalRoot: t.TempDir(), Logger: slog.New(slog.DiscardHandler),
		OnEvent: func(TreeEvent) { panic("report exploded") },
	})

	require.ErrorIs(t, err, panicguard.ErrPanic)
	assert.Equal(t, files, summary.Files, "every event is counted after the report stopped")
}
