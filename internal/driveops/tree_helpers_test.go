package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// treeTestGuard bounds every wait of a tree test, so a pool that never
// starts or never ends fails its test instead of hanging the suite.
const treeTestGuard = time.Minute

// treeRootID is the remote root folder of every generated tree, and
// treeRemoteRoot the path a download names it by.
const (
	treeRootID     = "root"
	treeRemoteRoot = "/Tree"
)

// errTreeTestRefused is the failure a test injects into a listing, a
// transfer or a folder creation.
var errTreeTestRefused = errors.New("refused by the test")

// fakeTree is a remote tree generated in memory and listed by folder ID a
// page at a time, the way Graph lists it. A child's ID is its parent's ID,
// "/" and its name, as it is (a name the walk refuses is not cleaned), so a
// test can name any item without bookkeeping.
type fakeTree struct {
	// pageSize is the number of children on a page; 0 puts a folder on one
	// page. Set before the tree is listed.
	pageSize int
	// mu guards folders, failures, listed and pagesAsked.
	mu       stdsync.Mutex
	folders  map[string][]graph.Item
	failures map[fakePage]error
	// listed is every folder ID whose first page was asked for, in order.
	listed []string
	// pagesAsked is every page asked for, as fakePage.String, in order.
	pagesAsked []string
	// onList, when set, runs before a folder's first page is asked for,
	// outside mu.
	onList func(folderID string)
}

// fakePage names one page of a folder's listing, the first page 1.
type fakePage struct {
	folderID string
	page     int
}

func (p fakePage) String() string { return fmt.Sprintf("%s#%d", p.folderID, p.page) }

func newFakeTree() *fakeTree {
	return &fakeTree{
		folders:  map[string][]graph.Item{treeRootID: nil},
		failures: map[fakePage]error{},
	}
}

// addFolder adds a folder named name to parentID and returns its ID.
func (f *fakeTree) addFolder(parentID, name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	id := treeChildID(parentID, name)
	f.folders[parentID] = append(f.folders[parentID], graph.Item{ID: id, Name: name, IsFolder: true})
	f.folders[id] = nil

	return id
}

// addFile adds a file of size bytes named name to parentID.
func (f *fakeTree) addFile(parentID, name string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.folders[parentID] = append(f.folders[parentID], graph.Item{ID: treeChildID(parentID, name), Name: name, Size: size})
}

// treeChildID is the ID of the child name of the folder parentID.
func treeChildID(parentID, name string) string { return parentID + "/" + name }

// treeRelPath is the path below the root of the item whose ID is id.
func treeRelPath(id string) string {
	return strings.TrimPrefix(strings.TrimPrefix(id, treeRootID), "/")
}

// failListing makes the listing of folderID fail with err at its first page.
func (f *fakeTree) failListing(folderID string, err error) {
	f.failPage(folderID, 1, err)
}

// failPage makes the listing of folderID fail with err when page is asked for.
func (f *fakeTree) failPage(folderID string, page int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures[fakePage{folderID: folderID, page: page}] = err
}

// ListChildrenPages hands parentID's children to visit a page at a time, as
// Graph does, and asks for no page past one visit refuses.
func (f *fakeTree) ListChildrenPages(_ context.Context, _ driveid.ID, parentID string, visit graph.ChildrenPageFunc) error {
	if f.onList != nil {
		f.onList(parentID)
	}

	for page := 1; ; page++ {
		children, last, err := f.listPage(fakePage{folderID: parentID, page: page})
		if err != nil {
			return err
		}
		if !visit(children) || last {
			return nil
		}
	}
}

// listPage is the page of a folder's children asked for, and whether it is
// the folder's last page.
func (f *fakeTree) listPage(asked fakePage) ([]graph.Item, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if asked.page == 1 {
		f.listed = append(f.listed, asked.folderID)
	}
	f.pagesAsked = append(f.pagesAsked, asked.String())
	if err := f.failures[asked]; err != nil {
		return nil, false, err
	}
	children, ok := f.folders[asked.folderID]
	if !ok {
		return nil, false, graph.ErrNotFound
	}
	if f.pageSize == 0 {
		return slices.Clone(children), true, nil
	}

	start := min((asked.page-1)*f.pageSize, len(children))
	end := min(start+f.pageSize, len(children))

	return slices.Clone(children[start:end]), end == len(children), nil
}

// listedFolders is every folder ID listed so far, in order.
func (f *fakeTree) listedFolders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.listed)
}

// askedPages is every page asked for so far, in order.
func (f *fakeTree) askedPages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.pagesAsked)
}

// filesOf is the files directly in folderID.
func (f *fakeTree) filesOf(folderID string) []graph.Item {
	f.mu.Lock()
	defer f.mu.Unlock()

	var files []graph.Item
	for i := range f.folders[folderID] {
		if !f.folders[folderID][i].IsFolder {
			files = append(files, f.folders[folderID][i])
		}
	}

	return files
}

// gatedFiles stands in for every file transfer of a tree: a transfer waits
// for the gate to open, or for its context to end, and the largest number
// running at once is recorded. It writes nothing.
type gatedFiles struct {
	gate    chan struct{}
	started chan struct{}
	running atomic.Int64
	peak    atomic.Int64
	// mu guards failures and calls.
	mu       stdsync.Mutex
	failures map[string]error
	calls    []string
}

// newGatedFiles makes the transfers of a tree of up to capacity files, gate
// closed.
func newGatedFiles(capacity int) *gatedFiles {
	return &gatedFiles{
		gate:     make(chan struct{}),
		started:  make(chan struct{}, capacity),
		failures: map[string]error{},
	}
}

// openGate lets every transfer, waiting or still to come, through.
func (g *gatedFiles) openGate() { close(g.gate) }

// failTransfer makes the transfer of localPath fail with err once the gate
// lets it through.
func (g *gatedFiles) failTransfer(localPath string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.failures[localPath] = err
}

// transfer is the transfer of the file at localPath.
func (g *gatedFiles) transfer(ctx context.Context, localPath string) error {
	defer g.running.Add(-1)
	raisePeak(&g.peak, g.running.Add(1))

	g.mu.Lock()
	g.calls = append(g.calls, localPath)
	failure := g.failures[localPath]
	g.mu.Unlock()
	g.started <- struct{}{}

	select {
	case <-g.gate:
		return failure
	case <-ctx.Done():
		return fmt.Errorf("transfer stopped: %w", ctx.Err())
	}
}

// raisePeak records running in peak when it is the most seen yet.
func raisePeak(peak *atomic.Int64, running int64) {
	for {
		seen := peak.Load()
		if running <= seen || peak.CompareAndSwap(seen, running) {
			return
		}
	}
}

// DownloadToFile makes gatedFiles the FileDownloader of a tree download.
func (g *gatedFiles) DownloadToFile(
	ctx context.Context, _ driveid.ID, _, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	if err := g.transfer(ctx, targetPath); err != nil {
		return nil, err
	}

	return &DownloadResult{Size: opts.RemoteSize, HashVerified: true}, nil
}

// putFile is gatedFiles as the PutFile of a tree upload: the uploaded item's
// ID is its parent's ID and its name.
func (g *gatedFiles) putFile(ctx context.Context, parentID, name, localPath string) (*UploadResult, error) {
	if err := g.transfer(ctx, localPath); err != nil {
		return nil, err
	}

	return &UploadResult{Item: &graph.Item{ID: treeChildID(parentID, name)}, Size: int64(len(name))}, nil
}

// waitWorkersBusy waits until TreeWorkers transfers have started: with the
// gate closed, every worker is then busy.
func (g *gatedFiles) waitWorkersBusy(t *testing.T) {
	t.Helper()

	g.waitStarted(t, TreeWorkers)
}

// waitStarted waits until n transfers have started.
func (g *gatedFiles) waitStarted(t *testing.T, n int) {
	t.Helper()

	for i := range n {
		select {
		case <-g.started:
		case <-time.After(treeTestGuard):
			require.FailNow(t, "transfers did not start", "%d of %d started", i, n)
		}
	}
}

// transferred is every local path a transfer was started for.
func (g *gatedFiles) transferred() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.calls)
}

// downloadFunc is a FileDownloader made of one function, for a test whose
// transfers do something of their own.
type downloadFunc func(ctx context.Context, targetPath string, opts DownloadOpts) (*DownloadResult, error)

func (f downloadFunc) DownloadToFile(
	ctx context.Context, _ driveid.ID, _, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	return f(ctx, targetPath, opts)
}

// awaitClosed waits until signal is closed; what says what that means.
func awaitClosed(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(treeTestGuard):
		require.FailNow(t, "waited in vain", what)
	}
}

// treeRun is a tree transfer running on a goroutine of its own, the way a
// command runs it while the test watches, with every event it reported.
type treeRun struct {
	done    chan struct{}
	summary TreeSummary
	err     error
	// events is appended to by the transfer's collector alone and read only
	// once done is closed.
	events []TreeEvent
	// reported is closed when the collector delivers its first event, so a
	// test knows the collector is running.
	reported     chan struct{}
	reportedOnce stdsync.Once
}

func newTreeRun() *treeRun {
	return &treeRun{done: make(chan struct{}), reported: make(chan struct{})}
}

func (r *treeRun) record(event TreeEvent) {
	r.events = append(r.events, event)
	r.reportedOnce.Do(func() { close(r.reported) })
}

// wait waits for the transfer to return.
func (r *treeRun) wait(t *testing.T) {
	t.Helper()

	select {
	case <-r.done:
	case <-time.After(treeTestGuard):
		require.FailNow(t, "the tree transfer did not return")
	}
}

// startTreeDownload downloads tree's root into localRoot through files.
func startTreeDownload(ctx context.Context, tree *fakeTree, files FileDownloader, localRoot string) *treeRun {
	run := newTreeRun()
	go func() {
		defer close(run.done)
		run.summary, run.err = DownloadTree(ctx, TreeDownload{
			Lister:     tree,
			Files:      files,
			DriveID:    driveid.New("0000000000000001"),
			FolderID:   treeRootID,
			RemoteRoot: treeRemoteRoot,
			LocalRoot:  localRoot,
			Logger:     slog.New(slog.DiscardHandler),
			OnEvent:    run.record,
		})
	}()

	return run
}

// downloadTreeNow downloads tree's root into localRoot with every transfer
// let through, and returns when the download does.
func downloadTreeNow(t *testing.T, tree *fakeTree, files *gatedFiles, localRoot string) *treeRun {
	t.Helper()

	files.openGate()
	run := startTreeDownload(t.Context(), tree, files, localRoot)
	run.wait(t)

	return run
}

// eventsOf is the events of kind, in the order they were reported.
func eventsOf(events []TreeEvent, kind TreeEventKind) []TreeEvent {
	var matching []TreeEvent
	for i := range events {
		if events[i].Kind == kind {
			matching = append(matching, events[i])
		}
	}

	return matching
}

// eventFor is the one event about relPath.
func eventFor(t *testing.T, events []TreeEvent, relPath string) TreeEvent {
	t.Helper()

	var found []TreeEvent
	for i := range events {
		if events[i].RelPath == relPath {
			found = append(found, events[i])
		}
	}
	require.Len(t, found, 1, "events about %q", relPath)

	return found[0]
}

// assertSummaryCountsEvents holds the summary to the events it summarizes.
func assertSummaryCountsEvents(t *testing.T, run *treeRun) {
	t.Helper()

	var bytes int64
	for _, done := range eventsOf(run.events, TreeFileDone) {
		bytes += done.Size
	}
	assert.Equal(t, TreeSummary{
		Files:   len(eventsOf(run.events, TreeFileDone)),
		Folders: len(eventsOf(run.events, TreeFolderCreated)),
		Failed:  len(eventsOf(run.events, TreeFileFailed)) + len(eventsOf(run.events, TreeFolderFailed)),
		Bytes:   bytes,
	}, run.summary)
}

// assertEveryListedFileAccounted is the rule that nothing is dropped without
// a word: every file of every folder the walk listed has exactly one event,
// unless its folder has a failed event that says why the walk did not reach
// it.
func assertEveryListedFileAccounted(t *testing.T, tree *fakeTree, run *treeRun) {
	t.Helper()

	fileEvents := map[string]int{}
	failedFolders := map[string]bool{}
	for i := range run.events {
		switch run.events[i].Kind {
		case TreeFileDone, TreeFileFailed:
			fileEvents[run.events[i].RelPath]++
		case TreeFolderFailed:
			failedFolders[run.events[i].RelPath] = true
		case TreeFolderCreated:
		}
	}
	for path, count := range fileEvents {
		assert.Equal(t, 1, count, "%s is reported once", path)
	}

	for _, folderID := range tree.listedFolders() {
		rel := treeRelPath(folderID)
		if failedFolders[rel] {
			continue
		}
		files := tree.filesOf(folderID)
		for i := range files {
			relFile := treeRelPath(files[i].ID)
			assert.Equal(t, 1, fileEvents[relFile], "%s of listed folder %q has an event", relFile, rel)
		}
	}
}
