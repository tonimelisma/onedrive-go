package sync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// moveScenarioRemote is a tiny in-memory OneDrive for multi-pass engine
// scenarios: every delta call returns the whole current tree, and mutations
// change that tree, so each RunOnce sees what the previous one did.
type moveScenarioRemote struct {
	mu       stdsync.Mutex
	driveID  driveid.ID
	items    map[string]*graph.Item // by item ID
	contents map[string]string      // file content by item ID
	etagSeq  int

	uploads       int
	folderCreates []string
	deletes       []string
	moves         []string
}

func newMoveScenarioRemote(driveID driveid.ID) *moveScenarioRemote {
	return &moveScenarioRemote{
		driveID: driveID,
		items: map[string]*graph.Item{
			"root": {ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
		},
		contents: map[string]string{},
	}
}

func (r *moveScenarioRemote) nextETag() string {
	r.etagSeq++
	return fmt.Sprintf("etag-%d", r.etagSeq)
}

func (r *moveScenarioRemote) addFolder(id, parentID, name string) {
	r.items[id] = &graph.Item{ID: id, Name: name, ParentID: parentID, IsFolder: true, DriveID: r.driveID, ETag: r.nextETag()}
}

func (r *moveScenarioRemote) addFile(t *testing.T, id, parentID, name, content string) {
	t.Helper()
	r.items[id] = &graph.Item{
		ID: id, Name: name, ParentID: parentID, DriveID: r.driveID,
		Size: int64(len(content)), QuickXorHash: hashContentQuickXor(t, content), ETag: r.nextETag(),
		ModifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	r.contents[id] = content
}

func (r *moveScenarioRemote) move(id, parentID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.items[id]
	item.ParentID = parentID
	item.Name = name
	item.ETag = r.nextETag()
}

func (r *moveScenarioRemote) snapshot() []graph.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []graph.Item{*r.items["root"]}
	for id, item := range r.items {
		if id != "root" {
			out = append(out, *item)
		}
	}
	return out
}

func (r *moveScenarioRemote) client(t *testing.T) *engineMockClient {
	t.Helper()

	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems(r.snapshot(), "token"), nil
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			item, ok := r.items[itemID]
			if !ok {
				return nil, graph.ErrNotFound
			}
			copied := *item
			return &copied, nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			r.mu.Lock()
			content := r.contents[itemID]
			r.mu.Unlock()
			n, err := w.Write([]byte(content))
			return int64(n), err
		},
		moveItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, newParentID, newName, _ string) (*graph.Item, error) {
			r.move(itemID, newParentID, newName)
			r.mu.Lock()
			defer r.mu.Unlock()
			r.moves = append(r.moves, itemID+"->"+newName)
			copied := *r.items[itemID]
			return &copied, nil
		},
		createFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			id := "created-" + name
			r.addFolder(id, parentID, name)
			r.folderCreates = append(r.folderCreates, name)
			copied := *r.items[id]
			return &copied, nil
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, _ string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.items, itemID)
			r.deletes = append(r.deletes, itemID)
			return nil
		},
		uploadFn: func(
			_ context.Context, _ driveid.ID, parentID, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc,
		) (*graph.Item, error) {
			data := make([]byte, size)
			if _, err := content.ReadAt(data, 0); err != nil && err != io.EOF {
				return nil, fmt.Errorf("read upload content: %w", err)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.uploads++
			id := "uploaded-" + name
			r.addFile(t, id, parentID, name, string(data))
			copied := *r.items[id]
			return &copied, nil
		},
	}
}

func runScenarioPass(t *testing.T, eng *testEngine) *Report {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 0, report.Failed, "errors: %v", report.Errors)

	return report
}

// Validates: R-2.17.1, R-2.17.2
func TestRunOnce_LocalFolderMoveBecomesOneRemoteMoveWithoutResurrectingSource(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-deep", "root", "deep")
	remote.addFolder("item-er", "item-deep", "er")
	remote.addFolder("item-still", "item-er", "still deeper")
	remote.addFile(t, "item-leaf", "item-still", "leaf.md", "leaf\n")
	remote.addFolder("item-target", "root", "ünïcødé 日本語 🎉")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "deep", "er", "still deeper", "leaf.md"))

	require.NoError(t, os.Rename(
		filepath.Join(syncRoot, "deep"),
		filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "moved-deep"),
	))

	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-deep->moved-deep"}, remote.moves, "one remote folder move carries the subtree")
	assert.Empty(t, remote.folderCreates)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, remote.uploads)
	assert.NoDirExists(t, filepath.Join(syncRoot, "deep"), "the moved source tree must not be re-created locally")
	assert.FileExists(t, filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "moved-deep", "er", "still deeper", "leaf.md"))

	// A converged pass afterwards must be quiet.
	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)
	assert.NoDirExists(t, filepath.Join(syncRoot, "deep"))
}

// Validates: R-2.17.2
func TestRunOnce_LocalFolderDeleteDeletesRemoteFolder(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-many", "root", "many")
	for i := range 3 {
		remote.addFile(t, fmt.Sprintf("item-f%03d", i), "item-many", fmt.Sprintf("f%03d.txt", i), fmt.Sprintf("file %d\n", i))
	}

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "many", "f000.txt"))

	require.NoError(t, os.RemoveAll(filepath.Join(syncRoot, "many")))
	runScenarioPass(t, eng)

	assert.Contains(t, remote.deletes, "item-many", "the deleted folder must be deleted on OneDrive")
	assert.NoDirExists(t, filepath.Join(syncRoot, "many"), "the deleted folder must not be re-created locally")

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.FolderCreates)
	assert.NoDirExists(t, filepath.Join(syncRoot, "many"))
}

// Validates: R-2.17.3
func TestRunOnce_RemoteRenameAppliedLocallyDoesNotReupload(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-dir", "root", "ünïcødé 日本語 🎉")
	remote.addFile(t, "item-renamed", "root", "renamed.txt", "ok")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "renamed.txt"))

	remote.move("item-renamed", "item-dir", "renamed-remotely.txt")
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "ünïcødé 日本語 🎉", "renamed-remotely.txt"))
	require.NoFileExists(t, filepath.Join(syncRoot, "renamed.txt"))

	report := runScenarioPass(t, eng)
	assert.Zero(t, remote.uploads, "unchanged content moved by the server must not be uploaded again")
	assert.Zero(t, report.Uploads+report.Downloads)
}

// Validates: R-2.17.4
func TestRunOnce_LocalFileReplacedByFolderConverges(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-lower", "root", "lowercase.txt", "x\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "lowercase.txt"))

	require.NoError(t, os.Remove(filepath.Join(syncRoot, "lowercase.txt")))
	writeLocalFile(t, syncRoot, filepath.Join("lowercase.txt", "inside.txt"), "\n")
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-lower"}, remote.deletes, "the old file is replaced, not overwritten with a directory")
	assert.Equal(t, []string{"lowercase.txt"}, remote.folderCreates)
	assert.Equal(t, 1, remote.uploads)

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates,
		"a replaced item must converge, not stall or churn")
}
