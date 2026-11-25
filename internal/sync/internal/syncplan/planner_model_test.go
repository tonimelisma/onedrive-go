package syncplan

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// Planner-level model test (D-093). A pure model of the three truths (the
// local tree, OneDrive, and the baseline of what was last synced) is written
// into a fresh store, the real comparison SQL and planner plan it, and the
// plan is applied to the model as a faithful executor would apply it, with no
// executor, filesystem or Graph in the loop. Random edits on both sides must
// converge with every written content kept and a quiet plan at the end.
//
// It is the planner's half of the engine model test
// (engine_model_based_test.go): thousands of seeds per minute instead of a
// handful, at the price of trusting the model's executor. A failing seed logs
// its `seed-replay:` line, the command that reruns it with
// ONEDRIVE_GO_PLANNER_SEED=<seed>; widen the search with
// ONEDRIVE_GO_PLANNER_SEEDS=<count> from ONEDRIVE_GO_PLANNER_SEED_START=<seed>
// (default 1).

const (
	plannerModelMaxRounds    = 10
	plannerModelEditsPerSide = 3
	plannerModelFileNames    = "a.txt b.txt c.txt"
	plannerModelFolderNames  = "d e"
	plannerModelLocalDevice  = 1
	pmBirthTimeBase          = int64(1_600_000_000_000_000_000)
)

// pmItem is one entry of a model side. Remote entries carry the item ID and
// eTag version; local entries carry the inode.
type pmItem struct {
	id      string
	folder  bool
	content string
	inode   uint64
	etag    int
}

// pmSynced is a baseline row: the item as both sides last agreed on it.
type pmSynced struct {
	id            string
	folder        bool
	localContent  string
	remoteContent string
	inode         uint64
	etag          int
}

type pmState struct {
	t *testing.T
	// store is reused across rounds, emptied before each: opening a fresh
	// one per plan dominated the run time.
	store    *syncstore.SyncStore
	local    map[string]pmItem
	remote   map[string]pmItem
	baseline map[string]pmSynced
	// planned is the baseline as the plan being applied saw it: an
	// action's outcome carries the rows of its view (plannedSynced).
	planned map[string]pmSynced
	// refused is set by an action the executor would fail (a stale
	// precondition, a 409): apply skips what waits for it.
	refused bool
	seq     int
	log     []string
	// birthTimes makes every local entry report a birth time, as a
	// filesystem that records one does (Linux statx, macOS st_birthtime);
	// off, the model is a filesystem that records none. An entry's birth
	// time follows its inode, which the model never reuses, so a rename
	// keeps it and a new entry gets its own (R-2.17.1).
	birthTimes bool
}

func newPMState(t *testing.T) *pmState {
	t.Helper()

	return &pmState{t: t, local: map[string]pmItem{}, remote: map[string]pmItem{}, baseline: map[string]pmSynced{}}
}

func (s *pmState) next() int {
	s.seq++
	return s.seq
}

// refusef fails the action being applied, as the executor fails it: nothing
// changes, and the actions that wait for it are skipped.
func (s *pmState) refusef(format string, args ...any) {
	s.refused = true
	s.logf("    "+format, args...)
}

func (s *pmState) logf(format string, args ...any) {
	s.log = append(s.log, fmt.Sprintf(format, args...))
}

// addSynced puts an entry on both sides and in the baseline.
func (s *pmState) addSynced(p string, folder bool, content string) {
	id := "item-" + strconv.Itoa(s.next())
	inode := uint64(s.next())
	etag := s.next()
	s.remote[p] = pmItem{id: id, folder: folder, content: content, etag: etag}
	s.local[p] = pmItem{folder: folder, content: content, inode: inode}
	s.baseline[p] = pmSynced{id: id, folder: folder, localContent: content, remoteContent: content, inode: inode, etag: etag}
}

// birthTime is the birth time the local entry with inode reports: none
// unless the model records birth times.
func (s *pmState) birthTime(inode uint64) int64 {
	if !s.birthTimes {
		return 0
	}

	return pmBirthTimeBase + int64(inode)
}

func pmMtime(content string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(content))

	return 1_700_000_000_000_000_000 + int64(h.Sum32())*1_000_000
}

func (s *pmState) hash(folder bool, content string) string {
	if folder {
		return ""
	}

	return syncfixture.HashContentQuickXor(s.t, content)
}

func pmType(folder bool) syncdomain.ItemType {
	if folder {
		return syncdomain.ItemTypeFolder
	}

	return syncdomain.ItemTypeFile
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

// remoteParentID is the item ID of p's parent on OneDrive.
func (s *pmState) remoteParentID(p string) string {
	dir := path.Dir(p)
	if dir == "." {
		return syncdomain.StrRoot
	}
	if parent, ok := s.remote[dir]; ok {
		return parent.id
	}

	return syncdomain.StrRoot
}

// plan writes the model into a fresh store and plans it.
func (s *pmState) plan() *syncdomain.ActionPlan {
	s.writeStore()

	return planCurrentStateForStore(s.t, s.store)
}

// writeStore writes the model into the store, emptied first: OneDrive's
// items, the baseline and the local tree.
func (s *pmState) writeStore() {
	t := s.t
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	if s.store == nil {
		s.store = syncstoretest.NewTestStore(t)
	}
	store := s.store
	for _, table := range []string{"baseline", "remote_state", "local_state"} {
		_, err := syncstoretest.RawDB(t, store).ExecContext(ctx, "DELETE FROM "+table)
		require.NoError(t, err)
	}
	store.DropBaselineCacheForTest()

	var observed []syncdomain.ObservedItem
	for _, p := range sortedKeys(s.remote) {
		item := s.remote[p]
		observed = append(observed, syncdomain.ObservedItem{
			DriveID: driveID, ItemID: item.id, ParentID: s.remoteParentID(p), Path: p, ItemType: pmType(item.folder),
			Hash: s.hash(item.folder, item.content), Size: int64(len(item.content)), Mtime: pmMtime(item.content),
			ETag: "etag-" + strconv.Itoa(item.etag),
		})
	}
	require.NoError(t, store.CommitObservation(ctx, observed, "token", driveID))

	for _, p := range sortedKeys(s.baseline) {
		row := s.baseline[p]
		parentID := s.remoteParentID(p)
		if dir := path.Dir(p); dir != "." {
			if synced, ok := s.baseline[dir]; ok {
				parentID = synced.id
			}
		}
		require.NoError(t, syncstore.UpsertBaselineRowTxForTest(ctx, syncstoretest.RawDB(t, store), &syncdomain.BaselineEntry{
			Path: p, ItemID: row.id, ParentID: parentID, ItemType: pmType(row.folder),
			LocalHash: s.hash(row.folder, row.localContent), RemoteHash: s.hash(row.folder, row.remoteContent),
			LocalSize: int64(len(row.localContent)), LocalSizeKnown: true,
			RemoteSize: int64(len(row.remoteContent)), RemoteSizeKnown: true,
			LocalMtime: pmMtime(row.localContent), RemoteMtime: pmMtime(row.remoteContent),
			LocalCtime: pmMtime(row.localContent), LocalDevice: plannerModelLocalDevice, LocalInode: row.inode,
			LocalBirthTime: s.birthTime(row.inode), LocalHasIdentity: true, ETag: "etag-" + strconv.Itoa(row.etag),
		}))
	}

	var local []syncdomain.LocalStateRow
	for _, p := range sortedKeys(s.local) {
		item := s.local[p]
		local = append(local, syncdomain.LocalStateRow{
			Path: p, ItemType: pmType(item.folder), Hash: s.hash(item.folder, item.content),
			Size: int64(len(item.content)), Mtime: pmMtime(item.content), Ctime: pmMtime(item.content),
			LocalDevice: plannerModelLocalDevice, LocalInode: item.inode, LocalBirthTime: s.birthTime(item.inode),
			LocalHasIdentity: true,
		})
	}
	require.NoError(t, store.ReplaceLocalState(ctx, local))
}

// actionLine renders an action for logs and the golden matrix.
func actionLine(a *syncdomain.Action) string {
	line := a.Type.String() + " " + a.Path
	if a.OldPath != "" {
		line += " <- " + a.OldPath
	}
	if a.Type == syncdomain.ActionFolderCreate {
		line += " (" + a.CreateSide.String() + ")"
	}

	return line
}

// planOrder is the plan's actions in an order that respects its
// dependencies, as the dependency graph would dispatch them.
func planOrder(t *testing.T, plan *syncdomain.ActionPlan) []int {
	t.Helper()

	done := make([]bool, len(plan.Actions))
	order := make([]int, 0, len(plan.Actions))
	for len(order) < len(plan.Actions) {
		progressed := false
		for i := range plan.Actions {
			if done[i] {
				continue
			}
			ready := true
			if i < len(plan.Deps) {
				for _, dep := range plan.Deps[i] {
					ready = ready && done[dep]
				}
			}
			if ready {
				done[i] = true
				order = append(order, i)
				progressed = true
			}
		}
		require.True(t, progressed, "the plan's dependencies form a cycle")
	}

	return order
}

// apply carries out a plan on the model: its actions in dependency order,
// then its held deletes, as a user confirming them would.
func (s *pmState) apply(plan *syncdomain.ActionPlan) {
	s.planned = make(map[string]pmSynced, len(s.baseline))
	for key, row := range s.baseline {
		s.planned[key] = row
	}
	// A failed action's dependents are completed without running, as the
	// engine retires them (completeSubtree), and replanned next pass.
	failed := make([]bool, len(plan.Actions))
	for _, i := range planOrder(s.t, plan) {
		a := &plan.Actions[i]
		s.logf("  %s", actionLine(a))
		if i < len(plan.Deps) && slices.ContainsFunc(plan.Deps[i], func(dep int) bool { return failed[dep] }) {
			s.logf("    skipped: it waits for a failed action")
			failed[i] = true
			continue
		}
		s.refused = false
		s.applyAction(a)
		failed[i] = s.refused
	}
	for _, held := range plan.HeldDeletes {
		s.logf("  confirmed held %v delete %s", held.Side, held.Path)
		if held.Side == syncdomain.DeleteSafetySideLocal {
			s.deleteTree(s.local, held.Path)
		} else {
			s.deleteTree(s.remote, held.Path)
		}
		deleteSyncedTree(s.baseline, held.Path)
	}
}

func (s *pmState) applyAction(a *syncdomain.Action) {
	switch a.Type {
	case syncdomain.ActionDownload:
		s.download(a)
	case syncdomain.ActionUpload:
		s.upload(a)
	case syncdomain.ActionLocalDelete:
		s.deleteTree(s.local, a.Path)
		deleteSyncedTree(s.baseline, a.Path)
	case syncdomain.ActionRemoteDelete:
		if at, _, found := s.remoteItem(a); found {
			s.deleteTree(s.remote, at)
		}
		deleteSyncedTree(s.baseline, a.Path)
	case syncdomain.ActionLocalMove:
		s.localMove(a)
	case syncdomain.ActionRemoteMove:
		s.remoteMove(a)
	case syncdomain.ActionFolderCreate:
		s.createFolder(a)
	case syncdomain.ActionConflictCopy:
		// The executor renames the entry, so a folder's copy carries its
		// whole subtree; the baseline is left as it was (ExecuteConflictCopy).
		_, ok := s.local[a.Path]
		require.True(s.t, ok, "conflict copy of %s: no local entry", a.Path)
		moveTree(s.local, a.Path, conflictModelPath(a.Path, s.next()))
	case syncdomain.ActionBaselineUpdate:
		s.updateBaseline(a)
	case syncdomain.ActionCleanup:
		delete(s.baseline, a.Path)
	}
}

// download fetches by ID, as Graph serves it: the item may still sit at
// another path a later move of this plan puts right.
func (s *pmState) download(a *syncdomain.Action) {
	_, src, ok := s.remoteItem(a)
	require.True(s.t, ok && !src.folder, "download of %s: no remote file", a.Path)
	inode := uint64(s.next())
	s.local[a.Path] = pmItem{content: src.content, inode: inode}
	s.putSynced(a.Path, pmSynced{id: src.id, localContent: src.content, remoteContent: src.content, inode: inode, etag: src.etag})
}

// upload overwrites the item by ID, wherever it is, or creates a new item at
// the path; a create never replaces (409, R-5.6.14), so onto a taken name the
// action fails and the next plan sees what took it.
func (s *pmState) upload(a *syncdomain.Action) {
	src, ok := s.local[a.Path]
	require.True(s.t, ok && !src.folder, "upload of %s: no local file", a.Path)
	etag := s.next()
	at, existing, found := s.remoteItem(a)
	id := existing.id
	if a.ItemID == "" || !found {
		if _, taken := s.remote[a.Path]; taken {
			s.refusef("409: %s is taken on OneDrive", a.Path)
			return
		}
		at, id = a.Path, "item-"+strconv.Itoa(s.next())
	}
	s.remote[at] = pmItem{id: id, content: src.content, etag: etag}
	s.putSynced(a.Path, pmSynced{id: id, localContent: src.content, remoteContent: src.content, inode: src.inode, etag: etag})
}

// localMove renames the local entry as ExecuteLocalMove does: the source
// must still be there and the destination's parent a folder, and a taken
// destination is refused (a stale precondition, replanned next pass) unless
// the move replaces it and it is still the file the plan saw, which goes to
// the trash.
func (s *pmState) localMove(a *syncdomain.Action) {
	if _, ok := s.local[a.OldPath]; !ok {
		s.refusef("stale: local move source %s is missing", a.OldPath)
		return
	}
	if dir := path.Dir(a.Path); dir != "." {
		if parent, ok := s.local[dir]; !ok || !parent.folder {
			s.refusef("stale: local move destination parent %s is missing", dir)
			return
		}
	}
	if dest, taken := s.local[a.Path]; taken {
		planned := a.View.Local
		replaceable := a.ReplaceLocalTarget && !dest.folder && planned != nil && planned.Hash != "" &&
			planned.Hash == s.hash(false, dest.content) && planned.LocalInode == dest.inode
		if !replaceable {
			s.refusef("stale: local move destination %s already exists", a.Path)
			return
		}
		delete(s.local, a.Path)
	}
	moveTree(s.local, a.OldPath, a.Path)
	s.moveSynced(a)
}

func (s *pmState) remoteMove(a *syncdomain.Action) {
	at, _, found := s.remoteItem(a)
	require.True(s.t, found, "remote move to %s: no remote item %s", a.Path, a.ItemID)
	if _, taken := s.remote[a.Path]; taken && at != a.Path {
		s.refusef("409: %s is taken on OneDrive", a.Path)
		return
	}
	moveTree(s.remote, at, a.Path)
	s.bumpRemote(a.Path)
	s.moveSynced(a)
}

// remoteItem finds the remote item an action names: by its ID when it has
// one, as Graph addresses items, else at its path.
func (s *pmState) remoteItem(a *syncdomain.Action) (string, pmItem, bool) {
	if a.ItemID != "" {
		for p, item := range s.remote {
			if item.id == a.ItemID {
				return p, item, true
			}
		}

		return "", pmItem{}, false
	}
	item, ok := s.remote[a.Path]

	return a.Path, item, ok
}

func (s *pmState) bumpRemote(p string) {
	item := s.remote[p]
	item.etag = s.next()
	s.remote[p] = item
}

func (s *pmState) createFolder(a *syncdomain.Action) {
	_, remote, remoteOK := s.remoteItem(a)
	local, localOK := s.local[a.Path]
	if a.CreateSide == syncdomain.CreateRemote {
		require.True(s.t, localOK && local.folder, "remote folder create %s: no local folder", a.Path)
		// The executor creates by parent and name, never by the action's
		// item ID, which can name the item that left the path (a file
		// OneDrive moved away from where a folder was made here), and adopts
		// only a folder there (adoptExistingRemoteFolder); a file at the
		// name refuses the create.
		atPath, taken := s.remote[a.Path]
		switch {
		case taken && !atPath.folder:
			s.refusef("409: %s is a file on OneDrive", a.Path)
			return
		case taken:
			remote = atPath
		default:
			remote = pmItem{id: "item-" + strconv.Itoa(s.next()), folder: true, etag: s.next()}
			s.remote[a.Path] = remote
		}
	} else {
		require.True(s.t, remoteOK && remote.folder, "local folder create %s: no remote folder", a.Path)
		if !localOK {
			local = pmItem{folder: true, inode: uint64(s.next())}
			s.local[a.Path] = local
		}
	}
	s.putSynced(a.Path, pmSynced{id: remote.id, folder: true, inode: local.inode, etag: remote.etag})
}

// updateBaseline records agreement: a rekey (OldPath set) moves the synced
// row and keeps its content facts, so a content change still reads as one;
// otherwise both sides as they are now.
func (s *pmState) updateBaseline(a *syncdomain.Action) {
	if a.OldPath != "" && a.OldPath != a.Path {
		s.moveSynced(a)
		return
	}
	remote, remoteOK := s.remote[a.Path]
	local, localOK := s.local[a.Path]
	require.True(s.t, remoteOK && localOK, "baseline update %s: missing a side", a.Path)
	s.putSynced(a.Path, pmSynced{
		id: remote.id, folder: remote.folder, localContent: local.content, remoteContent: remote.content,
		inode: local.inode, etag: remote.etag,
	})
}

// putSynced records row as the baseline row at p, as the store's upsert
// does (commitUpsert): the baseline is keyed by item ID with a unique path,
// so a row of another item at p is dropped and the item's row at another
// path moves to p, without its children.
func (s *pmState) putSynced(p string, row pmSynced) {
	for key, synced := range s.baseline {
		if synced.id == row.id && key != p {
			delete(s.baseline, key)
		}
	}
	s.baseline[p] = row
}

// moveSynced moves the moved item's baseline row to the action's path, as
// the store's move does (commitMove): the row is found by item ID, not at
// OldPath, which is the moving side's path and can name another item's row,
// and a folder's subtree follows from where its row was.
func (s *pmState) moveSynced(a *syncdomain.Action) {
	from := ""
	for key, synced := range s.baseline {
		if synced.id == a.ItemID {
			from = key
		}
	}
	if from == "" {
		// An earlier action of the plan took the item's row away (a create
		// at its old path drops the row there): the store inserts the row
		// the outcome carries, the baseline the plan saw, and no subtree
		// follows.
		row, planned := s.plannedSynced(a.ItemID)
		require.True(s.t, planned, "%s: no baseline row of %s", actionLine(a), a.ItemID)
		s.putSynced(a.Path, row)
		return
	}
	row := s.baseline[from]
	s.putSynced(a.Path, row)
	if row.folder && from != a.Path {
		repathSyncedSubtree(s.baseline, from, a.Path)
	}
}

// plannedSynced is the row the plan saw for item id.
func (s *pmState) plannedSynced(id string) (pmSynced, bool) {
	for _, row := range s.planned {
		if row.id == id {
			return row, true
		}
	}

	return pmSynced{}, false
}

// repathSyncedSubtree moves the rows strictly below from to sit below to
// (repathSubtreeTx).
func repathSyncedSubtree(baseline map[string]pmSynced, from, to string) {
	moved := map[string]pmSynced{}
	for key, row := range baseline {
		if rest, ok := strings.CutPrefix(key, from+"/"); ok {
			moved[to+"/"+rest] = row
			delete(baseline, key)
		}
	}
	for key, row := range moved {
		baseline[key] = row
	}
}

func conflictModelPath(p string, n int) string {
	ext := path.Ext(p)
	return strings.TrimSuffix(p, ext) + ".conflict-" + strconv.Itoa(n) + ext
}

func (s *pmState) deleteTree(side map[string]pmItem, p string) {
	for key := range side {
		if key == p || strings.HasPrefix(key, p+"/") {
			delete(side, key)
		}
	}
}

func deleteSyncedTree(baseline map[string]pmSynced, p string) {
	for key := range baseline {
		if key == p || strings.HasPrefix(key, p+"/") {
			delete(baseline, key)
		}
	}
}

func moveTree[V any](side map[string]V, from, to string) {
	moved := map[string]V{}
	for key, value := range side {
		switch {
		case key == from:
			moved[to] = value
		case strings.HasPrefix(key, from+"/"):
			moved[to+strings.TrimPrefix(key, from)] = value
		default:
			continue
		}
		delete(side, key)
	}
	for key, value := range moved {
		side[key] = value
	}
}

// view is a side as path -> content, folders as "/".
func pmView(side map[string]pmItem) map[string]string {
	out := make(map[string]string, len(side))
	for p, item := range side {
		if item.folder {
			out[p] = syncfixture.ModelFolderSentinel
			continue
		}
		out[p] = item.content
	}

	return out
}

// pmRun is one seed of the planner model.
type pmRun struct {
	*pmState
	rng     *rand.Rand
	seed    uint64
	written []syncfixture.ModelEdit
	// mustSurvive is the written content sync must keep: pmWrittenStillHeld.
	mustSurvive []syncfixture.ModelEdit
}

// pmWrittenStillHeld is the written content its own side still holds once
// the edits are done, wherever it now sits: a later rename or move of the
// file or a folder above it carries the content along and must not excuse
// losing it, while a later edit, delete or replacement on the same side
// legitimately removed it. Judging by path instead ("did the side touch this
// path again?") let an edit followed by a rename go unchecked.
func pmWrittenStillHeld(written []syncfixture.ModelEdit, local, remote map[string]pmItem) []syncfixture.ModelEdit {
	held := map[string]map[string]bool{syncfixture.ModelLocalSide: {}, syncfixture.ModelRemoteSide: {}}
	for side, items := range map[string]map[string]pmItem{syncfixture.ModelLocalSide: local, syncfixture.ModelRemoteSide: remote} {
		for _, item := range items {
			if !item.folder {
				held[side][item.content] = true
			}
		}
	}
	var survive []syncfixture.ModelEdit
	for _, edit := range written {
		if edit.Content != "" && held[edit.Side][edit.Content] {
			survive = append(survive, edit)
		}
	}

	return survive
}

func (r *pmRun) pick(options []string) string {
	if len(options) == 0 {
		return ""
	}

	return options[r.rng.IntN(len(options))]
}

func pmFolders(side map[string]pmItem) []string {
	folders := []string{""}
	for _, p := range sortedKeys(side) {
		if side[p].folder {
			folders = append(folders, p)
		}
	}

	return folders
}

func (r *pmRun) freePath(side map[string]pmItem, pool string) string {
	candidate := path.Join(r.pick(pmFolders(side)), r.pick(strings.Fields(pool)))
	if _, taken := side[candidate]; taken {
		return ""
	}

	return candidate
}

func (r *pmRun) content(sideName string) string {
	return fmt.Sprintf("seed %d %s %d", r.seed, sideName, r.next())
}

// mutate makes one random edit on a side, as a user or another device would.
func (r *pmRun) mutate(sideName string, side map[string]pmItem, isLocal bool) {
	e := pmSideEdit{run: r, name: sideName, side: side, isLocal: isLocal}
	entries := sortedKeys(side)
	switch r.rng.IntN(6) {
	case 0: // create a file
		if p := r.freePath(side, plannerModelFileNames); p != "" {
			content := r.content(sideName)
			side[p] = e.newEntry(false, content)
			e.record(syncfixture.ModelOpCreateFile, p, "", content)
		}
	case 1:
		e.edit(r.pick(entries))
	case 2: // delete an entry and its subtree
		if p := r.pick(entries); p != "" {
			r.deleteTree(side, p)
			e.record(syncfixture.ModelOpDelete, p, "", "")
		}
	case 3:
		e.move(r.pick(entries))
	case 4: // make a folder
		if p := r.freePath(side, plannerModelFolderNames); p != "" {
			side[p] = e.newEntry(true, "")
			e.record(syncfixture.ModelOpMakeFolder, p, "", "")
		}
	default:
		e.replace(r.pick(entries))
	}
}

// pmSideEdit is one random edit on one side of the planner model.
type pmSideEdit struct {
	run     *pmRun
	name    string
	side    map[string]pmItem
	isLocal bool
}

func (e pmSideEdit) newEntry(folder bool, content string) pmItem {
	if e.isLocal {
		return pmItem{folder: folder, content: content, inode: uint64(e.run.next())}
	}

	return pmItem{id: "item-" + strconv.Itoa(e.run.next()), folder: folder, content: content, etag: e.run.next()}
}

func (e pmSideEdit) record(kind, p, to, content string) {
	if to != "" {
		e.run.logf("%s %s %s -> %s", e.name, kind, p, to)
	} else {
		e.run.logf("%s %s %s", e.name, kind, p)
	}
	e.run.written = append(e.run.written, syncfixture.ModelEdit{Side: e.name, Kind: kind, Path: p, To: to, Content: content})
}

// edit rewrites a file in place.
func (e pmSideEdit) edit(p string) {
	item, ok := e.side[p]
	if !ok || item.folder {
		return
	}
	item.content = e.run.content(e.name)
	if !e.isLocal {
		item.etag = e.run.next()
	}
	e.side[p] = item
	e.record(syncfixture.ModelOpEditFile, p, "", item.content)
}

// move renames an entry or moves it into another folder.
func (e pmSideEdit) move(from string) {
	if from == "" {
		return
	}
	pool := plannerModelFileNames
	if e.side[from].folder {
		pool = plannerModelFolderNames
	}
	to := e.run.freePath(e.side, pool+" moved-"+path.Base(from))
	if to == "" || to == from || strings.HasPrefix(to, from+"/") {
		return
	}
	if _, parentOK := e.side[path.Dir(to)]; path.Dir(to) != "." && !parentOK {
		return
	}
	moveTree(e.side, from, to)
	if !e.isLocal {
		// A move gives the item a new eTag; its children keep theirs.
		moved := e.side[to]
		moved.etag = e.run.next()
		e.side[to] = moved
	}
	e.record(syncfixture.ModelOpMove, from, to, "")
}

// replace swaps an entry for one of the other type.
func (e pmSideEdit) replace(p string) {
	if p == "" {
		return
	}
	wasFolder := e.side[p].folder
	e.run.deleteTree(e.side, p)
	content := e.run.content(e.name)
	if wasFolder {
		e.side[p] = e.newEntry(false, content)
	} else {
		e.side[p] = e.newEntry(true, "")
		e.side[path.Join(p, "inside.txt")] = e.newEntry(false, content)
	}
	e.record(syncfixture.ModelOpReplace, p, "", content)
}

// Validates: R-2.3.1, R-2.17.1, R-2.17.2
func TestPlannerModel_RandomTriplesConverge(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	for _, seed := range syncfixture.PlannerModelSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			syncfixture.LogSeedReplayOnFailure(t, syncfixture.PlannerModelSeedEnv, seed)
			runPlannerModel(t, seed)
		})
	}
}

// Validates: R-2.3.1
//
// Seed 327056 conflict-copies the folder e, which holds a file created here,
// before moving the remote's d onto e's name. The executor renames a folder
// with its subtree, but the model moved only the folder's own entry, so the
// new file stayed at e/b.txt for the move to land on and was reported lost;
// the planner was right. The model now renames the subtree, as the executor
// does.
func TestPlannerModel_AFolderConflictCopyCarriesItsSubtree(t *testing.T) {
	t.Parallel()

	runPlannerModel(t, plannerModelFolderConflictCopySeed)
}

// Validates: R-2.3.1, R-2.17.6
//
// Seeds 9132 and 26899 move a folder on both sides to different places, and
// a folder made here at OneDrive's destination is bound to the item there.
// The store keys the baseline by item ID with a unique path, so that row
// replaces the item's row at its old path; the model kept both, the store it
// wrote them into kept the old one, and the plan bound the folder again on
// every pass. The planner was right; the model now keys its baseline as the
// store does (putSynced, LI-20261125-02).
func TestPlannerModel_ABaselineRowFollowsItsItemID(t *testing.T) {
	t.Parallel()

	for _, seed := range []uint64{plannerModelBaselineByItemSeedA, plannerModelBaselineByItemSeedB} {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			runPlannerModel(t, seed)
		})
	}
}

// Validates: R-2.3.1, R-2.17.6
//
// Seed 6678 re-keys the baseline row of a folder both sides moved alike after
// a folder create at its old path took the row there away. The store's move
// finds the row by item ID, and with none left inserts the row the outcome
// carries, from the plan's view; the model moved whatever row sat at the old
// path, the new folder's, and the plan never went quiet. The planner was
// right (moveSynced, LI-20261125-02).
func TestPlannerModel_ARekeyFindsItsRowByItemID(t *testing.T) {
	t.Parallel()

	runPlannerModel(t, plannerModelRekeyByItemSeed)
}

// Validates: R-2.3.1, R-2.17.4
//
// Seeds 25963, 39126 and 41422 replace a file here with a folder while
// OneDrive moves the file away. The remote folder create carries the file's
// item ID, which the executor ignores: it creates by parent and name. The
// model looked the folder up by that ID, found the moved file, created
// nothing, and the upload below it went into a folder OneDrive never had.
// The planner was right (createFolder, LI-20261125-02).
func TestPlannerModel_ARemoteFolderCreateIsMadeByName(t *testing.T) {
	t.Parallel()

	for _, seed := range []uint64{
		plannerModelFolderCreateByNameSeedA, plannerModelFolderCreateByNameSeedB, plannerModelFolderCreateByNameSeedC,
	} {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			t.Parallel()
			runPlannerModel(t, seed)
		})
	}
}

// The seeds of the 2026-11-25 sweep of seeds 1 to 42,000 that found the
// model's baseline and remote folder create unlike the store's and the
// executor's.
const (
	plannerModelBaselineByItemSeedA     = 9132
	plannerModelBaselineByItemSeedB     = 26899
	plannerModelRekeyByItemSeed         = 6678
	plannerModelFolderCreateByNameSeedA = 25963
	plannerModelFolderCreateByNameSeedB = 39126
	plannerModelFolderCreateByNameSeedC = 41422
)

// Validates: R-2.17.6
//
// Seed 6943: OneDrive moved e into d and d onto e's name, and a file was
// made here in e. Both folders move here, e inside d before d moves, and the
// file's upload was rebased through e's move alone, to d/e/b.txt, where d's
// move had left nothing. The planner was wrong: the upload now follows both
// moves to e/e/b.txt (LI-20261125-01).
func TestPlannerModel_ACreateFollowsAFolderMoveCarriedOnByAnother(t *testing.T) {
	t.Parallel()

	runPlannerModel(t, plannerModelCarriedFolderMoveSeed)
}

// plannerModelCarriedFolderMoveSeed is the seed of the 2026-11-25 sweep that
// found a create rebased through one of two chained folder moves.
const plannerModelCarriedFolderMoveSeed = 6943

// plannerModelFolderConflictCopySeed is the seed of the 2026-11-24 dispatched
// nightly that found the model's folder conflict copy dropping its subtree.
const plannerModelFolderConflictCopySeed = 327056

// failureContext names the seed and the operations of the run. The
// `seed-replay:` line a failing seed logs (logSeedReplayOnFailure) is the one
// command that reruns it, so the context carries no second one.
func (r *pmRun) failureContext() string {
	return fmt.Sprintf("seed %d (birth times %t):\n  %s", r.seed, r.birthTimes, strings.Join(r.log, "\n  "))
}

func runPlannerModel(t *testing.T, seed uint64) {
	t.Helper()

	r := &pmRun{
		pmState: newPMState(t),
		rng:     rand.New(rand.NewPCG(seed, seed^0x7f4a7c159e3779b9)), //nolint:gosec // deterministic test RNG
		seed:    seed,
	}
	// Odd seeds run on a filesystem that records birth times, where a file
	// moved and edited here is the item (R-2.17.1); even seeds on one that
	// records none, where it is new content.
	r.birthTimes = seed%2 == 1
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("planner model %s", r.failureContext())
		}
	})
	r.addSynced("d", true, "")
	r.addSynced("d/a.txt", false, "synced a")
	r.addSynced("b.txt", false, "synced b")
	r.addSynced("e", true, "")
	r.addSynced("e/c.txt", false, "synced c")

	for range plannerModelEditsPerSide {
		r.mutate(syncfixture.ModelLocalSide, r.local, true)
		r.mutate(syncfixture.ModelRemoteSide, r.remote, false)
	}
	r.mustSurvive = pmWrittenStillHeld(r.written, r.local, r.remote)

	failure := r.failureContext
	quiet := false
	for round := 1; round <= plannerModelMaxRounds && !quiet; round++ {
		plan := r.plan()
		quiet = len(plan.Actions) == 0 && len(plan.HeldDeletes) == 0
		r.logf("round %d: %d actions, %d held deletes", round, len(plan.Actions), len(plan.HeldDeletes))
		r.apply(plan)
	}
	require.True(t, quiet, "the plan never went quiet; %s", failure())

	local, remote := pmView(r.local), pmView(r.remote)
	require.Equal(t, remote, local, "local and OneDrive differ; %s", failure())
	present := map[string]bool{}
	for _, content := range local {
		present[content] = true
	}
	for _, edit := range r.mustSurvive {
		assert.True(t, present[edit.Content], "%s %s of %s was lost; %s", edit.Side, edit.Kind, edit.Path, failure())
	}
}

// Validates: R-2.3.1
//
// The oracle follows content, not paths: an edit followed by a rename of the
// file (or its folder) must survive at the new name; an edit followed by a
// delete or another edit need not.
func TestPMWrittenStillHeld_FollowsContentThroughMoves(t *testing.T) {
	t.Parallel()

	written := []syncfixture.ModelEdit{
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpEditFile, Path: "d/a.txt", Content: "edited then renamed"},
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpMove, Path: "d", To: "e"},
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpEditFile, Path: "b.txt", Content: "edited then deleted"},
		{Side: syncfixture.ModelLocalSide, Kind: syncfixture.ModelOpDelete, Path: "b.txt"},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpEditFile, Path: "c.txt", Content: "overwritten"},
		{Side: syncfixture.ModelRemoteSide, Kind: syncfixture.ModelOpEditFile, Path: "c.txt", Content: "final"},
	}
	local := map[string]pmItem{"e": {folder: true}, "e/a.txt": {content: "edited then renamed"}}
	remote := map[string]pmItem{"c.txt": {content: "final"}}

	var contents []string
	for _, edit := range pmWrittenStillHeld(written, local, remote) {
		contents = append(contents, edit.Content)
	}
	assert.Equal(t, []string{"edited then renamed", "final"}, contents)
	assert.True(t, syncfixture.ModelOverwrittenLater(written, 0), "the path rule this replaces excused the renamed edit")
}

// Validates: R-6.10.25
//
// A failing planner model seed shows one command that reruns it, its
// `seed-replay:` line: the run's failure context names the seed and carries
// no second rerun hint, like the engine model's
// (TestModelRun_FailureContextCarriesNoSecondRerunHint).
func TestPlannerModelRun_FailureContextCarriesNoSecondRerunHint(t *testing.T) {
	t.Parallel()

	context := (&pmRun{pmState: &pmState{}, seed: 7}).failureContext()
	assert.Contains(t, context, "seed 7")
	assert.NotContains(t, context, "rerun")
	assert.NotContains(t, context, "_SEED=")
}
