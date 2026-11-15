package driveops

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	stdsync "sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// uploadParentID is the remote folder every test upload goes into, under
// the name treeRootID; it exists from the start. uploadRootID is the remote
// folder the upload makes there.
const (
	uploadParentID = "parent"
	uploadRootID   = uploadParentID + "/" + treeRootID
)

// fakeFolders stands in for OneDrive's folder creation. A folder's ID is its
// parent's ID, "/" and its name; uploadParentID exists from the start.
type fakeFolders struct {
	// mu guards ensured, orphans and failures.
	mu       stdsync.Mutex
	ensured  map[string]int
	orphans  []string
	failures map[string]error
	// onEnsure, when set, runs on every call, before anything else.
	onEnsure func(parentID, name string)
}

func newFakeFolders() *fakeFolders {
	return &fakeFolders{ensured: map[string]int{uploadParentID: 1}, failures: map[string]error{}}
}

// failFolder makes the creation of every folder named name fail with err.
func (f *fakeFolders) failFolder(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures[name] = err
}

func (f *fakeFolders) EnsureFolder(_ context.Context, parentID, name string) (*graph.Item, error) {
	if f.onEnsure != nil {
		f.onEnsure(parentID, name)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.failures[name]; err != nil {
		return nil, err
	}
	if f.ensured[parentID] == 0 {
		f.orphans = append(f.orphans, treeChildID(parentID, name))
	}
	id := treeChildID(parentID, name)
	f.ensured[id]++

	return &graph.Item{ID: id, Name: name, IsFolder: true}, nil
}

// exists reports whether the folder id was made, or is uploadParentID.
func (f *fakeFolders) exists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ensured[id] > 0
}

// made is every folder made so far, by ID, with how often.
func (f *fakeFolders) made() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return maps.Clone(f.ensured)
}

// writeLocalTree writes each of files, a "/"-separated path below root, as a
// one-byte file, creating its folders.
func writeLocalTree(t *testing.T, root string, files ...string) {
	t.Helper()

	for _, file := range files {
		local := filepath.Join(root, filepath.FromSlash(file))
		require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o700))
		require.NoError(t, os.WriteFile(local, []byte("x"), 0o600))
	}
}

// startTreeUpload uploads localRoot's content into the remote root through
// folders and put.
func startTreeUpload(
	ctx context.Context, folders FolderEnsurer,
	put func(ctx context.Context, parentID, name, localPath string) (*UploadResult, error), localRoot string,
) *treeRun {
	return startUpload(ctx, TreeUpload{Folders: folders, PutFile: put, LocalRoot: localRoot})
}

// startUpload runs u, into treeRootID under uploadParentID, on a goroutine of
// its own.
func startUpload(ctx context.Context, u TreeUpload) *treeRun {
	run := newTreeRun()
	u.ParentID, u.Name = uploadParentID, treeRootID
	u.Logger = slog.New(slog.DiscardHandler)
	u.OnEvent = run.record
	go func() {
		defer close(run.done)
		run.summary, run.err = UploadTree(ctx, u)
	}()

	return run
}

// relPaths is the RelPath of every event in events.
func relPaths(events []TreeEvent) []string {
	paths := make([]string, 0, len(events))
	for i := range events {
		paths = append(paths, events[i].RelPath)
	}

	return paths
}

// Every remote folder exists before a file is uploaded into it, and before
// any folder below it is created: the walk makes sure of a folder before it
// reads the folder.
//
// Validates: R-1.3.1
func TestUploadTree_CreatesParentBeforeChildren(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "a/b/c/deep.txt", "a/b/y.txt", "a/x.txt", "top.txt")
	folders := newFakeFolders()
	transfers := newGatedFiles(4)
	transfers.openGate()
	var mu stdsync.Mutex // guards early
	var early []string
	put := func(ctx context.Context, parentID, name, localPath string) (*UploadResult, error) {
		if !folders.exists(parentID) {
			mu.Lock()
			early = append(early, localPath)
			mu.Unlock()
		}
		return transfers.putFile(ctx, parentID, name, localPath)
	}

	run := startTreeUpload(t.Context(), folders, put, localRoot)
	run.wait(t)

	require.NoError(t, run.err)
	assert.Empty(t, early, "files uploaded before their folder existed")
	assert.Empty(t, folders.orphans, "folders created before their parent")
	assert.Equal(t, map[string]int{
		uploadParentID: 1, uploadRootID: 1, uploadRootID + "/a": 1, uploadRootID + "/a/b": 1, uploadRootID + "/a/b/c": 1,
	}, folders.made())
	assert.ElementsMatch(t, []string{"", "a", "a/b", "a/b/c"}, relPaths(eventsOf(run.events, TreeFolderCreated)))
	assert.Equal(t, TreeEvent{Kind: TreeFolderCreated, LocalPath: localRoot, ItemID: uploadRootID}, run.events[0],
		"the remote root is the first event")
	assert.ElementsMatch(t, []string{"a/b/c/deep.txt", "a/b/y.txt", "a/x.txt", "top.txt"}, relPaths(eventsOf(run.events, TreeFileDone)))
	deep := eventFor(t, run.events, "a/b/c/deep.txt")
	assert.Equal(t, uploadRootID+"/a/b/c/deep.txt", deep.ItemID)
	assert.Equal(t, filepath.Join(localRoot, "a", "b", "c", "deep.txt"), deep.LocalPath)
	assertSummaryCountsEvents(t, run)
}

// A folder of many files runs exactly TreeWorkers uploads at once.
//
// Validates: R-1.3.1
func TestUploadTree_InFlightNeverExceedsWorkers(t *testing.T) {
	t.Parallel()

	const files = 10 * TreeWorkers
	localRoot := t.TempDir()
	names := make([]string, files)
	for i := range files {
		names[i] = fmt.Sprintf("file-%03d.txt", i)
	}
	writeLocalTree(t, localRoot, names...)
	transfers := newGatedFiles(files)

	run := startTreeUpload(t.Context(), newFakeFolders(), transfers.putFile, localRoot)
	transfers.waitWorkersBusy(t)
	transfers.openGate()
	run.wait(t)

	require.NoError(t, run.err)
	assert.Equal(t, int64(TreeWorkers), transfers.peak.Load(), "uploads running at once")
	assert.ElementsMatch(t, names, relPaths(eventsOf(run.events, TreeFileDone)))
	assertSummaryCountsEvents(t, run)
}

// A folder with more entries than one batch read uploads every one of them.
//
// Validates: R-1.3.1
func TestUploadTree_UploadsAFolderLargerThanOneBatch(t *testing.T) {
	t.Parallel()

	const files = uploadDirBatch + 1
	localRoot := t.TempDir()
	names := make([]string, files)
	for i := range files {
		names[i] = fmt.Sprintf("file-%03d.txt", i)
	}
	writeLocalTree(t, localRoot, names...)
	transfers := newGatedFiles(files)
	transfers.openGate()

	run := startTreeUpload(t.Context(), newFakeFolders(), transfers.putFile, localRoot)
	run.wait(t)

	require.NoError(t, run.err)
	assert.ElementsMatch(t, names, relPaths(eventsOf(run.events, TreeFileDone)))
}

// A folder that cannot be read is one failure naming it, and is not created
// on OneDrive; its siblings upload.
//
// Validates: R-1.3.1
func TestUploadTree_ReadDirFailureReported(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the superuser ignores folder modes")
	}

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "locked/hidden.txt", "open/seen.txt", "top.txt")
	locked := filepath.Join(localRoot, "locked")
	setTestDirPermissions(t, locked, 0o000)
	t.Cleanup(func() { setTestDirPermissions(t, locked, 0o700) })
	folders := newFakeFolders()
	transfers := newGatedFiles(3)
	transfers.openGate()

	run := startTreeUpload(t.Context(), folders, transfers.putFile, localRoot)
	run.wait(t)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "locked")
	assert.Equal(t, TreeFolderFailed, failure.Kind)
	require.ErrorIs(t, failure.Err, os.ErrPermission)
	assert.Contains(t, failure.Err.Error(), locked)
	assert.False(t, folders.exists(uploadRootID+"/locked"), "a folder that cannot be read is not created on OneDrive")
	assert.ElementsMatch(t, []string{"open/seen.txt", "top.txt"}, relPaths(eventsOf(run.events, TreeFileDone)))
	assertSummaryCountsEvents(t, run)
}

// A folder that cannot be created on OneDrive is one failure naming it, and
// nothing below it is uploaded; its siblings upload.
//
// Validates: R-1.3.1
func TestUploadTree_FolderThatCannotBeCreatedIsReported(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "Bad/inner.txt", "Good/inner.txt")
	folders := newFakeFolders()
	folders.failFolder("Bad", errTreeTestRefused)
	transfers := newGatedFiles(2)
	transfers.openGate()

	run := startTreeUpload(t.Context(), folders, transfers.putFile, localRoot)
	run.wait(t)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "Bad")
	assert.Equal(t, TreeFolderFailed, failure.Kind)
	require.ErrorIs(t, failure.Err, errTreeTestRefused)
	assert.Equal(t, filepath.Join(localRoot, "Bad")+": "+errTreeTestRefused.Error(), failure.Err.Error())
	assert.Equal(t, []string{"Good/inner.txt"}, relPaths(eventsOf(run.events, TreeFileDone)))
	assertSummaryCountsEvents(t, run)
}

// A file that fails to upload is one failure naming it and the cause; the
// other files upload.
//
// Validates: R-1.3.1
func TestUploadTree_TransferFailureIsReportedAndOthersContinue(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "a.txt", "b.txt", "c.txt")
	transfers := newGatedFiles(3)
	transfers.failTransfer(filepath.Join(localRoot, "b.txt"), errTreeTestRefused)
	transfers.openGate()

	run := startTreeUpload(t.Context(), newFakeFolders(), transfers.putFile, localRoot)
	run.wait(t)

	require.NoError(t, run.err)
	failure := eventFor(t, run.events, "b.txt")
	assert.Equal(t, TreeFileFailed, failure.Kind)
	require.ErrorIs(t, failure.Err, errTreeTestRefused)
	assert.Equal(t, filepath.Join(localRoot, "b.txt")+": "+errTreeTestRefused.Error(), failure.Err.Error())
	assert.Empty(t, failure.ItemID)
	for _, name := range []string{"a.txt", "c.txt"} {
		done := eventFor(t, run.events, name)
		assert.Equal(t, TreeFileDone, done.Kind)
		assert.Equal(t, uploadRootID+"/"+name, done.ItemID)
	}
	assertSummaryCountsEvents(t, run)
}

// failingRead is the read of a local folder that hands out one entry, then
// fails with errTreeTestRefused; every other folder reads as usual, sorted by
// name so the walk visits them in a known order.
func failingRead(failing string) func(dir *localpath.DirReader, localPath string) ([]os.DirEntry, error) {
	reads := 0 // the walk reads on one goroutine
	return func(dir *localpath.DirReader, localPath string) ([]os.DirEntry, error) {
		if localPath != failing {
			entries, err := dir.Next(uploadDirBatch)
			slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			return entries, err //nolint:wrapcheck // the walk reads localpath's own error, as it does in production
		}
		reads++
		if reads == 1 {
			return dir.Next(1)
		}
		return nil, fmt.Errorf("reading directory %s: %w", localPath, errTreeTestRefused)
	}
}

// A folder whose read fails partway through uploads what was read before the
// failure, is reported once with the cause, and the walk goes on with the
// next folder.
//
// Validates: R-1.3.1
func TestUploadTree_ReadFailurePartwayThroughAFolderIsReported(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "Broken/a.txt", "Broken/b.txt", "Broken/c.txt", "Next/n.txt")
	broken := filepath.Join(localRoot, "Broken")
	transfers := newGatedFiles(4)
	transfers.openGate()

	run := startUpload(t.Context(), TreeUpload{
		Folders: newFakeFolders(), PutFile: transfers.putFile, LocalRoot: localRoot, readBatch: failingRead(broken),
	})
	run.wait(t)

	require.NoError(t, run.err)
	failures := eventsOf(run.events, TreeFolderFailed)
	require.Len(t, failures, 1, "the folder is reported once")
	assert.Equal(t, "Broken", failures[0].RelPath)
	require.ErrorIs(t, failures[0].Err, errTreeTestRefused)
	assert.Contains(t, failures[0].Err.Error(), broken)
	done := relPaths(eventsOf(run.events, TreeFileDone))
	slices.Sort(done)
	require.Len(t, done, 2, "the entry read before the failure, and the next folder's file")
	assert.True(t, strings.HasPrefix(done[0], "Broken/"), "%s was read before the failure", done[0])
	assert.Equal(t, "Next/n.txt", done[1])
	assertSummaryCountsEvents(t, run)
}

// assertEveryLocalFileAccounted is the rule that nothing is dropped without a
// word, for an upload of files, each "/"-separated below the root: each file
// of a folder the walk made sure of has exactly one event, transferred or
// failed, and a file of a folder it never reached has none, while a folder
// above it is reported as cut short.
func assertEveryLocalFileAccounted(t *testing.T, run *treeRun, files []string) {
	t.Helper()

	reached, cutShort, fileEvents := map[string]bool{}, map[string]bool{}, map[string]int{}
	for i := range run.events {
		switch run.events[i].Kind {
		case TreeFolderCreated:
			reached[run.events[i].RelPath] = true
		case TreeFolderFailed:
			cutShort[run.events[i].RelPath] = true
		case TreeFileDone, TreeFileFailed:
			fileEvents[run.events[i].RelPath]++
		}
	}

	for _, file := range files {
		folder := strings.TrimSuffix(path.Dir(file), ".")
		if reached[folder] {
			assert.Equal(t, 1, fileEvents[file], "%s, in a folder the walk reached, has one event", file)
			continue
		}
		assert.Zero(t, fileEvents[file], "%s, in a folder the walk never reached, has no event", file)
		covered := false
		for above := folder; !covered; above = strings.TrimSuffix(path.Dir(above), ".") {
			covered = cutShort[above]
			if above == "" {
				break
			}
		}
		assert.True(t, covered, "%s is covered by a folder reported as cut short", file)
	}
}

// A canceled upload accounts for every file of the tree. With one file per
// folder and the walk stopped at the file that finds every worker busy and
// the queue full, each file of a folder the walk reached is reported once,
// failed with the context's error, and the files of every folder it did not
// reach are covered by the root, reported as cut short.
//
// Validates: R-6.3.6, R-1.3.1
func TestUploadTree_CancellationReportsEveryUnstartedFile(t *testing.T) {
	t.Parallel()

	const folders = 20
	// The walk waits at the file of folder 2×TreeWorkers+1: TreeWorkers
	// files are transferring and TreeWorkers more fill the queue.
	const walkWaitsAt = 2*TreeWorkers + 1
	localRoot := t.TempDir()
	files := make([]string, folders)
	for i := range folders {
		files[i] = fmt.Sprintf("folder-%02d/file.txt", i)
	}
	writeLocalTree(t, localRoot, files...)
	remote := newFakeFolders()
	reached := make(chan struct{})
	subfolders := 0 // counted on the walk's goroutine alone
	remote.onEnsure = func(parentID, _ string) {
		if parentID != uploadRootID {
			return
		}
		subfolders++
		if subfolders == walkWaitsAt {
			close(reached)
		}
	}
	transfers := newGatedFiles(folders)
	ctx, cancel := context.WithCancel(t.Context())

	run := startTreeUpload(ctx, remote, transfers.putFile, localRoot)
	transfers.waitWorkersBusy(t)
	awaitClosed(t, reached, "the walk did not reach the folder of the file the full queue holds back")
	cancel()
	run.wait(t)

	require.ErrorIs(t, run.err, context.Canceled)
	assert.Empty(t, eventsOf(run.events, TreeFileDone))
	failed := eventsOf(run.events, TreeFileFailed)
	assert.Len(t, failed, walkWaitsAt, "one file in each folder the walk reached")
	for _, file := range failed {
		require.ErrorIs(t, file.Err, context.Canceled)
		assert.True(t, strings.HasPrefix(file.Err.Error(), file.LocalPath+": "), "%q names its file", file.Err)
	}
	assertEveryLocalFileAccounted(t, run, files)
	cutShort := eventsOf(run.events, TreeFolderFailed)
	require.NotEmpty(t, cutShort)
	for _, folder := range cutShort {
		require.ErrorIs(t, folder.Err, context.Canceled)
	}
	assert.Empty(t, cutShort[len(cutShort)-1].RelPath, "the root's walk was cut short too")
	assertSummaryCountsEvents(t, run)
}

// A local root that cannot be opened, missing or a file, is the returned
// error, naming it, before anything is reported, and nothing is created on
// OneDrive: the root is opened before the remote one is made.
//
// Validates: R-1.3.1
func TestUploadTree_UnopenableRootCreatesNothingOnOneDrive(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file.txt")
	writeLocalTree(t, filepath.Dir(file), "file.txt")
	for root, cause := range map[string]error{
		filepath.Join(t.TempDir(), "missing"): os.ErrNotExist,
		file:                                  syscall.ENOTDIR,
	} {
		remote := newFakeFolders()
		run := startTreeUpload(t.Context(), remote, newGatedFiles(0).putFile, root)
		run.wait(t)

		var rootErr *TreeRootError
		require.ErrorAs(t, run.err, &rootErr)
		require.ErrorIs(t, run.err, cause)
		require.ErrorContains(t, run.err, root)
		assert.Empty(t, run.events)
		assert.Equal(t, map[string]int{uploadParentID: 1}, remote.made(), "nothing is created on OneDrive for %s", root)
	}
}

// A remote root that cannot be made is the returned error, before anything is
// reported or uploaded.
//
// Validates: R-1.3.1
func TestUploadTree_RootThatCannotBeCreatedIsTheReturnedError(t *testing.T) {
	t.Parallel()

	localRoot := t.TempDir()
	writeLocalTree(t, localRoot, "a.txt")
	remote := newFakeFolders()
	remote.failFolder(treeRootID, errTreeTestRefused)
	transfers := newGatedFiles(1)

	run := startTreeUpload(t.Context(), remote, transfers.putFile, localRoot)
	run.wait(t)

	var rootErr *TreeRootError
	require.ErrorAs(t, run.err, &rootErr)
	require.ErrorIs(t, run.err, errTreeTestRefused)
	assert.Empty(t, run.events)
	assert.Empty(t, transfers.transferred())
}
