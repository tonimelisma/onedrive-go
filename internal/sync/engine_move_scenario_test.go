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
// scenarios. Delta follows Graph's contract: an empty token enumerates the
// whole tree, and a saved token returns only the items changed since it,
// plus tombstones for deleted ones. Like Graph, a folder move or delete
// reports the folder (and ancestors whose eTag changed), never its unchanged
// descendants. A fake that returned the whole tree on every call hid two
// bugs where the engine left those descendants at stale remote paths.
type moveScenarioRemote struct {
	mu       stdsync.Mutex
	driveID  driveid.ID
	items    map[string]*graph.Item // by item ID, as GetItem sees them live
	contents map[string]string      // file content by item ID
	etagSeq  int
	// observedFolderETags is the folder eTag delta reports. Live, a folder's
	// eTag also changes whenever a descendant changes, but remote_state kept
	// the eTag recorded when the folder was created (live retest, movesrc).
	// GetItem returns the churned eTag; delta returns this one.
	observedFolderETags map[string]string

	// changeSeq numbers mutations; changedAt and tombstones record the
	// sequence of each item's last change so delta can return only what a
	// token has not seen.
	changeSeq  int
	changedAt  map[string]int
	tombstones map[string]deletedScenarioItem

	uploads       int
	folderCreates []string
	deletes       []string
	moves         []string
}

type deletedScenarioItem struct {
	item graph.Item
	seq  int
}

func newMoveScenarioRemote(driveID driveid.ID) *moveScenarioRemote {
	return &moveScenarioRemote{
		driveID: driveID,
		items: map[string]*graph.Item{
			"root": {ID: "root", IsRoot: true, IsFolder: true, DriveID: driveID},
		},
		contents:            map[string]string{},
		observedFolderETags: map[string]string{},
		changedAt:           map[string]int{},
		tombstones:          map[string]deletedScenarioItem{},
	}
}

// touch records that delta must report id again.
func (r *moveScenarioRemote) touch(id string) {
	r.changeSeq++
	r.changedAt[id] = r.changeSeq
}

// bumpAncestorETags models OneDrive changing every ancestor folder's eTag when
// something beneath it changes.
func (r *moveScenarioRemote) bumpAncestorETags(parentID string) {
	for id := parentID; id != "" && id != strRoot; {
		item, ok := r.items[id]
		if !ok {
			return
		}
		item.ETag = r.nextETag()
		r.touch(id)
		id = item.ParentID
	}
}

func (r *moveScenarioRemote) nextETag() string {
	r.etagSeq++
	return fmt.Sprintf("etag-%d", r.etagSeq)
}

func (r *moveScenarioRemote) addFolder(id, parentID, name string) {
	etag := r.nextETag()
	r.items[id] = &graph.Item{ID: id, Name: name, ParentID: parentID, IsFolder: true, DriveID: r.driveID, ETag: etag}
	r.observedFolderETags[id] = etag
	r.touch(id)
	r.bumpAncestorETags(parentID)
}

func (r *moveScenarioRemote) addFile(t *testing.T, id, parentID, name, content string) {
	t.Helper()
	r.items[id] = &graph.Item{
		ID: id, Name: name, ParentID: parentID, DriveID: r.driveID,
		Size: int64(len(content)), QuickXorHash: hashContentQuickXor(t, content), ETag: r.nextETag(),
		ModifiedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	r.contents[id] = content
	r.touch(id)
	r.bumpAncestorETags(parentID)
}

func (r *moveScenarioRemote) move(id, parentID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.items[id]
	r.bumpAncestorETags(item.ParentID)
	item.ParentID = parentID
	item.Name = name
	item.ETag = r.nextETag()
	if item.IsFolder {
		r.observedFolderETags[id] = item.ETag
	}
	r.touch(id)
	r.bumpAncestorETags(parentID)
}

// deleteRemotely deletes id on "OneDrive" as another client would.
func (r *moveScenarioRemote) deleteRemotely(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.remove(id)
}

// remove deletes id and, for a folder, its whole subtree, as OneDrive does.
// Only id itself gets a tombstone: delta reports a deleted folder alone.
func (r *moveScenarioRemote) remove(id string) {
	item, ok := r.items[id]
	if !ok {
		return
	}
	r.bumpAncestorETags(item.ParentID)
	for _, childID := range r.childIDs(id) {
		r.removeUnreported(childID)
	}
	delete(r.items, id)
	delete(r.changedAt, id)
	r.changeSeq++
	tombstone := *item
	tombstone.IsDeleted = true
	r.tombstones[id] = deletedScenarioItem{item: tombstone, seq: r.changeSeq}
}

func (r *moveScenarioRemote) removeUnreported(id string) {
	for _, childID := range r.childIDs(id) {
		r.removeUnreported(childID)
	}
	delete(r.items, id)
	delete(r.changedAt, id)
}

func (r *moveScenarioRemote) childIDs(parentID string) []string {
	var ids []string
	for id, item := range r.items {
		if item.ParentID == parentID {
			ids = append(ids, id)
		}
	}

	return ids
}

// delta returns the page for token: the whole tree for "", otherwise the
// items changed and deleted since that token's sequence.
func (r *moveScenarioRemote) delta(token string) *graph.DeltaPage {
	r.mu.Lock()
	since, incremental := parseScenarioDeltaToken(token)
	next := fmt.Sprintf("seq-%d", r.changeSeq)
	r.mu.Unlock()
	if !incremental {
		return deltaPageWithItems(r.snapshot(), next)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var items []graph.Item
	for id, seq := range r.changedAt {
		if seq <= since {
			continue
		}
		reported := *r.items[id]
		if observed, ok := r.observedFolderETags[id]; ok {
			reported.ETag = observed
		}
		items = append(items, reported)
	}
	for _, deleted := range r.tombstones {
		if deleted.seq > since {
			items = append(items, deleted.item)
		}
	}

	return deltaPageWithItems(items, next)
}

func parseScenarioDeltaToken(token string) (int, bool) {
	var seq int
	if _, err := fmt.Sscanf(token, "seq-%d", &seq); err != nil {
		return 0, false
	}

	return seq, true
}

func (r *moveScenarioRemote) snapshot() []graph.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []graph.Item{*r.items["root"]}
	for id, item := range r.items {
		if id == "root" {
			continue
		}
		reported := *item
		if observed, ok := r.observedFolderETags[id]; ok {
			reported.ETag = observed
		}
		out = append(out, reported)
	}
	return out
}

func (r *moveScenarioRemote) client(t *testing.T) *engineMockClient {
	t.Helper()

	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			return r.delta(token), nil
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
			r.remove(itemID)
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

// requireScenarioBaselineMoved asserts a folder move re-pathed the subtree's
// baseline rows: left at the old paths, every descendant replans as a move
// whose source is gone and is superseded forever.
func requireScenarioBaselineMoved(t *testing.T, eng *testEngine, oldPaths, newPaths []string) {
	t.Helper()

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	for _, p := range oldPaths {
		_, found := bl.GetByPath(p)
		assert.False(t, found, "baseline still holds pre-move path %s", p)
	}
	for _, p := range newPaths {
		_, found := bl.GetByPath(p)
		assert.True(t, found, "baseline lacks moved path %s", p)
	}
}

// requireScenarioQuietPlan asserts the next plan from current truth is empty.
func requireScenarioQuietPlan(t *testing.T, eng *testEngine) {
	t.Helper()

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{DryRun: true})
	require.NoError(t, err)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates+report.Moves,
		"converged state must plan nothing: %+v", report)
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

	requireScenarioBaselineMoved(t, eng,
		[]string{"deep", "deep/er", "deep/er/still deeper", "deep/er/still deeper/leaf.md"},
		[]string{
			"ünïcødé 日本語 🎉/moved-deep", "ünïcødé 日本語 🎉/moved-deep/er",
			"ünïcødé 日本語 🎉/moved-deep/er/still deeper", "ünïcødé 日本語 🎉/moved-deep/er/still deeper/leaf.md",
		})
	requireScenarioQuietPlan(t, eng)

	// A converged pass afterwards must be quiet.
	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)
	assert.NoDirExists(t, filepath.Join(syncRoot, "deep"))
}

// Validates: R-2.17.1
func TestRunOnce_LegacyFolderBaselineGainsIdentityAndMovesAsOne(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-old", "root", "old")
	remote.addFolder("item-sub", "item-old", "sub")
	remote.addFile(t, "item-leaf", "item-sub", "leaf.md", "leaf\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// Simulate a state database written before folder baselines recorded
	// their filesystem identity.
	ctx := t.Context()
	_, err := eng.baseline.rawDB().ExecContext(ctx, `
		UPDATE baseline SET local_device = 0, local_inode = 0, local_has_identity = 0
		WHERE item_type = 'folder'`)
	require.NoError(t, err)
	require.NoError(t, eng.baseline.reloadBaselineCache(ctx))

	// The next full scan backfills identity without changing anything.
	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads+report.LocalDeletes+report.RemoteDeletes+report.FolderCreates)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "old"), filepath.Join(syncRoot, "new")))
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"item-old->new"}, remote.moves, "a legacy folder must move as one item once its identity is known")
	assert.Empty(t, remote.folderCreates)
	assert.Empty(t, remote.deletes)
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
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

// Validates: R-2.17.1
func TestRunOnce_LocalFolderMoveSurvivesFolderETagChurn(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	eng, syncRoot := newTestEngine(t, remote.client(t))

	// Created locally, so the folders are created remotely before their
	// children are uploaded into them: the recorded folder eTags go stale.
	writeLocalFile(t, syncRoot, filepath.Join("movesrc", "inner", "a.txt"), "a\n")
	runScenarioPass(t, eng)
	require.Len(t, remote.folderCreates, 2)

	require.NoError(t, os.Rename(filepath.Join(syncRoot, "movesrc"), filepath.Join(syncRoot, "movedst")))
	runScenarioPass(t, eng)

	assert.Equal(t, []string{"created-movesrc->movedst"}, remote.moves)
	assert.Empty(t, remote.deletes)
	assert.Equal(t, 1, remote.uploads)
	requireScenarioBaselineMoved(t, eng,
		[]string{"movesrc", "movesrc/inner", "movesrc/inner/a.txt"},
		[]string{"movedst", "movedst/inner", "movedst/inner/a.txt"})

	// Delta now reports the children under movedst/ and local agrees: the
	// next passes must converge without planning a child move.
	requireScenarioQuietPlan(t, eng)
	runScenarioPass(t, eng)
	assert.Equal(t, []string{"created-movesrc->movedst"}, remote.moves, "no descendant move after the folder move")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.2.3
func TestRunOnce_TimestampOnlyLocalChangeRefreshesBaselineWithoutTransfer(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFile(t, "item-touch", "root", "touch.txt", "touch me\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)

	// touch -d 2020-01-01: same content, new mtime.
	old := time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(syncRoot, "touch.txt"), old, old))

	report := runScenarioPass(t, eng)
	assert.Zero(t, report.Uploads+report.Downloads, "a timestamp-only change transfers nothing")
	assert.Zero(t, remote.uploads)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByPath("touch.txt")
	require.True(t, ok)
	assert.Equal(t, old.UnixNano(), entry.LocalMtime,
		"the baseline must learn the new mtime, or every scan re-hashes the file")
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.3
func TestRunOnce_RemoteFolderRenameMovesExistingChildrenOnce(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-links", "root", "links")
	remote.addFolder("item-sub", "item-links", "sub")
	remote.addFile(t, "item-a", "item-links", "a.txt", "a\n")
	remote.addFile(t, "item-b", "item-sub", "b.txt", "b\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "links", "sub", "b.txt"))

	// Renamed on OneDrive: the next delta reports only the folder.
	remote.move("item-links", "root", "links-renamed")
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "links"), "the old folder must not come back")
	assert.FileExists(t, filepath.Join(syncRoot, "links-renamed", "a.txt"))
	assert.FileExists(t, filepath.Join(syncRoot, "links-renamed", "sub", "b.txt"))
	assert.Empty(t, remote.folderCreates, "nothing is re-created on OneDrive")
	assert.Empty(t, remote.moves, "the engine moves nothing back")
	assert.Zero(t, remote.uploads)
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.17.2
func TestRunOnce_RemoteFolderDeleteRemovesLocalTreeAndLeavesNoDrift(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	remote := newMoveScenarioRemote(driveID)
	remote.addFolder("item-probe", "root", "probe")
	remote.addFolder("item-len", "item-probe", "len")
	remote.addFile(t, "item-f", "item-len", "f.txt", "f\n")

	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.FileExists(t, filepath.Join(syncRoot, "probe", "len", "f.txt"))

	// Deleted on OneDrive: the next delta reports only the folder's tombstone.
	remote.deleteRemotely("item-probe")
	runScenarioPass(t, eng)

	assert.NoDirExists(t, filepath.Join(syncRoot, "probe"))
	var leftover int
	require.NoError(t, eng.baseline.rawDB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM remote_state WHERE path LIKE 'probe%'`).Scan(&leftover))
	assert.Zero(t, leftover, "no remote_state rows survive under a deleted folder")
	requireScenarioQuietPlan(t, eng)
}
