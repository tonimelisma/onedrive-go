package syncplan

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// Exhaustive planner check. A synced tree of two folders and two files (the
// folder p holding the file f, an empty folder x, and the file z at the top)
// is given every short history of user actions on each side, and the real
// planner plans each pair of histories pass after pass, applied by a model of
// the executor and of OneDrive (exWorld), until a plan has nothing to do or
// the pass limit is reached. It starts twice: from the synced tree, and from
// the same tree after x was renamed x2 here, a new x made and f moved out of p
// into it, while OneDrive made X2, which it takes for the same name as x2, so
// that x's move, and f's into the new x, wait for as long as X2 is there.
//
// A history is none, one or two of these on one side (exEditsOn): an entry
// renamed to a fresh name, moved into another folder under its own name, or
// moved onto a synced path that side no longer holds; an entry deleted; a
// file edited; a file made; a synced folder made again at its path. Fresh
// names are g for a file and q for a folder, and on OneDrive also G and Q,
// which OneDrive takes for g and q, so that a rename onto a path the other
// side changed is one of these moves. A pair makes at most three actions in
// all from the synced tree and two more from the held one.
//
// Two reductions keep the space small enough for the unit run. Histories that
// leave a side in the same state (the same entries, each the same synced item
// or a new one, with the same kind of content) plan alike, so one stands for
// all. A pair whose actions split into groups that touch unrelated paths (no
// path of one group at, above or below a path of another, compared as OneDrive
// compares names) is not run: each group is a pair of shorter histories of its
// own, and the planner decides unrelated paths independently.
//
// Every pass is checked, and the end:
//   - no pass removes from a side the content of a file the user did not
//     delete or overwrite, unless the same pass keeps it in a conflict copy or
//     OneDrive keeps it as a version of the item an upload replaced;
//   - the sides end equal, at a plan with nothing to do, within the pass
//     limit, and no file the user wrote is gone from both;
//   - that plan defers and drops nothing;
//   - a synced item the user deleted on one side, making nothing at its path
//     again, while the other side left it with everything above and below it
//     as synced, is gone from both, unless it lay in a folder the user
//     deleted with it that the other side wrote into: that folder is kept
//     whole, and the item with it;
//   - no side ends holding one file content at two paths, a conflict copy
//     apart: nothing the user did copies a file.
//
// While a name OneDrive takes for another entry's stays on both sides, the
// sides cannot converge; when a plan has nothing left to do or a pass changes
// nothing, OneDrive's user renames that item, as the user must, and the passes
// continue.
//
// A pair whose passes delete something runs again once for each create of
// each of its passes (exRefusals), with that create refused in that pass, as
// a create OneDrive or this computer refuses once: what waits for it is
// skipped, and every pass and the end are checked as before. A refusal is
// named by the create and how many passes had planned it, not by the pass. A
// delete that waits for held work is planned by a later pass, so the
// refusals reach the pass that releases it.
//
// The runs that break an invariant, and which, are checked in as
// testdata/exhaustive/pairs.golden: a change shows up as a diff of that list;
// refresh it with
// `go test ./internal/sync/internal/syncplan -run TestPlannerExhaustive -update`.
//
// What the check does not see: the planner and the store are real, but every
// pass writes the store anew from the model (a full listing, the baseline the
// model keeps, the local state), so the scanner, the delta and the executor's
// baseline commits are not exercised, and each pass is in effect a restart.
// Actions run one at a time in one order the dependencies allow (exPlanOrder),
// and content is compared between passes, not between the actions of one, so
// a window inside a pass is not seen. Only creates are refused, at most one
// per run, and nothing else fails but the model's own preconditions. The
// delete-safety stage, sync modes other than bidirectional, unavailable
// truth, a local volume that folds case, case-only renames of synced entries
// and trees deeper than two folders are outside it.

const (
	exhaustiveGolden = "testdata/exhaustive/pairs.golden"
	exPassLimit      = 6
	exResolvedName   = "r"
	// exWorkers is how many stores run pairs in parallel.
	exWorkers = 4
	// exRaceStride is the share of pairs a run of the full tier under the
	// race detector plans: the planner is single-goroutine apart from the
	// store.
	exRaceStride = 40
	// exSampleStride is the share of pairs any run outside the full tier
	// plans (syncfixture.FullTier).
	exSampleStride = 400
)

// exKind is one kind of user action.
type exKind int

const (
	exMoveFile exKind = iota
	exMoveFolder
	exDeleteFile
	exDeleteFolder
	exEditFile
	exCreateFile
	exMakeFolder
)

// exEdit is one user action on one side: from is the entry it acts on, to the
// path it moves that entry to or makes.
type exEdit struct {
	kind exKind
	from string
	to   string
}

func (e exEdit) String() string {
	switch e.kind {
	case exMoveFile, exMoveFolder:
		return "mv " + e.from + " " + e.to
	case exDeleteFile, exDeleteFolder:
		return "rm " + e.from
	case exEditFile:
		return "edit " + e.from
	case exCreateFile:
		return "new " + e.to
	case exMakeFolder:
		return "mkdir " + e.to
	}

	return ""
}

// paths are the paths the edit acts on.
func (e exEdit) paths() []string {
	var paths []string
	for _, p := range []string{e.from, e.to} {
		if p != "" {
			paths = append(paths, p)
		}
	}

	return paths
}

// exStart is where the histories start: the synced tree after the user
// actions every pair of the start shares.
type exStart struct {
	name   string
	local  []exEdit
	remote []exEdit
	// most is how many actions a pair of the start makes in all.
	most int
}

func exStarts() []exStart {
	return []exStart{
		{name: "synced", most: 3},
		{
			name: "held",
			most: 2,
			local: []exEdit{
				{kind: exMoveFolder, from: "x", to: "x2"},
				{kind: exMakeFolder, to: "x"},
				{kind: exMoveFile, from: "p/f", to: "x/f"},
			},
			remote: []exEdit{{kind: exMakeFolder, to: "X2"}},
		},
	}
}

// exFreshNames are the names no synced item has that a side gives a file or
// a folder: OneDrive's add the names it takes for this computer's.
func exFreshNames(local, folder bool) []string {
	switch {
	case local && folder:
		return []string{"q"}
	case local:
		return []string{"g"}
	case folder:
		return []string{"Q", "q"}
	default:
		return []string{"G", "g"}
	}
}

// exEntry is one entry of the synced tree: content "" for a folder.
type exEntry struct {
	path    string
	content string
}

// exSyncedTree is what both sides hold, as synced, before any history.
func exSyncedTree() []exEntry {
	return []exEntry{{path: "p"}, {path: "p/f", content: "f"}, {path: "x"}, {path: "z", content: "z"}}
}

func exSyncedPaths() []string {
	var paths []string
	for _, entry := range exSyncedTree() {
		paths = append(paths, entry.path)
	}

	return paths
}

func exSyncedFolders() []string {
	var folders []string
	for _, entry := range exSyncedTree() {
		if entry.content == "" {
			folders = append(folders, entry.path)
		}
	}

	return folders
}

func exSideName(local bool) string {
	if local {
		return syncfixture.ModelLocalSide
	}

	return syncfixture.ModelRemoteSide
}

// exWorld is the planner model's three truths with a model of the executor
// and of OneDrive: an action whose precondition does not hold fails, as the
// executor's verification fails it, and OneDrive refuses a name a sibling
// already takes as it compares names.
type exWorld struct {
	*pmState
	logger *slog.Logger
	// baseRemote and baseLocal name each synced item of the tree by its
	// path, by item ID and by inode; baseContent holds the synced contents.
	baseRemote  map[string]string
	baseLocal   map[uint64]string
	baseContent map[string]bool
}

func newExWorld(t *testing.T, store *syncstore.SyncStore, start exStart, local, remote []exEdit) *exWorld {
	t.Helper()

	w := &exWorld{
		pmState:     newPMState(t),
		logger:      slog.New(slog.DiscardHandler),
		baseRemote:  map[string]string{},
		baseLocal:   map[uint64]string{},
		baseContent: map[string]bool{},
	}
	w.store = store
	for _, entry := range exSyncedTree() {
		w.addSynced(entry.path, entry.content == "", entry.content)
	}
	for p, item := range w.remote {
		w.baseRemote[item.id] = p
		if !item.folder {
			w.baseContent[item.content] = true
		}
	}
	for p, item := range w.local {
		w.baseLocal[item.inode] = p
	}
	for _, history := range [][]exEdit{start.local, local} {
		for _, e := range history {
			w.applyEdit(true, e)
		}
	}
	for _, history := range [][]exEdit{start.remote, remote} {
		for _, e := range history {
			w.applyEdit(false, e)
		}
	}

	return w
}

func (w *exWorld) side(local bool) map[string]pmItem {
	if local {
		return w.local
	}

	return w.remote
}

func (w *exWorld) newEntry(local, folder bool, content string) pmItem {
	if local {
		return pmItem{folder: folder, content: content, inode: uint64(w.next())}
	}

	return pmItem{id: "item-" + strconv.Itoa(w.next()), folder: folder, content: content, etag: w.next()}
}

// applyEdit makes one user action on one side.
func (w *exWorld) applyEdit(local bool, e exEdit) {
	side := w.side(local)
	w.logf("%s %s", exSideName(local), e)
	switch e.kind {
	case exMoveFile, exMoveFolder:
		moveTree(side, e.from, e.to)
		if !local {
			w.bumpRemote(e.to)
		}
	case exDeleteFile, exDeleteFolder:
		w.deleteTree(side, e.from)
	case exEditFile:
		item := side[e.from]
		item.content = exSideName(local) + " " + strconv.Itoa(w.next())
		if !local {
			item.etag = w.next()
		}
		side[e.from] = item
	case exCreateFile:
		side[e.to] = w.newEntry(local, false, exSideName(local)+" "+strconv.Itoa(w.next()))
	case exMakeFolder:
		side[e.to] = w.newEntry(local, true, "")
	}
}

// exEditsOn lists every user action on a side: each entry renamed to a fresh
// name, moved into each other folder under its own name, or moved onto each
// synced path that side no longer holds, and deleted; each file edited; a
// file made in each folder under a fresh name; a synced folder made again at
// its path.
func exEditsOn(side map[string]pmItem, local bool) []exEdit {
	dirs := pmFolders(side)
	var edits []exEdit
	for _, p := range sortedKeys(side) {
		folder := side[p].folder
		kind, deleteKind := exMoveFile, exDeleteFile
		if folder {
			kind, deleteKind = exMoveFolder, exDeleteFolder
		}
		for _, to := range exMoveTargets(side, local, p, dirs) {
			edits = append(edits, exEdit{kind: kind, from: p, to: to})
		}
		edits = append(edits, exEdit{kind: deleteKind, from: p})
		if !folder {
			edits = append(edits, exEdit{kind: exEditFile, from: p})
		}
	}
	for _, dir := range dirs {
		for _, name := range exFreshNames(local, false) {
			if to := path.Join(dir, name); exFree(side, local, to, "") {
				edits = append(edits, exEdit{kind: exCreateFile, to: to})
			}
		}
	}
	for _, folder := range exSyncedFolders() {
		if exFree(side, local, folder, "") {
			edits = append(edits, exEdit{kind: exMakeFolder, to: folder})
		}
	}

	return edits
}

// exMoveTargets lists where from may move on a side.
func exMoveTargets(side map[string]pmItem, local bool, from string, dirs []string) []string {
	dir, name := path.Dir(from), path.Base(from)
	if dir == "." {
		dir = ""
	}
	var candidates []string
	for _, fresh := range exFreshNames(local, side[from].folder) {
		candidates = append(candidates, path.Join(dir, fresh))
	}
	for _, other := range dirs {
		if other != dir {
			candidates = append(candidates, path.Join(other, name))
		}
	}
	candidates = append(candidates, exSyncedPaths()...)
	var targets []string
	for _, to := range candidates {
		parent := path.Dir(to)
		if parent == "." {
			parent = ""
		}
		if to == from || strings.HasPrefix(to, from+"/") || slices.Contains(targets, to) ||
			!slices.Contains(dirs, parent) || !exFree(side, local, to, from) {
			continue
		}
		targets = append(targets, to)
	}

	return targets
}

// exFree reports whether an entry can be put at p on a side: a free path
// here, and on OneDrive a name no sibling takes as OneDrive compares names,
// moving itself excepted.
func exFree(side map[string]pmItem, local bool, p, moving string) bool {
	if local {
		_, taken := side[p]
		return !taken
	}
	key := namekey.OfPath(p)
	for held := range side {
		if moving != "" && (held == moving || strings.HasPrefix(held, moving+"/")) {
			continue
		}
		if namekey.OfPath(held) == key {
			return false
		}
	}

	return true
}

// sideKey is the state of a side as far as planning goes: each entry, which
// synced item it is, if any, and what content it holds.
func (w *exWorld) sideKey(local bool) string {
	side := w.side(local)
	var b strings.Builder
	for _, p := range sortedKeys(side) {
		item := side[p]
		origin := w.baseRemote[item.id]
		if local {
			origin = w.baseLocal[item.inode]
		}
		content := item.content
		if !item.folder && !w.baseContent[content] {
			content = "written"
		}
		fmt.Fprintf(&b, "%s:%t:%s:%s;", p, item.folder, origin, content)
	}

	return b.String()
}

// exHistories lists every history of none, one or two user actions on one
// side after start, one for each state they leave the side in.
func exHistories(t *testing.T, start exStart, local bool) [][]exEdit {
	t.Helper()

	seen := map[string]bool{}
	var histories [][]exEdit
	visit := func(edits []exEdit) (*exWorld, bool) {
		var w *exWorld
		if local {
			w = newExWorld(t, nil, start, edits, nil)
		} else {
			w = newExWorld(t, nil, start, nil, edits)
		}
		key := w.sideKey(local)
		if seen[key] {
			return w, false
		}
		seen[key] = true
		histories = append(histories, edits)

		return w, true
	}
	// Breadth first: a state one action reaches is reached by one action.
	root, _ := visit(nil)
	type reached struct {
		edit exEdit
		w    *exWorld
	}
	var singles []reached
	for _, first := range exEditsOn(root.side(local), local) {
		if w, isNew := visit([]exEdit{first}); isNew {
			singles = append(singles, reached{edit: first, w: w})
		}
	}
	for _, single := range singles {
		for _, second := range exEditsOn(single.w.side(local), local) {
			visit([]exEdit{single.edit, second})
		}
	}

	return histories
}

// exConnected reports whether the user actions of a pair, its start's
// included, form one group of related paths. Each action is given as the name
// keys of the paths it acts on (exActionKeys).
func exConnected(groups ...[][]namekey.PathKey) bool {
	var actions [][]namekey.PathKey
	for _, group := range groups {
		actions = append(actions, group...)
	}
	parent := make([]int, len(actions))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}

		return parent[i]
	}
	groupCount := len(actions)
	for i := range actions {
		for j := i + 1; j < len(actions); j++ {
			if exRelated(actions[i], actions[j]) && find(i) != find(j) {
				parent[find(i)] = find(j)
				groupCount--
			}
		}
	}

	return groupCount <= 1
}

// exActionKeys gives each edit as the name keys of the paths it acts on.
func exActionKeys(edits []exEdit) [][]namekey.PathKey {
	keys := make([][]namekey.PathKey, 0, len(edits))
	for _, e := range edits {
		var paths []namekey.PathKey
		for _, p := range e.paths() {
			paths = append(paths, namekey.OfPath(p))
		}
		keys = append(keys, paths)
	}

	return keys
}

// exRelated reports whether two actions act on a path at, above or below
// each other's, as OneDrive compares names.
func exRelated(a, b []namekey.PathKey) bool {
	for _, ka := range a {
		for _, kb := range b {
			if ka.HasPrefix(kb) || kb.HasPrefix(ka) {
				return true
			}
		}
	}

	return false
}

// exPair is one pair of histories after a start.
type exPair struct {
	start  exStart
	local  []exEdit
	remote []exEdit
}

// id names the pair by its start and its user actions.
func (p *exPair) id() string {
	return fmt.Sprintf("%s L[%s] R[%s]", p.start.name, exHistoryString(p.local), exHistoryString(p.remote))
}

func exHistoryString(edits []exEdit) string {
	parts := make([]string, 0, len(edits))
	for _, e := range edits {
		parts = append(parts, e.String())
	}

	return strings.Join(parts, ", ")
}

// exPairs lists the pairs the check runs.
func exPairs(t *testing.T) []exPair {
	t.Helper()

	var pairs []exPair
	for _, start := range exStarts() {
		localHistories := exHistories(t, start, true)
		remoteHistories := exHistories(t, start, false)
		startKeys := exActionKeys(append(slices.Clone(start.local), start.remote...))
		remoteKeys := make([][][]namekey.PathKey, len(remoteHistories))
		for i, remote := range remoteHistories {
			remoteKeys[i] = exActionKeys(remote)
		}
		for _, local := range localHistories {
			localKeys := exActionKeys(local)
			for i, remote := range remoteHistories {
				if len(local)+len(remote) <= start.most && exConnected(startKeys, localKeys, remoteKeys[i]) {
					pairs = append(pairs, exPair{start: start, local: local, remote: remote})
				}
			}
		}
	}

	return pairs
}

// exRun is one pair run: its world and what the user's actions ask of sync.
type exRun struct {
	*exWorld
	// retired holds the synced contents the user removed from a side.
	retired map[string]bool
	// written holds the contents the user wrote that a side still holds.
	written []string
	// mustGo holds the synced items whose delete must happen.
	mustGo []string
	pass   int
	// refusal is the create the run's executor refuses once; the zero value
	// refuses nothing.
	refusal exRefusal
	// passes records what each pass planned, for the refusals of the pair.
	passes []exPassRecord
	// broken names the invariants the pair broke; failures says how.
	broken   []string
	failures []string
}

// exRefusal is a create that OneDrive or this computer refuses once: the
// action of that line fails the nth time a plan holds it, as a create the
// executor's call returns an error for, and everything that waits for it is
// skipped. Counted so, rather than by pass, a refusal names the same failure
// on a planner that takes another pass to reach the create.
type exRefusal struct {
	action string
	nth    int
}

// exPassRecord is what one pass of a run planned: its creates, by line, and
// whether it deletes anything.
type exPassRecord struct {
	creates []string
	deletes bool
}

func newExRun(t *testing.T, store *syncstore.SyncStore, pair *exPair) *exRun {
	t.Helper()

	r := &exRun{exWorld: newExWorld(t, store, pair.start, pair.local, pair.remote), retired: map[string]bool{}}
	local, remote := r.contents(true), r.contents(false)
	for content := range r.baseContent {
		if !local[content] || !remote[content] {
			r.retired[content] = true
		}
	}
	for _, held := range []map[string]bool{local, remote} {
		for content := range held {
			if !r.baseContent[content] {
				r.written = append(r.written, content)
			}
		}
	}
	sort.Strings(r.written)
	r.mustGo = r.untouchedDeletes()

	return r
}

// The invariants a pair can break, as the golden file names them.
const (
	exRemovedInAPass = "removed-in-a-pass"
	exNoQuietPlan    = "no-quiet-plan"
	exSidesDiffer    = "sides-differ"
	exLeftHeld       = "left-held"
	exWrittenLost    = "written-lost"
	exDeleteUndone   = "delete-undone"
	exDuplicated     = "duplicated"
)

func (r *exRun) failf(invariant, format string, args ...any) {
	if !slices.Contains(r.broken, invariant) {
		r.broken = append(r.broken, invariant)
	}
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// exConflictInfix marks a conflict copy's name in the model
// (conflictModelPath).
const exConflictInfix = ".conflict-"

// checkNoDuplicates fails the run when a side ends with one file content at
// two plain paths (filesByContent).
func (r *exRun) checkNoDuplicates() {
	for _, local := range []bool{true, false} {
		for content, paths := range r.filesByContent(local) {
			if len(paths) > 1 {
				r.failf(exDuplicated, "%s holds %q at %v", exSideName(local), content, paths)
			}
		}
	}
}

// filesByContent lists, per file content a side holds outside conflict
// copies, the paths it holds it at, sorted. A conflict copy keeps both on
// purpose, under a name that tells the user; the same content at two plain
// paths is a copy nobody asked for.
func (w *exWorld) filesByContent(local bool) map[string][]string {
	byContent := map[string][]string{}
	for _, p := range sortedKeys(w.side(local)) {
		if item := w.side(local)[p]; !item.folder && !strings.Contains(p, exConflictInfix) {
			byContent[item.content] = append(byContent[item.content], p)
		}
	}

	return byContent
}

// contents is the set of file contents a side holds.
func (w *exWorld) contents(local bool) map[string]bool {
	held := map[string]bool{}
	for _, item := range w.side(local) {
		if !item.folder {
			held[item.content] = true
		}
	}

	return held
}

// basePath is the synced path of the item at p on a side, "" for a new one.
func (w *exWorld) basePath(local bool, item pmItem) string {
	if local {
		return w.baseLocal[item.inode]
	}

	return w.baseRemote[item.id]
}

// untouchedDeletes lists the synced paths the user deleted on one side,
// making nothing there again, while the other side left the item, the folders
// above it and everything below it as they were synced. A synced file in a
// folder the user deleted on that side with it (another entry may have taken
// the folder's path), which the other side wrote into, is not listed: that folder is kept whole, as a conflict copy here or
// made again here as OneDrive keeps it, which is the data-safe outcome, and
// the files the user did not touch in it are kept with it.
func (r *exRun) untouchedDeletes() []string {
	var gone []string
	for _, local := range []bool{true, false} {
		present := map[string]bool{}
		presentKeys := map[namekey.PathKey]bool{}
		for p, item := range r.side(local) {
			present[r.basePath(local, item)] = true
			presentKeys[namekey.OfPath(p)] = true
		}
		for _, base := range exSyncedPaths() {
			if !present[base] && !presentKeys[namekey.OfPath(base)] && r.leftAsSynced(!local, base) &&
				!r.inAWrittenIntoDeletedFolder(local, base, present) {
				gone = append(gone, base)
			}
		}
	}

	return gone
}

// inAWrittenIntoDeletedFolder reports whether a synced folder above base is
// gone from the side local names (present holds the synced paths of the
// items it still has; another entry may hold the folder's path), while the
// other side did not leave the folder as it was synced.
func (r *exRun) inAWrittenIntoDeletedFolder(local bool, base string, present map[string]bool) bool {
	for dir := path.Dir(base); dir != "."; dir = path.Dir(dir) {
		if !present[dir] && !r.leftAsSynced(!local, dir) {
			return true
		}
	}

	return false
}

// leftAsSynced reports whether a side holds base, the folders above it and
// everything below it as they were synced.
func (r *exRun) leftAsSynced(local bool, base string) bool {
	side := r.side(local)
	for p, item := range side {
		related := p == base || strings.HasPrefix(p, base+"/") || strings.HasPrefix(base, p+"/")
		if related && !r.syncedAsIs(local, p, item) {
			return false
		}
	}
	for dir := path.Dir(base); dir != "."; dir = path.Dir(dir) {
		if _, ok := side[dir]; !ok {
			return false
		}
	}
	_, ok := side[base]

	return ok
}

func (r *exRun) syncedAsIs(local bool, p string, item pmItem) bool {
	return r.basePath(local, item) == p && (item.folder || r.baseContent[item.content])
}

// run plans and applies passes until a plan has nothing to do, renames on
// OneDrive what collides with an entry here, and checks the end.
func (r *exRun) run() {
	plan, quiet := r.syncUntilQuiet()
	for range exPassLimit {
		if !r.resolveCaseCollision() {
			break
		}
		plan, quiet = r.syncUntilQuiet()
	}
	if !quiet {
		r.failf(exNoQuietPlan, "no plan with nothing to do within %d passes", exPassLimit)
	}
	if local, remote := pmView(r.local), pmView(r.remote); !assert.ObjectsAreEqual(remote, local) {
		r.failf(exSidesDiffer, "the sides differ: here %v, OneDrive %v", local, remote)
	}
	if plan.FollowUp.Deferred != 0 || plan.FollowUp.Dropped != 0 {
		r.failf(exLeftHeld, "the last plan defers %d and drops %d", plan.FollowUp.Deferred, plan.FollowUp.Dropped)
	}
	local, remote := r.contents(true), r.contents(false)
	for _, content := range r.written {
		if !local[content] && !remote[content] {
			r.failf(exWrittenLost, "written %q is lost", content)
		}
	}
	r.checkNoDuplicates()
	for _, base := range r.mustGo {
		if r.stillHolds(true, base) || r.stillHolds(false, base) {
			r.failf(exDeleteUndone, "the delete of %s did not happen", base)
		}
	}
}

// stillHolds reports whether a side holds an entry at base, or, for a synced
// file, its synced content anywhere.
func (r *exRun) stillHolds(local bool, base string) bool {
	side := r.side(local)
	if _, ok := side[base]; ok {
		return true
	}
	for _, entry := range exSyncedTree() {
		if entry.path == base && entry.content != "" && r.contents(local)[entry.content] {
			return true
		}
	}

	return false
}

// syncUntilQuiet plans and applies passes until a plan has nothing to do, up
// to the pass limit, and returns the last plan. A run with a refusal has one
// pass more, the one the refusal costs.
func (r *exRun) syncUntilQuiet() (*syncdomain.ActionPlan, bool) {
	limit := exPassLimit
	if r.refusal.nth > 0 {
		limit++
	}
	for range limit {
		plan := r.planPass()
		if len(plan.Actions) == 0 && len(plan.HeldDeletes) == 0 {
			return plan, true
		}
		r.pass++
		r.logf("pass %d: %d actions", r.pass, len(plan.Actions))
		r.passes = append(r.passes, exRecordPass(plan))
		refuse := ""
		if r.refusal.nth > 0 && r.timesPlanned(r.refusal.action) == r.refusal.nth &&
			slices.Contains(r.passes[len(r.passes)-1].creates, r.refusal.action) {
			refuse = r.refusal.action
		}
		before := map[bool]map[string]bool{true: r.contents(true), false: r.contents(false)}
		local, remote, baseline := maps.Clone(r.local), maps.Clone(r.remote), maps.Clone(r.baseline)
		kept := r.applyPlan(plan, refuse)
		r.checkKept(before, kept)
		if refuse == "" && maps.Equal(local, r.local) && maps.Equal(remote, r.remote) && maps.Equal(baseline, r.baseline) {
			// Nothing ran: every later pass plans the same.
			return plan, false
		}
	}

	return r.planPass(), false
}

// exRecordPass is what a pass's plan holds for the refusals of its pair.
func exRecordPass(plan *syncdomain.ActionPlan) exPassRecord {
	var record exPassRecord
	for i := range plan.Actions {
		if _, creates := createSide(&plan.Actions[i]); creates {
			record.creates = append(record.creates, actionLine(&plan.Actions[i]))
		}
		record.deletes = record.deletes || isDeleteAction(&plan.Actions[i])
	}
	record.deletes = record.deletes || len(plan.HeldDeletes) > 0

	return record
}

// timesPlanned counts the passes so far whose plan holds the create action.
func (r *exRun) timesPlanned(action string) int {
	n := 0
	for i := range r.passes {
		if slices.Contains(r.passes[i].creates, action) {
			n++
		}
	}

	return n
}

// exRefusals lists the refusals a run's pair is run again with: when a pass
// of the run plans a delete, each create of each pass, refused once in that
// pass, which is the nth pass to plan it. A delete that waits for held work is
// planned by a later pass, so the refusals reach the pass that releases it
// and those around it.
func (r *exRun) exRefusals() []exRefusal {
	if !slices.ContainsFunc(r.passes, func(p exPassRecord) bool { return p.deletes }) {
		return nil
	}
	var refusals []exRefusal
	planned := map[string]int{}
	for _, pass := range r.passes {
		for _, create := range slices.Compact(slices.Sorted(slices.Values(pass.creates))) {
			planned[create]++
			refusals = append(refusals, exRefusal{action: create, nth: planned[create]})
		}
	}

	return refusals
}

// checkKept fails a pass that removed from a side the content of a file the
// user did not delete or overwrite.
func (r *exRun) checkKept(before map[bool]map[string]bool, kept map[string]bool) {
	for _, local := range []bool{true, false} {
		after := r.contents(local)
		for _, content := range sortedKeys(before[local]) {
			if after[content] || r.retired[content] || kept[content] {
				continue
			}
			r.failf(exRemovedInAPass, "pass %d removed %q from %s", r.pass, content, exSideName(local))
		}
	}
}

// resolveCaseCollision renames on OneDrive the first item whose name OneDrive
// takes for the name of a different entry here, as its user would, and
// reports whether it renamed one.
func (r *exRun) resolveCaseCollision() bool {
	for _, remotePath := range sortedKeys(r.remote) {
		for localPath := range r.local {
			if localPath == remotePath || namekey.OfPath(localPath) != namekey.OfPath(remotePath) {
				continue
			}
			to := r.freeRemoteName(path.Dir(remotePath))
			r.logf("remote renames %s to %s", remotePath, to)
			moveTree(r.remote, remotePath, to)
			r.bumpRemote(to)

			return true
		}
	}

	return false
}

func (r *exRun) freeRemoteName(dir string) string {
	for i := 1; ; i++ {
		to := path.Join(dir, exResolvedName+strconv.Itoa(i))
		if exFree(r.remote, false, to, "") {
			if _, here := r.local[to]; !here {
				return to
			}
		}
	}
}

// planPass writes the model into the store, with the case collisions a scan
// here reports, and plans it.
func (w *exWorld) planPass() *syncdomain.ActionPlan {
	w.writeStore()
	ctx := w.t.Context()
	db := w.store.RawDBForTest()
	_, err := db.ExecContext(ctx, "DELETE FROM observation_issues")
	require.NoError(w.t, err)
	for _, p := range w.localCaseCollisions() {
		_, err := db.ExecContext(ctx,
			"INSERT INTO observation_issues (path, issue_type, scope_key) VALUES (?, ?, '')", p, string(syncdomain.IssueCaseCollision))
		require.NoError(w.t, err)
	}

	return planCurrentStateForStoreWithLogger(w.t, w.store, syncdomain.SyncBidirectional, w.logger)
}

// localCaseCollisions lists the paths a full scan here skips as case
// collisions (DetectCaseCollisions): an entry that is new or changed since
// it was synced and whose name OneDrive takes for a synced sibling's still
// here, or for another such entry's, and what lies below it.
func (w *exWorld) localCaseCollisions() []string {
	var events []syncdomain.LocalEntry
	names := make(syncobserve.ScanNames)
	for _, p := range sortedKeys(w.local) {
		item := w.local[p]
		names.Add(p, path.Base(p))
		row, synced := w.baseline[p]
		if synced && row.folder == item.folder && row.inode == item.inode && (item.folder || row.localContent == item.content) {
			continue
		}
		events = append(events, syncdomain.LocalEntry{Path: p, ItemType: pmType(item.folder)})
	}
	_, skipped := syncobserve.DetectCaseCollisions(events, names)
	paths := make([]string, 0, len(skipped))
	for i := range skipped {
		paths = append(paths, skipped[i].Path)
	}
	sort.Strings(paths)

	return paths
}

// applyPlan runs a plan's actions in dependency order as the engine does: an
// action whose dependency did not succeed does not run. The first action of
// the line refuse fails without changing anything. It returns the OneDrive
// contents an upload replaced, which OneDrive keeps as versions.
func (w *exWorld) applyPlan(plan *syncdomain.ActionPlan, refuse string) map[string]bool {
	kept := map[string]bool{}
	failed := make([]bool, len(plan.Actions))
	for _, i := range exPlanOrder(plan) {
		a := &plan.Actions[i]
		if i < len(plan.Deps) && slices.ContainsFunc(plan.Deps[i], func(dep int) bool { return failed[dep] }) {
			failed[i] = true
			w.logf("  skipped %s", actionLine(a))

			continue
		}
		if refuse != "" && actionLine(a) == refuse {
			refuse = ""
			failed[i] = true
			w.logf("  refused %s", actionLine(a))

			continue
		}
		failed[i] = !w.applyAction(a, kept)
		if failed[i] {
			w.logf("  failed %s", actionLine(a))
		} else {
			w.logf("  %s", actionLine(a))
		}
	}
	for _, held := range plan.HeldDeletes {
		w.logf("  confirmed held %v delete %s", held.Side, held.Path)
		if held.Side == syncdomain.DeleteSafetySideLocal {
			w.deleteTree(w.local, held.Path)
		} else {
			w.deleteTree(w.remote, held.Path)
		}
		deleteSyncedTree(w.baseline, held.Path)
	}

	return kept
}

// exPlanOrder is the plan's actions in an order that respects its
// dependencies, as the engine reduces its ready frontier: the ready
// publication-only actions (a baseline update, a cleanup) first, committed
// through the store before any action of the frontier is dispatched, then
// the other ready actions, in plan order; actions caught in a cycle come
// last, in plan order.
func exPlanOrder(plan *syncdomain.ActionPlan) []int {
	done := make([]bool, len(plan.Actions))
	order := make([]int, 0, len(plan.Actions))
	ready := func(i int) bool {
		return !done[i] && (i >= len(plan.Deps) || !slices.ContainsFunc(plan.Deps[i], func(dep int) bool { return !done[dep] }))
	}
	for progressed := true; progressed; {
		progressed = false
		for drained := true; drained; {
			drained = false
			for i := range plan.Actions {
				if exPublicationOnly(&plan.Actions[i]) && ready(i) {
					done[i] = true
					order = append(order, i)
					drained = true
				}
			}
		}
		var frontier []int
		for i := range plan.Actions {
			if ready(i) {
				frontier = append(frontier, i)
			}
		}
		for _, i := range frontier {
			done[i] = true
			order = append(order, i)
			progressed = true
		}
	}
	for i := range plan.Actions {
		if !done[i] {
			order = append(order, i)
		}
	}

	return order
}

// exPublicationOnly reports an action the engine commits through the store
// rather than dispatching to a worker.
func exPublicationOnly(a *syncdomain.Action) bool {
	return a.Type == syncdomain.ActionBaselineUpdate || a.Type == syncdomain.ActionCleanup
}

// applyAction carries out one action and reports whether it succeeded.
func (w *exWorld) applyAction(a *syncdomain.Action, kept map[string]bool) bool {
	switch a.Type {
	case syncdomain.ActionDownload:
		return w.exDownload(a)
	case syncdomain.ActionUpload:
		return w.exUpload(a, kept)
	case syncdomain.ActionLocalDelete:
		return w.exLocalDelete(a)
	case syncdomain.ActionRemoteDelete:
		return w.exRemoteDelete(a)
	case syncdomain.ActionLocalMove:
		return w.exLocalMove(a)
	case syncdomain.ActionRemoteMove:
		return w.exRemoteMove(a)
	case syncdomain.ActionFolderCreate:
		if a.CreateSide == syncdomain.CreateRemote {
			return w.exRemoteFolderCreate(a)
		}

		return w.exLocalFolderCreate(a)
	case syncdomain.ActionConflictCopy:
		w.exConflictCopy(a, kept)

		return true
	case syncdomain.ActionBaselineUpdate:
		return w.exBaselineUpdate(a)
	case syncdomain.ActionCleanup:
		delete(w.baseline, a.Path)

		return true
	}

	return false
}

// exConflictCopy renames the entry here, a file or a folder, aside, keeping
// its contents; a source that is gone leaves nothing to keep
// (ExecuteConflictCopy).
func (w *exWorld) exConflictCopy(a *syncdomain.Action, kept map[string]bool) {
	if _, ok := w.local[a.Path]; !ok {
		return
	}
	copyPath := conflictModelPath(a.Path, w.next())
	for p, item := range w.local {
		if p == a.Path || strings.HasPrefix(p, a.Path+"/") {
			kept[item.content] = true
		}
	}
	moveTree(w.local, a.Path, copyPath)
}

// exDownload writes the file here; the executor refuses to replace an entry
// that is not the synced version of the path.
func (w *exWorld) exDownload(a *syncdomain.Action) bool {
	_, src, ok := w.remoteItem(a)
	if !ok || src.folder || !w.localParentPresent(a.Path) || !w.localReplaceable(a.Path) {
		return false
	}
	inode := uint64(w.next())
	w.local[a.Path] = pmItem{content: src.content, inode: inode}
	w.recordSynced(a.Path, pmSynced{id: src.id, localContent: src.content, remoteContent: src.content, inode: inode, etag: src.etag})

	return true
}

// moveSynced re-paths the moved item's baseline row and its subtree, as the
// executor's commit does (commitMove): from where the baseline has the item,
// looked up by its ID, not from the action's OldPath, which is the moving
// side's path and differs from it where the two sides diverged.
func (w *exWorld) moveSynced(a *syncdomain.Action) {
	from := a.OldPath
	if a.View != nil && a.View.Baseline != nil && a.View.Baseline.ItemID != "" {
		for held, synced := range w.baseline {
			if synced.id == a.View.Baseline.ItemID {
				from = held
			}
		}
	}
	moveTree(w.baseline, from, a.Path)
}

// recordSynced records the item synced at p, as the executor's commit does:
// the baseline keys a row by item ID, so a row the item had at another path
// goes.
func (w *exWorld) recordSynced(p string, row pmSynced) {
	for held, synced := range w.baseline {
		if held != p && synced.id == row.id {
			delete(w.baseline, held)
		}
	}
	w.baseline[p] = row
}

func (w *exWorld) localReplaceable(p string) bool {
	item, taken := w.local[p]
	if !taken {
		return true
	}
	row, synced := w.baseline[p]

	return synced && !item.folder && !row.folder && row.localContent == item.content
}

func (w *exWorld) localParentPresent(p string) bool {
	dir := path.Dir(p)
	if dir == "." {
		return true
	}
	parent, ok := w.local[dir]

	return ok && parent.folder
}

// exUpload overwrites the item by ID wherever it is, or makes a new item at
// the path; a new item never replaces a name a sibling takes.
func (w *exWorld) exUpload(a *syncdomain.Action, kept map[string]bool) bool {
	src, ok := w.local[a.Path]
	if !ok || src.folder {
		return false
	}
	at, existing, found := w.remoteItem(a)
	id := existing.id
	if a.ItemID == "" || !found {
		parent, parentOK := w.remoteFolderAt(path.Dir(a.Path))
		if !parentOK {
			return false
		}
		at = path.Join(parent, path.Base(a.Path))
		if !exFree(w.remote, false, at, "") {
			return false
		}
		id = "item-" + strconv.Itoa(w.next())
	} else if existing.content != src.content {
		kept[existing.content] = true
	}
	etag := w.next()
	w.remote[at] = pmItem{id: id, content: src.content, etag: etag}
	w.recordSynced(a.Path, pmSynced{id: id, localContent: src.content, remoteContent: src.content, inode: src.inode, etag: etag})

	return true
}

// remoteFolderAt finds the folder at dir on OneDrive, which resolves a path as
// it compares names.
func (w *exWorld) remoteFolderAt(dir string) (string, bool) {
	if dir == "." || dir == "" {
		return "", true
	}
	if item, ok := w.remote[dir]; ok {
		return dir, item.folder
	}
	key := namekey.OfPath(dir)
	for held, item := range w.remote {
		if namekey.OfPath(held) == key {
			return held, item.folder
		}
	}

	return "", false
}

// exLocalDelete removes a file only while it holds the synced content, and a
// folder only when it is empty (DeleteLocalFolder).
func (w *exWorld) exLocalDelete(a *syncdomain.Action) bool {
	item, ok := w.local[a.Path]
	if !ok {
		deleteSyncedTree(w.baseline, a.Path)

		return true
	}
	if item.folder {
		for p := range w.local {
			if strings.HasPrefix(p, a.Path+"/") {
				return false
			}
		}
	} else if row, synced := w.plannedSynced(a); !synced || row.localContent != item.content {
		return false
	}
	delete(w.local, a.Path)
	deleteSyncedTree(w.baseline, a.Path)

	return true
}

// plannedSynced is the baseline row of the item a delete was planned
// against: the row at its path, or, when an earlier action of the pass
// committed the item at another path (a download of OneDrive's move of it,
// which records the row by item ID), that row. The executor verifies the
// file against the baseline the plan carries, not the row's current path.
func (w *exWorld) plannedSynced(a *syncdomain.Action) (pmSynced, bool) {
	if row, synced := w.baseline[a.Path]; synced {
		return row, true
	}
	if a.View == nil || a.View.Baseline == nil || a.View.Baseline.ItemID == "" {
		return pmSynced{}, false
	}
	for _, row := range w.baseline {
		if row.id == a.View.Baseline.ItemID {
			return row, true
		}
	}

	return pmSynced{}, false
}

// exRemoteDelete deletes the item and everything below it on OneDrive, as
// OneDrive does, after the executor's checks (ExecuteRemoteDelete): the item
// is still there, nothing of the same kind is at its path here again, and a
// folder holds only synced items, its files as they were synced.
func (w *exWorld) exRemoteDelete(a *syncdomain.Action) bool {
	at, item, found := w.remoteItem(a)
	if !found {
		return false
	}
	if here, ok := w.local[a.Path]; ok && here.folder == item.folder {
		return false
	}
	if item.folder && !w.remoteHoldsOnlySynced(at) {
		return false
	}
	w.deleteTree(w.remote, at)
	deleteSyncedTree(w.baseline, a.Path)

	return true
}

func (w *exWorld) remoteHoldsOnlySynced(folder string) bool {
	synced := make(map[string]pmSynced, len(w.baseline))
	for _, row := range w.baseline {
		synced[row.id] = row
	}
	for p, item := range w.remote {
		if !strings.HasPrefix(p, folder+"/") {
			continue
		}
		row, ok := synced[item.id]
		if !ok || (!item.folder && (row.remoteContent != item.content || row.etag != item.etag)) {
			return false
		}
	}

	return true
}

func (w *exWorld) exLocalMove(a *syncdomain.Action) bool {
	if _, ok := w.local[a.OldPath]; !ok || !w.localParentPresent(a.Path) || strings.HasPrefix(a.Path, a.OldPath+"/") {
		return false
	}
	if _, taken := w.local[a.Path]; taken {
		if !a.ReplaceLocalTarget || !w.localReplaceable(a.Path) {
			return false
		}
		delete(w.local, a.Path)
	}
	moveTree(w.local, a.OldPath, a.Path)
	w.moveSynced(a)

	return true
}

func (w *exWorld) exRemoteMove(a *syncdomain.Action) bool {
	at, _, found := w.remoteItem(a)
	parent, parentOK := w.remoteFolderAt(path.Dir(a.Path))
	if !found || !parentOK {
		return false
	}
	to := path.Join(parent, path.Base(a.Path))
	if strings.HasPrefix(to, at+"/") || !exFree(w.remote, false, to, at) {
		return false
	}
	moveTree(w.remote, at, to)
	w.bumpRemote(to)
	w.moveSynced(a)
	if row, ok := w.baseline[a.Path]; ok {
		row.etag = w.remote[to].etag
		w.baseline[a.Path] = row
	}

	return true
}

func (w *exWorld) exRemoteFolderCreate(a *syncdomain.Action) bool {
	local, localOK := w.local[a.Path]
	if !localOK || !local.folder {
		return false
	}
	// A create planned without OneDrive truth at its path finds OneDrive
	// holding one there, and the worker's freshness check supersedes it
	// before it reaches OneDrive (evaluateRemoteActionFreshness).
	if _, held := w.remote[a.Path]; held && (a.View == nil || a.View.Remote == nil || a.View.Remote.IsDeleted) {
		return false
	}
	// The executor creates the folder by its parent and name and adopts a
	// folder OneDrive already has there (createRemoteFolder): the item ID the
	// action carries from the baseline names nothing it creates.
	parent, parentOK := w.remoteFolderAt(path.Dir(a.Path))
	if !parentOK {
		return false
	}
	at, found := w.remoteEntryAt(path.Join(parent, path.Base(a.Path)))
	remote := w.remote[at]
	if found && !remote.folder {
		return false
	}
	if !found {
		remote = pmItem{id: "item-" + strconv.Itoa(w.next()), folder: true, etag: w.next()}
		w.remote[at] = remote
	}
	w.baseline[a.Path] = pmSynced{id: remote.id, folder: true, inode: local.inode, etag: remote.etag}

	return true
}

// remoteEntryAt finds the entry OneDrive holds at p, as it compares names.
func (w *exWorld) remoteEntryAt(p string) (string, bool) {
	if _, ok := w.remote[p]; ok {
		return p, true
	}
	key := namekey.OfPath(p)
	for held := range w.remote {
		if namekey.OfPath(held) == key {
			return held, true
		}
	}

	return p, false
}

// exLocalFolderCreate makes the folder here, with any folder above it that is
// missing (createLocalFolder).
func (w *exWorld) exLocalFolderCreate(a *syncdomain.Action) bool {
	_, remote, found := w.remoteItem(a)
	if !found || !remote.folder {
		return false
	}
	for dir := path.Dir(a.Path); dir != "."; dir = path.Dir(dir) {
		if parent, ok := w.local[dir]; ok && !parent.folder {
			return false
		}
	}
	local, localOK := w.local[a.Path]
	if localOK && !local.folder {
		return false
	}
	for dir := path.Dir(a.Path); dir != "."; dir = path.Dir(dir) {
		if _, ok := w.local[dir]; !ok {
			w.local[dir] = pmItem{folder: true, inode: uint64(w.next())}
		}
	}
	if !localOK {
		local = pmItem{folder: true, inode: uint64(w.next())}
		w.local[a.Path] = local
	}
	w.baseline[a.Path] = pmSynced{id: remote.id, folder: true, inode: local.inode, etag: remote.etag}

	return true
}

func (w *exWorld) exBaselineUpdate(a *syncdomain.Action) bool {
	if a.OldPath != "" {
		w.moveSynced(a)

		return true
	}
	remote, remoteOK := w.remote[a.Path]
	local, localOK := w.local[a.Path]
	if !remoteOK || !localOK {
		return false
	}
	// The store keys the row by item ID: a row the item had at another
	// path goes (recordSynced).
	w.recordSynced(a.Path, pmSynced{
		id: remote.id, folder: remote.folder, localContent: local.content, remoteContent: remote.content,
		inode: local.inode, etag: remote.etag,
	})

	return true
}

// exQuietStore is a test store that logs nothing and keeps its prepared
// statements (exCachingDriver): the check plans thousands of times.
func exQuietStore(t *testing.T) *syncstore.SyncStore {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "exhaustive.db")
	require.NoError(t, os.WriteFile(dbPath, syncstoretest.LoadTestStoreTemplate(t), 0o600))
	store, err := syncstore.OpenSyncStoreWithHooksForTest(t.Context(), dbPath, slog.New(slog.DiscardHandler), false, syncstore.Hooks{Driver: exDriverName, SkipFsync: true})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, store.Close(context.Background()))
	})
	// A plan fills the planner-visible tables within its transaction and
	// rolls it back. Made once on the store's one connection, they stay, and
	// so do the statements that read them.
	for _, ddl := range []string{syncstore.SQLCreatePlannerVisibleLocalStateForTest(), syncstore.SQLCreatePlannerVisibleRemoteStateForTest()} {
		_, err := store.RawDBForTest().ExecContext(t.Context(), ddl)
		require.NoError(t, err)
	}

	return store
}

const exDriverName = "sqlite-exhaustive"

// registerExCachingDriver registers exCachingDriver once per test binary.
// Parallel tests call it together, and sql.Register panics on a second
// registration of one name, so a check of sql.Drivers before registering
// raced: the registration runs under exDriverRegistration, once.
func registerExCachingDriver(t *testing.T) {
	t.Helper()

	require.NoError(t, exDriverRegistration())
}

// exDriverRegistration registers exCachingDriver over SQLite's driver the
// first time it is called and reports that first call's error on every call.
//
//nolint:gochecknoglobals // sql.Register is a process-wide registry, so its guard has no narrower owner than the package.
var exDriverRegistration = sync.OnceValue(func() error {
	db, err := sql.Open(syncstore.SqliteDriverName, ":memory:")
	if err != nil {
		return fmt.Errorf("open sqlite driver: %w", err)
	}
	base := db.Driver()
	if err := db.Close(); err != nil {
		return fmt.Errorf("close sqlite driver probe: %w", err)
	}
	sql.Register(exDriverName, exCachingDriver{base: base})

	return nil
})

// exParallelRegistrations is how many goroutines register the driver at once
// in TestRegisterExCachingDriver_ParallelCallsRegisterItOnce.
const exParallelRegistrations = 16

// Parallel tests register the caching driver at the same moment: every call
// returns without error, none panics on a second registration, and the
// driver is registered once. Run under -race, it also proves the calls share
// no unguarded state.
func TestRegisterExCachingDriver_ParallelCallsRegisterItOnce(t *testing.T) {
	t.Parallel()

	start := make(chan struct{})
	errs := make([]error, exParallelRegistrations)
	var wg sync.WaitGroup
	for i := range exParallelRegistrations {
		wg.Go(func() {
			<-start
			errs[i] = exDriverRegistration()
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "registration %d", i)
	}
	registered := 0
	for _, name := range sql.Drivers() {
		if name == exDriverName {
			registered++
		}
	}
	assert.Equal(t, 1, registered)
	registerExCachingDriver(t)
}

// exCachingDriver is SQLite's driver keeping one prepared statement per
// query text and connection, where SQLite's prepares a statement for every
// query: preparing the store's and the planner's statements was most of what
// a plan cost. A statement whose rows are still being read is not shared: a
// use of its query meanwhile gets a statement of its own.
type exCachingDriver struct{ base driver.Driver }

func (d exCachingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.base.Open(name)
	if err != nil {
		return nil, fmt.Errorf("opening the store: %w", err)
	}
	prepare, preparesWithContext := conn.(driver.ConnPrepareContext)
	begin, beginsWithContext := conn.(driver.ConnBeginTx)
	if !preparesWithContext || !beginsWithContext {
		return nil, errors.Join(errors.New("the store's driver does not take a context"), conn.Close())
	}

	return &exCachingConn{Conn: conn, prepare: prepare, begin: begin, stmts: map[string]*exCachedStmt{}}, nil
}

type exCachedStmt struct {
	stmt  driver.Stmt
	exec  driver.StmtExecContext
	query driver.StmtQueryContext
	busy  bool
}

type exCachingConn struct {
	driver.Conn
	prepare driver.ConnPrepareContext
	begin   driver.ConnBeginTx
	stmts   map[string]*exCachedStmt
}

// statement returns query's statement and whether it is the shared one.
func (c *exCachingConn) statement(ctx context.Context, query string) (*exCachedStmt, bool, error) {
	cached, ok := c.stmts[query]
	if ok && !cached.busy {
		return cached, true, nil
	}
	stmt, err := c.PrepareContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	exec, executes := stmt.(driver.StmtExecContext)
	queries, queriesWithContext := stmt.(driver.StmtQueryContext)
	if !executes || !queriesWithContext {
		return nil, false, errors.Join(errors.New("the store's statements do not take a context"), stmt.Close())
	}
	fresh := &exCachedStmt{stmt: stmt, exec: exec, query: queries}
	if ok {
		return fresh, false, nil
	}
	c.stmts[query] = fresh

	return fresh, true, nil
}

func (c *exCachingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.prepare.PrepareContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("preparing: %w", err)
	}

	return stmt, nil
}

func (c *exCachingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.begin.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("beginning: %w", err)
	}

	return tx, nil
}

func (c *exCachingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	cached, shared, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	result, err := cached.exec.ExecContext(ctx, args)
	if !shared {
		err = errors.Join(err, cached.stmt.Close())
	}
	if err != nil {
		return nil, fmt.Errorf("executing: %w", err)
	}

	return result, nil
}

func (c *exCachingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	cached, shared, err := c.statement(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := cached.query.QueryContext(ctx, args)
	if err != nil {
		if !shared {
			err = errors.Join(err, cached.stmt.Close())
		}

		return nil, fmt.Errorf("querying: %w", err)
	}
	cached.busy = shared

	return &exCachedRows{Rows: rows, cached: cached, shared: shared}, nil
}

func (c *exCachingConn) ResetSession(ctx context.Context) error {
	resetter, ok := c.Conn.(driver.SessionResetter)
	if !ok {
		return nil
	}
	if err := resetter.ResetSession(ctx); err != nil {
		return fmt.Errorf("resetting: %w", err)
	}

	return nil
}

func (c *exCachingConn) Close() error {
	var errs []error
	for _, cached := range c.stmts {
		errs = append(errs, cached.stmt.Close())
	}
	errs = append(errs, c.Conn.Close())
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("closing: %w", err)
	}

	return nil
}

type exCachedRows struct {
	driver.Rows
	cached *exCachedStmt
	shared bool
}

func (r *exCachedRows) Close() error {
	err := r.Rows.Close()
	if r.shared {
		r.cached.busy = false
	} else {
		err = errors.Join(err, r.cached.stmt.Close())
	}
	if err != nil {
		return fmt.Errorf("closing rows: %w", err)
	}

	return nil
}

// exResult is what one run found: a pair's, or a pair's with a refusal, named
// by id.
type exResult struct {
	id     string
	broken []string
	detail string
}

// String names the refusal in a run's id.
func (f exRefusal) String() string {
	return fmt.Sprintf("refuse %s #%d", f.action, f.nth)
}

func (r *exRun) result(id string) exResult {
	return exResult{
		id: id, broken: r.broken,
		detail: strings.Join(r.failures, "; ") + "\n    " + strings.Join(r.log, "\n    "),
	}
}

// Validates: R-2.3.1, R-2.17.1, R-2.17.2, R-2.17.6
func TestPlannerExhaustive_ShortHistoriesOnBothSidesKeepContentAndConverge(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	registerExCachingDriver(t)
	stride := 1
	switch {
	case *updateGolden:
	case !syncfixture.FullTier():
		stride = exSampleStride
	case syncfixture.RaceDetectorEnabled:
		stride = exRaceStride
	}
	requireExhaustiveGolden(t, exRunPairs(t, exPairs(t), stride), stride == 1)
}

// Validates: R-2.17.6, R-2.1.7
//
// From the held start, OneDrive renamed the held folder away, which frees
// its name for the folder made here in its place, and that folder's create
// on OneDrive is refused once. The local move that follows OneDrive's
// rename records the item's baseline row where the item went, as the store
// does by item ID, so the passes after the refusal make the folder and its
// file and the plan empties, on every run with each create refused once.
func TestPlannerPairs_AFolderCreateRefusedAfterTheHeldFolderMovesAwayConverges(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "held"),
		remote: []exEdit{
			{kind: exDeleteFolder, from: "p"},
			{kind: exMoveFolder, from: "x", to: "q"},
		},
	})
}

// Validates: R-2.17.6, R-2.1.7
//
// The same rename on both sides, with a folder made here at the old path:
// the re-key of the renamed folder's baseline row is publication-only, so
// it is committed before the folder create is dispatched, and the create
// then records the new folder at the path the row left. The passes make
// the new folder on OneDrive and the plan empties.
func TestPlannerPairs_AFolderRenamedTheSameOnBothSidesWithANewFolderAtItsOldPathConverges(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "synced"),
		local: []exEdit{
			{kind: exMoveFolder, from: "p", to: "q"},
			{kind: exMakeFolder, to: "p"},
		},
		remote: []exEdit{{kind: exMoveFolder, from: "p", to: "q"}},
	})
}

// Validates: R-2.17.6
//
// From the held start, here p renamed q and the held folder moved into it as
// q/x2: once the case collision of x2 is resolved, f, which left p for the
// new x, moves after p's move, so x's move into q, the new x and f's move all
// run, and the plan empties.
func TestPlannerPairs_AHeldFolderMovedIntoARenamedFolderRunsOnceItsNameIsFree(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "held"),
		local: []exEdit{
			{kind: exMoveFolder, from: "p", to: "q"},
			{kind: exMoveFolder, from: "x2", to: "q/x2"},
		},
	})
}

// Validates: R-2.17.9, R-2.17.2
//
// OneDrive deleted a folder and moved a file onto its path: z onto p, which
// held the synced f, or onto the empty x, with this computer leaving the
// folder as synced or deleting f in it. The plan deletes the folder here and
// the move of z onto its path waits for that delete, so z ends once on each
// side, on every run with each create refused once. Before, the folder made
// the move wait, and z was downloaded at the folder's path while its copy
// here went back to OneDrive as a new file.
func TestPlannerPairs_AFileMovedOnOneDriveOntoAFolderItDeletedEndsOnce(t *testing.T) {
	t.Parallel()

	// Registered before the subtests run in parallel: registering checks,
	// then registers, and two at once would both register.
	registerExCachingDriver(t)
	for _, tc := range []struct {
		name   string
		folder string
		local  []exEdit
	}{
		{name: "onto a folder with a file", folder: "p"},
		{name: "onto an empty folder", folder: "x"},
		{name: "onto a folder emptied here", folder: "p", local: []exEdit{{kind: exDeleteFile, from: "p/f"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requirePairKeepsEveryInvariant(t, &exPair{
				start: exStartNamed(t, "synced"),
				local: tc.local,
				remote: []exEdit{
					{kind: exDeleteFolder, from: tc.folder},
					{kind: exMoveFile, from: "z", to: tc.folder},
				},
			})
		})
	}
}

// Validates: R-2.17.6, R-2.1.7
//
// Here p moved aside, a new p made, x moved into it as p/x, a new p/x/a made
// and the old p moved into it as p/x/a/p, while OneDrive kept both: x's
// OneDrive move lands in the new p, at the old p's path there, and the old
// p's target lies below x's. The old p moves aside on OneDrive first, the new
// p is made there and x moves into it, and a later pass makes p/x/a and moves
// the old p into it, on every run with each create refused once. Before,
// x's move was taken back while the old p held the path, and the old p
// waited for x: nothing moved, ever.
func TestPlannerPairs_AFolderMovedIntoANewFolderAtThePathOfAFolderMovedBelowItConverges(t *testing.T) {
	t.Parallel()

	requirePairKeepsEveryInvariant(t, &exPair{
		start: exStartNamed(t, "synced"),
		local: []exEdit{
			{kind: exMoveFolder, from: "p", to: "q"},
			{kind: exMakeFolder, to: "p"},
			{kind: exMoveFolder, from: "x", to: "p/x"},
			{kind: exMakeFolder, to: "p/x/a"},
			{kind: exMoveFolder, from: "q", to: "p/x/a/p"},
		},
	})
}

// Validates: R-2.17.6, R-2.1.7
//
// p moved into a new folder at its own path on each side, into p/a/p here
// and into p/b/p on OneDrive: OneDrive's place wins, so p moves to p/b/p here
// and the two new folders p are one folder by path. The folder made here is
// planned at the path p left with OneDrive's own new p as its OneDrive truth,
// so the freshness check, which supersedes a create planned without OneDrive
// truth at a path OneDrive holds, lets it through and it adopts that folder;
// p/a is then made on OneDrive and p/b here. Before, the create was planned
// without OneDrive truth and superseded on every pass, and p's move, waiting
// for p/b, never came.
func TestPlannerPairs_AFolderMovedIntoDifferentNewFoldersAtItsOwnPathOnEachSideConverges(t *testing.T) {
	t.Parallel()

	moveInto := func(newFolder string) []exEdit {
		return []exEdit{
			{kind: exMoveFolder, from: "p", to: "q"},
			{kind: exMakeFolder, to: "p"},
			{kind: exMakeFolder, to: "p/" + newFolder},
			{kind: exMoveFolder, from: "q", to: "p/" + newFolder + "/p"},
		}
	}
	requirePairKeepsEveryInvariant(t, &exPair{start: exStartNamed(t, "synced"), local: moveInto("a"), remote: moveInto("b")})
}

// Validates: R-2.17.6
//
// From the held start, a folder sub made here in the new x and f moved into
// it: OneDrive still keeps the old x at x, so sub is not made there until the
// old x has moved to x2, and no pass carries sub along with the old x and
// moves it back, on every run with each create refused once.
func TestPlannerPairs_AFolderMadeInTheHeldFoldersPlaceWaitsForItToLeave(t *testing.T) {
	t.Parallel()

	registerExCachingDriver(t)
	store := exQuietStore(t)
	pair := &exPair{start: exStartNamed(t, "held"), local: []exEdit{
		{kind: exMakeFolder, to: "x/sub"},
		{kind: exMoveFile, from: "x/f", to: "x/sub/f"},
	}}
	r := newExRun(t, store, pair)
	r.run()
	runs := []*exRun{r}
	for _, refusal := range r.exRefusals() {
		refused := newExRun(t, store, pair)
		refused.refusal = refusal
		refused.run()
		runs = append(runs, refused)
	}
	for _, run := range runs {
		assert.Empty(t, run.broken, "%s", run.result(pair.id()).detail)
		for _, line := range run.log {
			assert.NotContains(t, line, "<- x2/sub", "sub was carried with the old x: %s", run.result(pair.id()).detail)
		}
	}
}

// exRunPairs runs every stride-th pair, and that pair again with each of its
// refusals (exRefusals), on exWorkers stores in parallel. It returns what
// each run found, in pair order, a pair before its refusals.
func exRunPairs(t *testing.T, pairs []exPair, stride int) []exResult {
	t.Helper()

	results := make([][]exResult, len(pairs))
	t.Run("pairs", func(t *testing.T) {
		for worker := range exWorkers {
			t.Run("worker="+strconv.Itoa(worker), func(t *testing.T) {
				t.Parallel()

				store := exQuietStore(t)
				for i := worker * stride; i < len(pairs); i += exWorkers * stride {
					pair := &pairs[i]
					r := newExRun(t, store, pair)
					r.run()
					results[i] = append(results[i], r.result(pair.id()))
					for _, refusal := range r.exRefusals() {
						refused := newExRun(t, store, pair)
						refused.refusal = refusal
						refused.run()
						results[i] = append(results[i], refused.result(pair.id()+" "+refusal.String()))
					}
				}
			})
		}
	})

	return slices.Concat(results...)
}

// requireExhaustiveGolden compares the runs that broke an invariant, and
// which, with the checked-in list (exhaustiveGolden): a run that breaks one
// the list does not name, or no longer breaks one it names, fails the test,
// logged with what the run did. A run of only some pairs compares those; a
// complete one also fails on a listed run it no longer makes.
func requireExhaustiveGolden(t *testing.T, results []exResult, complete bool) {
	t.Helper()

	var broken []string
	byID := map[string]*exResult{}
	for i := range results {
		byID[results[i].id] = &results[i]
		if len(results[i].broken) > 0 {
			broken = append(broken, results[i].id+" :: "+strings.Join(results[i].broken, " "))
		}
	}
	if *updateGolden {
		requireGolden(t, exhaustiveGolden, strings.Join(broken, "\n")+"\n")
		return
	}
	golden, err := os.ReadFile(exhaustiveGolden)
	require.NoError(t, err, "golden file missing; run with -update")
	var want, gone []string
	for line := range strings.Lines(string(golden)) {
		line = strings.TrimSuffix(line, "\n")
		id, _, _ := strings.Cut(line, " :: ")
		switch {
		case byID[id] != nil:
			want = append(want, line)
		case complete:
			gone = append(gone, line)
		}
	}
	assert.Empty(t, gone, "listed runs the pairs no longer make; rerun with -update")
	if assert.Equal(t, want, broken, "runs that break an invariant changed; check the diff, then rerun with -update") {
		return
	}
	for _, line := range broken {
		if !slices.Contains(want, line) {
			id, _, _ := strings.Cut(line, " :: ")
			t.Logf("%s: %s", id, byID[id].detail)
		}
	}
}
