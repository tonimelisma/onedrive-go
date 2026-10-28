package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"path"
	"sort"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
)

// moveScenarioRemote is a small in-memory OneDrive for multi-pass engine
// scenarios. It follows the Graph behaviors the engine depends on:
//
//   - Delta: an empty token enumerates the whole tree; a saved token returns
//     only the items changed since it, plus tombstones for deleted ones. A
//     folder move or delete reports the folder (and ancestors whose eTag
//     changed), never its unchanged descendants. A fake that returned the
//     whole tree on every call hid two engine bugs that left descendants at
//     stale remote paths.
//   - Names are unique per folder under OneDrive's equivalence (namekey).
//     A create (upload of a new file, folder create) or move onto a taken
//     name fails with 409, as the client's create by body with
//     conflictBehavior=fail does; an overwrite, move or delete by ID honors
//     If-Match, and a move into the item's own subtree is refused.
//   - Deleting a folder deletes its subtree, and a deleted file's content
//     goes to the recycle bin; an overwrite keeps the replaced content as a
//     version. Together they are the no-loss oracle (assertSyncInvariants).
//   - A folder's eTag changes whenever anything beneath it changes, but delta
//     keeps reporting the eTag recorded when the folder was created (see
//     observedFolderETags).
//   - Delta pages are deterministic: an incremental page lists changes in
//     the order they happened, a full one by item ID after the root. With
//     shuffleDelta the order is a seeded permutation instead (D-090), as
//     Graph promises no order within a window.
type moveScenarioRemote struct {
	mu       stdsync.Mutex
	driveID  driveid.ID
	items    map[string]*graph.Item // by item ID, as GetItem sees them live
	contents map[string]string      // file content by item ID
	etagSeq  int
	idSeq    int
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

	// deltaRand, when set, shuffles every delta page (shuffleDelta).
	deltaRand *rand.Rand
	// fullDeltaAt records when each full enumeration (delta without a
	// token) was served.
	fullDeltaAt []time.Time

	// recycled and versions hold every file content OneDrive still keeps
	// after a delete or an overwrite.
	recycled []string
	versions []string

	uploads int
	// interruptCreates stops every create after its empty item exists.
	interruptCreates bool
	folderCreates    []string
	deletes          []string
	moves            []string
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

func (r *moveScenarioRemote) nextID(prefix string) string {
	r.idSeq++
	return fmt.Sprintf("%s-%d", prefix, r.idSeq)
}

// childNamed finds parentID's child whose name matches name the way OneDrive
// compares names: case-insensitively.
func (r *moveScenarioRemote) childNamed(parentID, name string) (*graph.Item, bool) {
	for _, item := range r.items {
		if item.ParentID == parentID && namekey.Equal(item.Name, name) {
			return item, true
		}
	}

	return nil, false
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
	r.items[id] = &graph.Item{ID: id, Name: name, ParentID: parentID, DriveID: r.driveID}
	r.setContent(t, id, content)
}

// setContent replaces a file's bytes in place, as an overwrite on OneDrive
// does: the item keeps its ID and gets a new hash, size and eTag.
func (r *moveScenarioRemote) setContent(t *testing.T, id, content string) {
	t.Helper()
	item := r.items[id]
	if previous, ok := r.contents[id]; ok && previous != content {
		r.versions = append(r.versions, previous)
	}
	item.Size = int64(len(content))
	item.QuickXorHash = hashContentQuickXor(t, content)
	item.ETag = r.nextETag()
	item.ModifiedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Add(time.Duration(r.etagSeq) * time.Second)
	r.contents[id] = content
	r.touch(id)
	r.bumpAncestorETags(item.ParentID)
}

func (r *moveScenarioRemote) move(id, parentID, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.moveLocked(id, parentID, name)
}

func (r *moveScenarioRemote) moveLocked(id, parentID, name string) {
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
	r.recycle(id)
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
	r.recycle(id)
	delete(r.items, id)
	delete(r.changedAt, id)
}

// recycle moves a deleted file's content to the recycle bin.
func (r *moveScenarioRemote) recycle(id string) {
	if content, ok := r.contents[id]; ok {
		r.recycled = append(r.recycled, content)
		delete(r.contents, id)
	}
}

// retainedContents is every file content OneDrive keeps outside the live
// tree: the recycle bin and version history.
func (r *moveScenarioRemote) retainedContents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append(append([]string(nil), r.recycled...), r.versions...)
}

func (r *moveScenarioRemote) childIDs(parentID string) []string {
	var ids []string
	for id, item := range r.items {
		if item.ParentID == parentID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	return ids
}

// pathOf is id's root-relative path, "" for the root.
func (r *moveScenarioRemote) pathOf(id string) string {
	var parts []string
	for current := id; current != "" && current != strRoot; {
		item, ok := r.items[current]
		if !ok {
			return ""
		}
		parts = append([]string{item.Name}, parts...)
		current = item.ParentID
	}

	return strings.Join(parts, "/")
}

// itemAt resolves a root-relative path case-insensitively, as OneDrive does.
func (r *moveScenarioRemote) itemAt(remotePath string) (*graph.Item, bool) {
	current := r.items[strRoot]
	if remotePath == "" {
		return current, true
	}
	for _, name := range strings.Split(remotePath, "/") {
		child, ok := r.childNamed(current.ID, name)
		if !ok {
			return nil, false
		}
		current = child
	}

	return current, true
}

// tree is the drive as path -> content for files and path -> "/" for folders.
func (r *moveScenarioRemote) tree() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.items))
	for id, item := range r.items {
		if id == strRoot {
			continue
		}
		if item.IsFolder {
			out[r.pathOf(id)] = "/"
			continue
		}
		out[r.pathOf(id)] = r.contents[id]
	}

	return out
}

// Remote-side edits by path, as another device would make them.

func (r *moveScenarioRemote) putFileAt(t *testing.T, remotePath, content string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.itemAt(remoteParentPath(remotePath))
	require.True(t, ok && parent.IsFolder, "remote parent of %s missing", remotePath)
	name := path.Base(remotePath)
	if existing, ok := r.childNamed(parent.ID, name); ok {
		require.False(t, existing.IsFolder, "remote %s is a folder", remotePath)
		r.setContent(t, existing.ID, content)
		return
	}
	r.addFile(t, r.nextID("remote-file"), parent.ID, name, content)
}

func (r *moveScenarioRemote) mkdirAt(t *testing.T, remotePath string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	parent, ok := r.itemAt(remoteParentPath(remotePath))
	require.True(t, ok && parent.IsFolder, "remote parent of %s missing", remotePath)
	if _, exists := r.childNamed(parent.ID, path.Base(remotePath)); exists {
		return
	}
	r.addFolder(r.nextID("remote-folder"), parent.ID, path.Base(remotePath))
}

func (r *moveScenarioRemote) deleteAt(t *testing.T, remotePath string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.itemAt(remotePath)
	require.True(t, ok, "remote %s missing", remotePath)
	r.remove(item.ID)
}

func (r *moveScenarioRemote) moveAt(t *testing.T, fromPath, toPath string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.itemAt(fromPath)
	require.True(t, ok, "remote %s missing", fromPath)
	parent, ok := r.itemAt(remoteParentPath(toPath))
	require.True(t, ok && parent.IsFolder, "remote parent of %s missing", toPath)
	r.moveLocked(item.ID, parent.ID, path.Base(toPath))
}

// shuffleDelta makes every later delta page a seeded permutation.
func (r *moveScenarioRemote) shuffleDelta(seed uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deltaRand = rand.New(rand.NewPCG(seed, seed^0x2545f4914f6cdd1d)) //nolint:gosec // deterministic test RNG
}

func (r *moveScenarioRemote) shuffleLocked(items []graph.Item) {
	if r.deltaRand == nil {
		return
	}
	r.deltaRand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
}

func (r *moveScenarioRemote) snapshot() []graph.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	var rest []graph.Item
	for _, id := range r.sortedIDs() {
		if id == "root" {
			continue
		}
		rest = append(rest, r.reported(id))
	}
	r.shuffleLocked(rest)
	return append([]graph.Item{*r.items["root"]}, rest...)
}

func (r *moveScenarioRemote) sortedIDs() []string {
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	return ids
}

func (r *moveScenarioRemote) reported(id string) graph.Item {
	reported := *r.items[id]
	if observed, ok := r.observedFolderETags[id]; ok {
		reported.ETag = observed
	}

	return reported
}

// delta returns the page for token: the whole tree for "", otherwise the
// items changed and deleted since that token's sequence.
func (r *moveScenarioRemote) delta(token string) *graph.DeltaPage {
	r.mu.Lock()
	since, incremental := parseScenarioDeltaToken(token)
	next := fmt.Sprintf("seq-%d", r.changeSeq)
	r.mu.Unlock()
	if !incremental {
		r.mu.Lock()
		r.fullDeltaAt = append(r.fullDeltaAt, time.Now())
		r.mu.Unlock()
		return deltaPageWithItems(r.snapshot(), next)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	type change struct {
		seq  int
		item graph.Item
	}
	var changes []change
	for _, id := range r.sortedIDs() {
		if seq, ok := r.changedAt[id]; ok && seq > since {
			changes = append(changes, change{seq: seq, item: r.reported(id)})
		}
	}
	for id := range r.tombstones {
		if r.tombstones[id].seq > since {
			changes = append(changes, change{seq: r.tombstones[id].seq, item: r.tombstones[id].item})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].seq != changes[j].seq {
			return changes[i].seq < changes[j].seq
		}
		return changes[i].item.ID < changes[j].item.ID
	})
	items := make([]graph.Item, 0, len(changes))
	for i := range changes {
		items = append(items, changes[i].item)
	}
	r.shuffleLocked(items)

	return deltaPageWithItems(items, next)
}

func parseScenarioDeltaToken(token string) (int, bool) {
	var seq int
	if _, err := fmt.Sscanf(token, "seq-%d", &seq); err != nil {
		return 0, false
	}

	return seq, true
}

func (r *moveScenarioRemote) client(t *testing.T) *engineMockClient {
	t.Helper()

	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			return r.delta(token), nil
		},
		getItemFn: func(_ context.Context, _ driveid.ID, itemID string) (*graph.Item, error) {
			return r.getItem(itemID)
		},
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			return r.getItemByPath(remotePath)
		},
		listChildrenFn: func(_ context.Context, _ driveid.ID, parentID string) ([]graph.Item, error) {
			return r.listChildren(parentID), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			return r.download(itemID, w)
		},
		moveItemOptsFn: func(
			_ context.Context, _ driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
		) (*graph.Item, error) {
			return r.moveItem(itemID, newParentID, newName, opts)
		},
		createFolderFn: func(_ context.Context, _ driveid.ID, parentID, name string) (*graph.Item, error) {
			return r.createFolder(parentID, name)
		},
		deleteItemIfMatchFn: func(_ context.Context, _ driveid.ID, itemID, ifMatch string) error {
			return r.deleteItem(itemID, ifMatch)
		},
		uploadFn: func(
			_ context.Context, _ driveid.ID, parentID, name string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc,
		) (*graph.Item, error) {
			return r.upload(t, parentID, name, content, size)
		},
		uploadCreatingFn: func(
			_ context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
		) (*graph.Item, error) {
			return r.uploadCreating(t, parentID, name, content, size, onCreated)
		},
		uploadToItemFn: func(
			_ context.Context, _ driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, _ time.Time, _ graph.ProgressFunc,
		) (*graph.Item, error) {
			return r.uploadToItem(t, itemID, ifMatch, content, size)
		},
	}
}

func (r *moveScenarioRemote) listChildren(parentID string) []graph.Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	var children []graph.Item
	for _, id := range r.childIDs(parentID) {
		children = append(children, *r.items[id])
	}

	return children
}

func (r *moveScenarioRemote) getItem(itemID string) (*graph.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[itemID]
	if !ok {
		return nil, graph.ErrNotFound
	}
	copied := *item
	return &copied, nil
}

func (r *moveScenarioRemote) getItemByPath(remotePath string) (*graph.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.itemAt(strings.Trim(remotePath, "/"))
	if !ok {
		return nil, graph.ErrNotFound
	}
	copied := *item
	return &copied, nil
}

func (r *moveScenarioRemote) download(itemID string, w io.Writer) (int64, error) {
	r.mu.Lock()
	content, ok := r.contents[itemID]
	r.mu.Unlock()
	if !ok {
		return 0, graph.ErrNotFound
	}
	n, err := w.Write([]byte(content))
	if err != nil {
		return int64(n), fmt.Errorf("write scenario download: %w", err)
	}
	return int64(n), nil
}

func scenarioNameConflict() error {
	return &graph.GraphError{StatusCode: 409, Message: "Name already exists", Err: graph.ErrConflict}
}

// scenarioPreconditionFailed is Graph's answer to an If-Match that no longer
// matches the item's eTag (live probe L-02).
func scenarioPreconditionFailed() error {
	return &graph.GraphError{StatusCode: 412, Code: "resourceModified", Err: graph.ErrPreconditionFailed}
}

// scenarioRenameTime is the "now" a rename without a modification time to
// keep stamps on the item, as OneDrive does (L-09).
func scenarioRenameTime() time.Time {
	return time.Date(2026, 10, 25, 20, 55, 50, 0, time.UTC)
}

func (r *moveScenarioRemote) moveItem(itemID, newParentID, newName string, opts graph.MoveOptions) (*graph.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[itemID]
	if !ok {
		return nil, graph.ErrNotFound
	}
	if opts.IfMatch != "" && item.ETag != opts.IfMatch {
		return nil, scenarioPreconditionFailed()
	}
	if newParentID == "" {
		newParentID = item.ParentID
	}
	if r.isSelfOrDescendant(newParentID, itemID) {
		return nil, &graph.GraphError{StatusCode: 400, Code: "invalidRequest", Message: "Cannot move an item into itself", Err: graph.ErrBadRequest}
	}
	keepModified := opts.KeepModified
	if newName == "" {
		newName = item.Name
	}
	if taken, exists := r.childNamed(newParentID, newName); exists && taken.ID != itemID {
		return nil, scenarioNameConflict()
	}
	switch {
	case !keepModified.IsZero():
		item.ModifiedAt = keepModified
	case newName != item.Name:
		item.ModifiedAt = scenarioRenameTime()
	}
	r.moveLocked(itemID, newParentID, newName)
	r.moves = append(r.moves, itemID+"->"+newName)
	copied := *r.items[itemID]
	return &copied, nil
}

// isSelfOrDescendant reports whether id is ancestor or lies beneath it:
// OneDrive refuses to move a folder into its own subtree.
func (r *moveScenarioRemote) isSelfOrDescendant(id, ancestor string) bool {
	for current := id; current != ""; {
		if current == ancestor {
			return true
		}
		item, ok := r.items[current]
		if !ok {
			return false
		}
		current = item.ParentID
	}

	return false
}

func (r *moveScenarioRemote) createFolder(parentID, name string) (*graph.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.childNamed(parentID, name); exists {
		return nil, scenarioNameConflict()
	}
	id := r.nextID("created-folder")
	r.addFolder(id, parentID, name)
	r.folderCreates = append(r.folderCreates, name)
	copied := *r.items[id]
	return &copied, nil
}

func (r *moveScenarioRemote) deleteItem(itemID, ifMatch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[itemID]
	if !ok {
		return graph.ErrNotFound
	}
	if ifMatch != "" && item.ETag != ifMatch {
		return scenarioPreconditionFailed()
	}
	r.remove(itemID)
	r.deletes = append(r.deletes, itemID)
	return nil
}

// upload follows the client's create by body with conflictBehavior=fail
// (R-5.6.14): any item OneDrive treats as having that name, file or folder,
// refuses the create with 409 nameAlreadyExists (live probe L-01).
func (r *moveScenarioRemote) upload(t *testing.T, parentID, name string, content io.ReaderAt, size int64) (*graph.Item, error) {
	t.Helper()
	data, err := readScenarioUpload(content, size)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploads++
	if _, exists := r.childNamed(parentID, name); exists {
		return nil, scenarioNameConflict()
	}
	id := r.nextID("uploaded")
	r.addFile(t, id, parentID, name, data)
	copied := *r.items[id]
	return &copied, nil
}

// uploadCreating follows the client's two-step create (R-5.6.14): an empty
// item with conflictBehavior=fail, the hook, then the content by ID. With
// interruptCreates set it stops after the hook, as a process killed mid-upload
// would, leaving the empty item behind.
func (r *moveScenarioRemote) uploadCreating(
	t *testing.T, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	t.Helper()
	data, err := readScenarioUpload(content, size)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if _, exists := r.childNamed(parentID, name); exists {
		r.mu.Unlock()
		return nil, scenarioNameConflict()
	}
	r.uploads++
	id := r.nextID("created")
	r.addFile(t, id, parentID, name, "")
	created := *r.items[id]
	interrupt := r.interruptCreates
	r.mu.Unlock()

	if onCreated != nil {
		if hookErr := onCreated(&created); hookErr != nil {
			return nil, &graph.CreatedItemError{Item: &created, Err: hookErr}
		}
	}
	if interrupt {
		return nil, &graph.CreatedItemError{Item: &created, Err: errors.New("connection reset by peer")}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if data != "" {
		r.setContent(t, id, data)
	}
	copied := *r.items[id]
	return &copied, nil
}

// uploadToItem honors If-Match like Graph does (live probe L-02).
func (r *moveScenarioRemote) uploadToItem(t *testing.T, itemID, ifMatch string, content io.ReaderAt, size int64) (*graph.Item, error) {
	t.Helper()
	data, err := readScenarioUpload(content, size)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[itemID]
	if !ok {
		return nil, graph.ErrNotFound
	}
	if ifMatch != "" && item.ETag != ifMatch {
		return nil, scenarioPreconditionFailed()
	}
	r.uploads++
	r.setContent(t, itemID, data)
	copied := *r.items[itemID]
	return &copied, nil
}

func readScenarioUpload(content io.ReaderAt, size int64) (string, error) {
	data := make([]byte, size)
	if _, err := content.ReadAt(data, 0); err != nil && err != io.EOF {
		return "", fmt.Errorf("read upload content: %w", err)
	}

	return string(data), nil
}

// clone is an independent copy of the drive as it is now, recycle bin,
// versions and change log included: the OneDrive a crashed client meets when
// it starts again (crash_points_test.go).
func (r *moveScenarioRemote) clone() *moveScenarioRemote {
	r.mu.Lock()
	defer r.mu.Unlock()

	c := &moveScenarioRemote{
		driveID:             r.driveID,
		items:               make(map[string]*graph.Item, len(r.items)),
		contents:            make(map[string]string, len(r.contents)),
		etagSeq:             r.etagSeq,
		idSeq:               r.idSeq,
		observedFolderETags: make(map[string]string, len(r.observedFolderETags)),
		changeSeq:           r.changeSeq,
		changedAt:           make(map[string]int, len(r.changedAt)),
		tombstones:          make(map[string]deletedScenarioItem, len(r.tombstones)),
		recycled:            append([]string(nil), r.recycled...),
		versions:            append([]string(nil), r.versions...),
	}
	for id, item := range r.items {
		copied := *item
		c.items[id] = &copied
	}
	for id, content := range r.contents {
		c.contents[id] = content
	}
	for id, etag := range r.observedFolderETags {
		c.observedFolderETags[id] = etag
	}
	for id, seq := range r.changedAt {
		c.changedAt[id] = seq
	}
	for id := range r.tombstones {
		c.tombstones[id] = r.tombstones[id]
	}

	return c
}
