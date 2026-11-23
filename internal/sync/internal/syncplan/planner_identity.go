package syncplan

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// The identity-keyed planner reconciles three trees whose nodes are keyed by
// stable identity, where the current planner joins the three sides on path
// and recovers moves afterwards by pairing a delete with a create. It is a
// pure function over the rows the current planner reads, and it is not
// wired into any run: it exists to be compared with the current planner
// (matrix.identity.golden, and the plan diff the models run) until its plans
// are understood well enough to decide whether it should replace it.
//
// A node is one item:
//   - the baseline row and OneDrive's row join on the item ID;
//   - the baseline row and the local row join on the local identity (device
//     and inode), with a confidence: strong when both carry the same non-zero
//     birth time, weak when either birth time is unknown and the type is the
//     same. An identity seen at two local paths (a hard link) or with two
//     different birth times (a reused inode) has no confidence, and the row
//     falls back to path pairing with the baseline row at its path;
//   - a row that joins no baseline row is a new item.
//
// An entry whose type differs from its baseline row's is a new item at the
// same position: the baseline item is gone on that side.
//
// Each node is decided on its own: where it ends up (a move on either side
// is an attribute of the node, so a move and an edit compose), then what its
// content needs, by the decision table the current planner uses too
// (syncdomain.Decide). A second step settles position conflicts, two nodes that end
// at the same path.

// IdentityInputs is what the identity-keyed planner reads: the baseline and
// the planner-visible local and remote rows of one snapshot.
type IdentityInputs struct {
	Baseline *syncdomain.BaselineSnapshot
	Local    []syncdomain.LocalStateRow
	Remote   []syncdomain.RemoteStateRow
	DriveID  driveid.ID
	// ObservationIssues are the pass's observation issues. The planner does
	// not read them; ExplainPlanDifferences does, because the current
	// planner suppresses work where truth is unavailable.
	ObservationIssues []syncdomain.ObservationIssueRow
}

// identityConfidence is how sure the local join is that a local row is the
// baseline's item.
type identityConfidence int

const (
	// identityByPath: no usable identity; the row joined by path.
	identityByPath identityConfidence = iota
	// identityWeak: same device and inode and type, birth time unknown.
	identityWeak
	// identityStrong: same device, inode and non-zero birth time.
	identityStrong
)

func (c identityConfidence) String() string {
	switch c {
	case identityByPath:
		return "path"
	case identityWeak:
		return "weak"
	case identityStrong:
		return "strong"
	}

	return fmt.Sprintf("confidence(%d)", int(c))
}

type localIdentityKey struct {
	device uint64
	inode  uint64
}

// identityNode is one item of the three trees.
type identityNode struct {
	base   *syncdomain.BaselineEntry
	local  *syncdomain.LocalStateRow
	remote *syncdomain.RemoteStateRow
	// confidence is the local join's (identityByPath for a node with no
	// baseline row or no local row).
	confidence identityConfidence
	// remoteReplaced: OneDrive's row joined by the baseline path, not the
	// item ID; OneDrive deleted the item and has a new one in its place.
	remoteReplaced bool
}

// identityOutcome is a node's decision: its actions in order (each depends
// on the one before it), and the path its content ends at with the side
// whose content that is, for the position step.
type identityOutcome struct {
	node    *identityNode
	actions []syncdomain.Action
	// claims is the path the node's content ends at, "" when it ends
	// nowhere (a delete, a cleanup, quiet).
	claims string
	// localContent and remoteContent say whose content survives at claims.
	localContent  bool
	remoteContent bool
	// retires is set when the node's only action is a cleanup of its
	// baseline row.
	retires bool
}

// PlanIdentityKeyed plans in from identity-keyed nodes. It is pure and
// deterministic: the actions come out ordered by the paths they act on.
func PlanIdentityKeyed(in *IdentityInputs) (*syncdomain.ActionPlan, error) {
	if in == nil || in.Baseline == nil {
		return nil, errors.New("sync: identity planner needs a baseline snapshot")
	}

	nodes := joinIdentityNodes(in)
	nodes = splitReplacedNodes(nodes)

	byBasePath := make(map[string]*identityNode, len(nodes))
	for _, node := range nodes {
		if node.base != nil {
			byBasePath[node.base.Path] = node
		}
	}

	outcomes := make([]identityOutcome, 0, len(nodes))
	for _, node := range nodes {
		outcome, err := decideIdentityNode(node, byBasePath, in.DriveID)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}

	settled := settleIdentityPositions(outcomes, in.DriveID)
	preserveIdentityParentFolders(settled)

	return assembleIdentityPlan(settled), nil
}

// joinIdentityNodes builds one node per identity: the baseline rows first,
// then the remote rows by item ID, then the local rows by local identity,
// falling back to the path for a local row the identity cannot place.
func joinIdentityNodes(in *IdentityInputs) []*identityNode {
	join := &identityJoin{
		byItemID:        make(map[string]*identityNode),
		byPath:          make(map[string]*identityNode),
		byLocalIdentity: make(map[localIdentityKey]*identityNode),
	}
	join.addBaseline(in.Baseline)
	join.addRemote(in.Remote)
	join.addLocal(in.Local)

	return join.nodes
}

// identityJoin is the working set of one joinIdentityNodes call.
type identityJoin struct {
	nodes           []*identityNode
	byItemID        map[string]*identityNode
	byPath          map[string]*identityNode
	byLocalIdentity map[localIdentityKey]*identityNode
}

func (j *identityJoin) addBaseline(baseline *syncdomain.BaselineSnapshot) {
	paths := make([]string, 0, baseline.Len())
	baseline.ForEachPath(func(p string, _ syncdomain.BaselineEntry) { paths = append(paths, p) })
	sort.Strings(paths)
	for _, p := range paths {
		entry, _ := baseline.GetByPath(p)
		if !identityPlansItem(p, entry.ItemType) {
			continue
		}
		node := &identityNode{base: &entry}
		j.nodes = append(j.nodes, node)
		j.byPath[p] = node
		if entry.ItemID != "" {
			j.byItemID[entry.ItemID] = node
		}
		if entry.LocalHasIdentity {
			j.byLocalIdentity[localIdentityKey{entry.LocalDevice, entry.LocalInode}] = node
		}
	}
}

// addRemote joins OneDrive's rows on the item ID first, so a synced item
// that moved finds its node wherever it went; a row no item ID claims then
// takes the baseline node at its own path when that node's item is gone
// from OneDrive: the item was deleted and re-created there, or restored
// under a new ID, and the baseline rebinds to it, as the current planner
// does (R-2.10.49).
func (j *identityJoin) addRemote(remote []syncdomain.RemoteStateRow) {
	remoteRows := append([]syncdomain.RemoteStateRow(nil), remote...)
	sort.Slice(remoteRows, func(i, k int) bool { return remoteRows[i].Path < remoteRows[k].Path })
	var unjoined []*syncdomain.RemoteStateRow
	for i := range remoteRows {
		row := &remoteRows[i]
		if !identityPlansItem(row.Path, row.ItemType) {
			continue
		}
		if node, ok := j.byItemID[row.ItemID]; ok && node.remote == nil {
			node.remote = row
			continue
		}
		unjoined = append(unjoined, row)
	}
	for _, row := range unjoined {
		if node, ok := j.byPath[row.Path]; ok && node.remote == nil {
			node.remote = row
			node.remoteReplaced = true
			continue
		}
		j.nodes = append(j.nodes, &identityNode{remote: row})
	}
}

func (j *identityJoin) addLocal(local []syncdomain.LocalStateRow) {
	localRows := append([]syncdomain.LocalStateRow(nil), local...)
	sort.Slice(localRows, func(i, k int) bool { return localRows[i].Path < localRows[k].Path })
	seen := make(map[localIdentityKey]int, len(localRows))
	for i := range localRows {
		if localRows[i].LocalHasIdentity {
			seen[localIdentityKey{localRows[i].LocalDevice, localRows[i].LocalInode}]++
		}
	}
	var unpaired []*syncdomain.LocalStateRow
	for i := range localRows {
		row := &localRows[i]
		if !identityPlansItem(row.Path, row.ItemType) {
			continue
		}
		if !j.pairLocalByIdentity(row, seen) {
			unpaired = append(unpaired, row)
		}
	}
	for _, row := range unpaired {
		if node, ok := j.byPath[row.Path]; ok && node.local == nil {
			node.local = row
			node.confidence = identityByPath
			continue
		}
		j.nodes = append(j.nodes, &identityNode{local: row})
	}
}

// pairLocalByIdentity joins row to the baseline node of its local identity
// when that identity is unique among the local rows and confident enough for
// what the join would decide.
func (j *identityJoin) pairLocalByIdentity(row *syncdomain.LocalStateRow, seen map[localIdentityKey]int) bool {
	key := localIdentityKey{row.LocalDevice, row.LocalInode}
	node, ok := j.byLocalIdentity[key]
	if !ok || !row.LocalHasIdentity || seen[key] > 1 || node.local != nil {
		return false
	}
	confidence := localJoinConfidence(node.base, row)
	if confidence == identityByPath {
		return false
	}
	// Without birth times a rename and a reused inode look alike. Against
	// OneDrive's delete, trusting the weak join at another path would delete
	// a new entry the user made, so the row pairs by path instead.
	if confidence == identityWeak && node.remote == nil && row.Path != node.base.Path {
		return false
	}
	node.local = row
	node.confidence = confidence

	return true
}

// identityPlansItem reports whether an entry is an item the planner plans:
// the drive root, at the empty path, is the sync root itself, never an item
// to create, move or delete.
func identityPlansItem(p string, itemType syncdomain.ItemType) bool {
	return p != "" && itemType != syncdomain.ItemTypeRoot
}

// localJoinConfidence is how sure the identity join is that row is base's
// item: identityByPath when it is not sure at all.
func localJoinConfidence(base *syncdomain.BaselineEntry, row *syncdomain.LocalStateRow) identityConfidence {
	if base == nil || !base.LocalHasIdentity || row.ItemType != base.ItemType {
		return identityByPath
	}
	switch {
	case base.LocalBirthTime != 0 && row.LocalBirthTime != 0 && base.LocalBirthTime == row.LocalBirthTime:
		return identityStrong
	case base.LocalBirthTime != 0 && row.LocalBirthTime != 0:
		// The inode was reused for another entry.
		return identityByPath
	default:
		return identityWeak
	}
}

// splitReplacedNodes turns a side whose entry has another type than the
// baseline row into a new item: the baseline item is gone on that side.
func splitReplacedNodes(nodes []*identityNode) []*identityNode {
	out := make([]*identityNode, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node)
		if node.base == nil {
			continue
		}
		if node.local != nil && node.local.ItemType != node.base.ItemType {
			out = append(out, &identityNode{local: node.local})
			node.local = nil
			node.confidence = identityByPath
		}
		if node.remote != nil && node.remote.ItemType != node.base.ItemType {
			out = append(out, &identityNode{remote: node.remote})
			node.remote = nil
		}
	}

	return out
}

// sideChange is one side's change of a node's item since the baseline.
// moved is a move of the item itself; an item whose only change of path is
// its folder's move (it kept its name under the moved folder) has not moved,
// and the folder's move carries it.
type sideChange struct {
	present bool
	moved   bool
	edited  bool
	path    string
	hash    string
}

// sidePath is the path of node's item on one side, "" when that side has
// none.
type sidePath func(node *identityNode) string

func localPathOf(node *identityNode) string {
	if node.local == nil {
		return ""
	}
	return node.local.Path
}

func remotePathOf(node *identityNode) string {
	if node.remote == nil {
		return ""
	}
	return node.remote.Path
}

// identityCarriedPath is where a side holds base's item when only its folders
// moved: its name under the place that side holds its baseline parent.
func identityCarriedPath(base *syncdomain.BaselineEntry, byBasePath map[string]*identityNode, pathOf sidePath) string {
	dir := path.Dir(base.Path)
	if dir == "." {
		return base.Path
	}
	parent, ok := byBasePath[dir]
	if !ok {
		return base.Path
	}
	parentPath := pathOf(parent)
	if parentPath == "" || parent.base == nil {
		return base.Path
	}
	if parentPath != dir {
		return path.Join(parentPath, path.Base(base.Path))
	}

	return path.Join(identityCarriedPath(parent.base, byBasePath, pathOf), path.Base(base.Path))
}

func sideChangeOf(
	node *identityNode, byBasePath map[string]*identityNode, pathOf sidePath, hash, baseHash string,
) sideChange {
	p := pathOf(node)
	if p == "" {
		return sideChange{}
	}
	change := sideChange{present: true, path: p, hash: hash}
	if node.base != nil {
		change.moved = p != identityCarriedPath(node.base, byBasePath, pathOf)
		change.edited = node.base.ItemType == syncdomain.ItemTypeFile && hash != baseHash
	}

	return change
}

func localChange(node *identityNode, byBasePath map[string]*identityNode) sideChange {
	if node.local == nil || node.base == nil {
		return sideChangeOf(node, byBasePath, localPathOf, localHash(node), "")
	}
	return sideChangeOf(node, byBasePath, localPathOf, node.local.Hash, node.base.LocalHash)
}

func remoteChange(node *identityNode, byBasePath map[string]*identityNode) sideChange {
	if node.remote == nil || node.base == nil {
		return sideChangeOf(node, byBasePath, remotePathOf, remoteHash(node), "")
	}
	return sideChangeOf(node, byBasePath, remotePathOf, node.remote.Hash, node.base.RemoteHash)
}

func localHash(node *identityNode) string {
	if node.local == nil {
		return ""
	}
	return node.local.Hash
}

func remoteHash(node *identityNode) string {
	if node.remote == nil {
		return ""
	}
	return node.remote.Hash
}

func decideIdentityNode(
	node *identityNode, byBasePath map[string]*identityNode, driveID driveid.ID,
) (identityOutcome, error) {
	if node.base == nil {
		return decideNewIdentityNode(node, driveID)
	}

	base := node.base
	local, remote := localChange(node, byBasePath), remoteChange(node, byBasePath)
	state := identityDecisionState(base, local, remote)
	addIdentityReplacements(&state, node, local, remote)
	if err := state.Valid(); err != nil {
		return identityOutcome{}, fmt.Errorf("sync: identity planner state of %s: %w", base.Path, err)
	}
	kind := syncdomain.Decide(state).Reconciliation
	act := identityActions{node: node, driveID: driveID}
	out := identityOutcome{node: node}

	switch {
	case !local.present && !remote.present:
		if kind != syncdomain.ReconcileBaselineRemove {
			return out, unexpectedIdentityKind(base.Path, kind)
		}
		act.add(syncdomain.ActionCleanup, base.Path, "")
		out.retires = true
	case !local.present:
		if err := decideIdentityOneSide(&act, &out, base, remote, false, kind); err != nil {
			return out, err
		}
	case !remote.present:
		if err := decideIdentityOneSide(&act, &out, base, local, true, kind); err != nil {
			return out, err
		}
	default:
		final := decideIdentityPosition(&act, base.Path, local, remote)
		if err := decideIdentityContent(&act, &out, final, kind); err != nil {
			return out, err
		}
	}
	out.actions = act.actions

	return out, nil
}

// addIdentityReplacements gives the decision what the current planner reads
// from rows at the baseline path: OneDrive's item there is a new one, and
// the local entry there is another entry than the synced one (D-056). The
// current planner reads them at a path, so a node a move carried elsewhere
// has neither. Metadata drift, which only updates the baseline, is not
// modeled (reasonBookkeeping).
func addIdentityReplacements(state *syncdomain.DecisionState, node *identityNode, local, remote sideChange) {
	state.RemoteItemReplaced = remote.present && node.remoteReplaced
	state.LocalItemReplaced = local.present && local.path == node.base.Path && !sameLocalEntry(node.base, node.local)
}

// sameLocalEntry reports whether row is the entry base recorded, when both
// carry an identity; without one either way it cannot tell, and says so.
func sameLocalEntry(base *syncdomain.BaselineEntry, row *syncdomain.LocalStateRow) bool {
	baseIdentity, baseKnown := base.FileIdentity()
	rowIdentity, rowKnown := row.FileIdentity()
	if !baseKnown || !rowKnown {
		return true
	}

	return synctree.SameIdentity(baseIdentity, rowIdentity)
}

// identityDecisionState is a node's state for the decision table every
// planner shares. Position is the node's own attribute, decided apart, so no
// move-pairing or ancestor-move flag is set. A side that changed is a file
// whose content changed, or, against a delete on the other side, a file that
// moved: that move is a change the delete must not drop. A folder has no
// content to change, so a moved folder the other side deleted is deleted.
func identityDecisionState(base *syncdomain.BaselineEntry, local, remote sideChange) syncdomain.DecisionState {
	baseType := syncdomain.SideTypeOf(base.ItemType)
	isFile := base.ItemType == syncdomain.ItemTypeFile
	state := syncdomain.DecisionState{Baseline: baseType}
	if local.present {
		state.Local = baseType
		state.LocalChanged = local.edited || (isFile && local.moved && !remote.present)
	}
	if remote.present {
		state.Remote = baseType
		state.RemoteChanged = remote.edited || (isFile && remote.moved && !local.present)
	}
	if local.present && remote.present {
		state.CurrentEqual = !isFile || local.hash == remote.hash
	}

	return state
}

// decideIdentityOneSide decides a node only one side still holds (the side
// change kept): the delete goes across, or the kept side's change wins and
// its item comes back to the other side at the kept side's position.
func decideIdentityOneSide(
	act *identityActions, out *identityOutcome, base *syncdomain.BaselineEntry,
	kept sideChange, keptLocal bool, kind syncdomain.ReconciliationKind,
) error {
	switch kind {
	case syncdomain.ReconcileRemoteDelete, syncdomain.ReconcileLocalDelete:
		act.add(identityDeleteOf(kind), kept.path, "")
	case syncdomain.ReconcileDownload, syncdomain.ReconcileFolderCreateLocal,
		syncdomain.ReconcileConflictEditDelete, syncdomain.ReconcileFolderCreateRemote:
		if kept.path != base.Path {
			act.add(syncdomain.ActionCleanup, base.Path, "")
		}
		act.addCreate(keptLocal, kept.path, base.ItemType)
		out.claims, out.localContent, out.remoteContent = kept.path, keptLocal, !keptLocal
	case syncdomain.ReconcileUnclassified, syncdomain.ReconcileNoop,
		syncdomain.ReconcileBaselineRemove, syncdomain.ReconcileBaselineUpdate,
		syncdomain.ReconcileUpload, syncdomain.ReconcileLocalMove, syncdomain.ReconcileRemoteMove,
		syncdomain.ReconcileConflictEditEdit, syncdomain.ReconcileConflictCreateCreate,
		syncdomain.ReconcileConflictKeepBoth, syncdomain.ReconcileReplaceRemote,
		syncdomain.ReconcileReplaceLocal:
		return unexpectedIdentityKind(base.Path, kind)
	default:
		return unexpectedIdentityKind(base.Path, kind)
	}

	return nil
}

func identityDeleteOf(kind syncdomain.ReconciliationKind) syncdomain.ActionType {
	if kind == syncdomain.ReconcileLocalDelete {
		return syncdomain.ActionLocalDelete
	}

	return syncdomain.ActionRemoteDelete
}

func unexpectedIdentityKind(p string, kind syncdomain.ReconciliationKind) error {
	return fmt.Errorf("sync: identity planner cannot plan %s for %s", kind, p)
}

// decideIdentityPosition adds the move that puts the node where it ends and
// returns that path. OneDrive's position wins when both sides moved it to
// different places, as in the current planner.
func decideIdentityPosition(act *identityActions, basePath string, local, remote sideChange) string {
	switch {
	case local.moved && remote.moved && local.path == remote.path:
		act.add(syncdomain.ActionBaselineUpdate, local.path, basePath)
		return local.path
	case remote.moved:
		act.add(syncdomain.ActionLocalMove, remote.path, local.path)
		return remote.path
	case local.moved:
		act.add(syncdomain.ActionRemoteMove, local.path, basePath)
		return local.path
	case remote.path != basePath:
		// Only its folders moved: it ends where the folder moves carry it.
		return remote.path
	}

	return local.path
}

// decideIdentityContent adds what the decision table says the node's
// content needs at final, the path the position step put it at.
func decideIdentityContent(act *identityActions, out *identityOutcome, final string, kind syncdomain.ReconciliationKind) error {
	out.claims = final
	out.localContent, out.remoteContent = true, true
	switch kind {
	case syncdomain.ReconcileNoop:
	case syncdomain.ReconcileBaselineUpdate:
		act.add(syncdomain.ActionBaselineUpdate, final, "")
	case syncdomain.ReconcileConflictEditEdit:
		act.add(syncdomain.ActionConflictCopy, final, "")
		act.add(syncdomain.ActionDownload, final, "")
	case syncdomain.ReconcileUpload:
		act.add(syncdomain.ActionUpload, final, "")
		out.remoteContent = false
	case syncdomain.ReconcileDownload:
		act.add(syncdomain.ActionDownload, final, "")
		out.localContent = false
	case syncdomain.ReconcileUnclassified, syncdomain.ReconcileBaselineRemove,
		syncdomain.ReconcileFolderCreateLocal, syncdomain.ReconcileFolderCreateRemote,
		syncdomain.ReconcileLocalDelete, syncdomain.ReconcileRemoteDelete, syncdomain.ReconcileLocalMove,
		syncdomain.ReconcileRemoteMove, syncdomain.ReconcileConflictEditDelete,
		syncdomain.ReconcileConflictCreateCreate, syncdomain.ReconcileConflictKeepBoth,
		syncdomain.ReconcileReplaceRemote, syncdomain.ReconcileReplaceLocal:
		return unexpectedIdentityKind(final, kind)
	default:
		return unexpectedIdentityKind(final, kind)
	}

	return nil
}

// decideNewIdentityNode decides an item only one side has. A local and a
// remote new item at one path meet in the position step.
func decideNewIdentityNode(node *identityNode, driveID driveid.ID) (identityOutcome, error) {
	act := identityActions{node: node, driveID: driveID}
	out := identityOutcome{node: node}
	switch {
	case node.local != nil:
		act.addCreate(true, node.local.Path, node.local.ItemType)
		out.claims, out.localContent = node.local.Path, true
	case node.remote != nil:
		act.addCreate(false, node.remote.Path, node.remote.ItemType)
		out.claims, out.remoteContent = node.remote.Path, true
	default:
		return out, errors.New("sync: identity planner built an empty node")
	}
	out.actions = act.actions

	return out, nil
}

// settleIdentityPositions is the second step: two nodes whose content ends
// at the same path. Local and remote content meeting at a path are a
// create/create conflict, decided as the current planner decides one: the
// same file or two folders agree (a baseline update); otherwise the local
// content is kept as a conflict copy and OneDrive's comes down. A retired
// baseline row at a path another node takes needs no cleanup of its own.
func settleIdentityPositions(outcomes []identityOutcome, driveID driveid.ID) []identityOutcome {
	byClaim := make(map[string][]int)
	for i := range outcomes {
		if outcomes[i].claims != "" {
			byClaim[outcomes[i].claims] = append(byClaim[outcomes[i].claims], i)
		}
	}

	drop := make(map[int]bool)
	for i := range outcomes {
		if outcomes[i].retires && len(byClaim[outcomes[i].node.base.Path]) > 0 {
			drop[i] = true
		}
	}

	claimed := make([]string, 0, len(byClaim))
	for p := range byClaim {
		claimed = append(claimed, p)
	}
	sort.Strings(claimed)
	for _, p := range claimed {
		indexes := byClaim[p]
		if len(indexes) < 2 {
			continue
		}
		localAt, remoteAt, met := oneSidedClaimants(outcomes, indexes)
		if !met {
			continue
		}
		merged := mergeIdentityConflict(&outcomes[localAt], &outcomes[remoteAt], p, driveID)
		outcomes[remoteAt] = merged
		drop[localAt] = true
	}

	kept := make([]identityOutcome, 0, len(outcomes))
	for i := range outcomes {
		if !drop[i] {
			kept = append(kept, outcomes[i])
		}
	}

	return kept
}

// oneSidedClaimants finds, among the outcomes at indexes that claim one
// path, the last that brings only local content and the last that brings
// only OneDrive's; met says both exist.
func oneSidedClaimants(outcomes []identityOutcome, indexes []int) (localAt, remoteAt int, met bool) {
	localAt, remoteAt = -1, -1
	for _, i := range indexes {
		switch {
		case outcomes[i].localContent && !outcomes[i].remoteContent:
			localAt = i
		case outcomes[i].remoteContent && !outcomes[i].localContent:
			remoteAt = i
		}
	}

	return localAt, remoteAt, localAt >= 0 && remoteAt >= 0
}

// mergeIdentityConflict is the outcome of local content (from localSide) and
// OneDrive content (from remoteSide) meeting at p. The two nodes' moves and
// deletes stay; their creates at p are replaced by the conflict's actions.
func mergeIdentityConflict(localSide, remoteSide *identityOutcome, p string, driveID driveid.ID) identityOutcome {
	localEntry, remoteEntry := claimedLocal(localSide.node), claimedRemote(remoteSide.node)
	node := &identityNode{local: localEntry, remote: remoteEntry}
	act := identityActions{node: node, driveID: driveID}
	act.actions = append(act.actions, withoutCreatesAt(localSide.actions, p)...)
	act.actions = append(act.actions, withoutCreatesAt(remoteSide.actions, p)...)

	bothFolders := localEntry != nil && remoteEntry != nil &&
		localEntry.ItemType == syncdomain.ItemTypeFolder && remoteEntry.ItemType == syncdomain.ItemTypeFolder
	sameFile := localEntry != nil && remoteEntry != nil &&
		localEntry.ItemType == syncdomain.ItemTypeFile && remoteEntry.ItemType == syncdomain.ItemTypeFile &&
		localEntry.Hash == remoteEntry.Hash
	switch {
	case bothFolders || sameFile:
		act.add(syncdomain.ActionBaselineUpdate, p, "")
	default:
		act.add(syncdomain.ActionConflictCopy, p, "")
		remoteType := syncdomain.ItemTypeFile
		if remoteEntry != nil {
			remoteType = remoteEntry.ItemType
		}
		act.addCreate(false, p, remoteType)
	}

	return identityOutcome{node: node, actions: act.actions, claims: p, localContent: true, remoteContent: true}
}

func claimedLocal(node *identityNode) *syncdomain.LocalStateRow {
	return node.local
}

func claimedRemote(node *identityNode) *syncdomain.RemoteStateRow {
	return node.remote
}

// withoutCreatesAt is actions without the create (upload, download, folder
// create) at p that the conflict at p replaces.
func withoutCreatesAt(actions []syncdomain.Action, p string) []syncdomain.Action {
	kept := make([]syncdomain.Action, 0, len(actions))
	for i := range actions {
		a := &actions[i]
		if a.Path == p && (a.Type == syncdomain.ActionUpload || a.Type == syncdomain.ActionDownload ||
			a.Type == syncdomain.ActionFolderCreate) {
			continue
		}
		kept = append(kept, *a)
	}

	return kept
}

// identityActions builds one node's actions.
type identityActions struct {
	node    *identityNode
	driveID driveid.ID
	actions []syncdomain.Action
}

func (b *identityActions) itemID() string {
	switch {
	case b.node.base != nil:
		return b.node.base.ItemID
	case b.node.remote != nil:
		return b.node.remote.ItemID
	}

	return ""
}

func (b *identityActions) add(actionType syncdomain.ActionType, p, oldPath string) {
	b.actions = append(b.actions, syncdomain.Action{
		Type:    actionType,
		DriveID: b.driveID,
		ItemID:  b.itemID(),
		Path:    p,
		OldPath: oldPath,
	})
}

// addCreate adds the create that brings an item of itemType to the other
// side at p: an upload or a remote folder create for local content, a
// download or a local folder create for OneDrive's.
func (b *identityActions) addCreate(fromLocal bool, p string, itemType syncdomain.ItemType) {
	switch {
	case itemType == syncdomain.ItemTypeFolder && fromLocal:
		b.add(syncdomain.ActionFolderCreate, p, "")
		b.actions[len(b.actions)-1].CreateSide = syncdomain.CreateRemote
	case itemType == syncdomain.ItemTypeFolder:
		b.add(syncdomain.ActionFolderCreate, p, "")
		b.actions[len(b.actions)-1].CreateSide = syncdomain.CreateLocal
	case fromLocal:
		b.add(syncdomain.ActionUpload, p, "")
	default:
		b.add(syncdomain.ActionDownload, p, "")
	}
}

// preserveIdentityParentFolders keeps a folder whose delete would take work
// planned below it: the delete becomes the folder's create on the side that
// deleted it, as the current planner keeps it
// (preserveParentFoldersForRunnableDescendants). A delete or cleanup below
// it needs no parent.
func preserveIdentityParentFolders(outcomes []identityOutcome) {
	var needParent []string
	for i := range outcomes {
		for j := range outcomes[i].actions {
			if !isDeleteAction(&outcomes[i].actions[j]) {
				needParent = append(needParent, outcomes[i].actions[j].Path)
			}
		}
	}
	for i := range outcomes {
		base := outcomes[i].node.base
		if base == nil || base.ItemType != syncdomain.ItemTypeFolder {
			continue
		}
		for j := range outcomes[i].actions {
			action := &outcomes[i].actions[j]
			if !holdsPathBelow(needParent, action.Path) {
				continue
			}
			switch action.Type {
			case syncdomain.ActionLocalDelete:
				action.Type, action.CreateSide = syncdomain.ActionFolderCreate, syncdomain.CreateRemote
			case syncdomain.ActionRemoteDelete:
				action.Type, action.CreateSide = syncdomain.ActionFolderCreate, syncdomain.CreateLocal
			case syncdomain.ActionCleanup, syncdomain.ActionDownload, syncdomain.ActionUpload,
				syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove, syncdomain.ActionFolderCreate,
				syncdomain.ActionConflictCopy, syncdomain.ActionBaselineUpdate:
			default:
				panic(syncdomain.OutsideTheEnum(action.Type))
			}
		}
	}
}

// holdsPathBelow reports whether paths holds a path strictly below parent.
func holdsPathBelow(paths []string, parent string) bool {
	prefix := parent + "/"
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}

	return false
}

// assembleIdentityPlan flattens the outcomes into a plan: each node's
// actions depend on the one before them, and the nodes come in path order.
func assembleIdentityPlan(outcomes []identityOutcome) *syncdomain.ActionPlan {
	sort.SliceStable(outcomes, func(i, j int) bool {
		return identityOutcomeKey(&outcomes[i]) < identityOutcomeKey(&outcomes[j])
	})
	plan := &syncdomain.ActionPlan{}
	for i := range outcomes {
		for j := range outcomes[i].actions {
			plan.Actions = append(plan.Actions, outcomes[i].actions[j])
			var deps []int
			if j > 0 {
				deps = []int{len(plan.Actions) - 2}
			}
			plan.Deps = append(plan.Deps, deps)
		}
	}

	return plan
}

func identityOutcomeKey(out *identityOutcome) string {
	if len(out.actions) > 0 {
		return out.actions[0].Path
	}

	return out.claims
}
