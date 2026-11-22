package syncplan

import (
	"fmt"
	"path"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
// content is. A second step settles position conflicts, two nodes that end
// at the same path.

// IdentityInputs is what the identity-keyed planner reads: the baseline and
// the planner-visible local and remote rows of one snapshot.
type IdentityInputs struct {
	Baseline *syncdomain.BaselineSnapshot
	Local    []syncdomain.LocalStateRow
	Remote   []syncdomain.RemoteStateRow
	DriveID  driveid.ID
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
		return nil, fmt.Errorf("sync: identity planner needs a baseline snapshot")
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

	return assembleIdentityPlan(settleIdentityPositions(outcomes, in.DriveID)), nil
}

// joinIdentityNodes builds one node per identity.
func joinIdentityNodes(in *IdentityInputs) []*identityNode {
	var nodes []*identityNode
	byItemID := make(map[string]*identityNode)
	byPath := make(map[string]*identityNode)
	byLocalIdentity := make(map[localIdentityKey]*identityNode)

	paths := make([]string, 0, in.Baseline.Len())
	in.Baseline.ForEachPath(func(p string, _ syncdomain.BaselineEntry) { paths = append(paths, p) })
	sort.Strings(paths)
	for _, p := range paths {
		entry, _ := in.Baseline.GetByPath(p)
		node := &identityNode{base: &entry}
		nodes = append(nodes, node)
		byPath[p] = node
		if entry.ItemID != "" {
			byItemID[entry.ItemID] = node
		}
		if entry.LocalHasIdentity {
			byLocalIdentity[localIdentityKey{entry.LocalDevice, entry.LocalInode}] = node
		}
	}

	remoteRows := append([]syncdomain.RemoteStateRow(nil), in.Remote...)
	sort.Slice(remoteRows, func(i, j int) bool { return remoteRows[i].Path < remoteRows[j].Path })
	for i := range remoteRows {
		row := &remoteRows[i]
		if node, ok := byItemID[row.ItemID]; ok && node.remote == nil {
			node.remote = row
			continue
		}
		nodes = append(nodes, &identityNode{remote: row})
	}

	localRows := append([]syncdomain.LocalStateRow(nil), in.Local...)
	sort.Slice(localRows, func(i, j int) bool { return localRows[i].Path < localRows[j].Path })
	seen := make(map[localIdentityKey]int, len(localRows))
	for i := range localRows {
		if localRows[i].LocalHasIdentity {
			seen[localIdentityKey{localRows[i].LocalDevice, localRows[i].LocalInode}]++
		}
	}
	var unpaired []*syncdomain.LocalStateRow
	for i := range localRows {
		row := &localRows[i]
		key := localIdentityKey{row.LocalDevice, row.LocalInode}
		node, ok := byLocalIdentity[key]
		if !ok || !row.LocalHasIdentity || seen[key] > 1 || node.local != nil {
			unpaired = append(unpaired, row)
			continue
		}
		confidence := localJoinConfidence(node.base, row)
		if confidence == identityByPath {
			unpaired = append(unpaired, row)
			continue
		}
		node.local = row
		node.confidence = confidence
	}
	for _, row := range unpaired {
		if node, ok := byPath[row.Path]; ok && node.local == nil {
			node.local = row
			node.confidence = identityByPath
			continue
		}
		nodes = append(nodes, &identityNode{local: row})
	}

	return nodes
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
	act := identityActions{node: node, driveID: driveID}
	out := identityOutcome{node: node}

	switch {
	case !local.present && !remote.present:
		act.add(syncdomain.ActionCleanup, base.Path, "")
		out.retires = true
	case !local.present && !remote.moved && !remote.edited:
		act.add(syncdomain.ActionRemoteDelete, remote.path, "")
	case !local.present:
		// OneDrive changed the item the local side deleted: its change wins
		// and comes back down, at OneDrive's position.
		if remote.path != base.Path {
			act.add(syncdomain.ActionCleanup, base.Path, "")
		}
		act.addCreate(false, remote.path, base.ItemType)
		out.claims, out.remoteContent = remote.path, true
	case !remote.present && !local.moved && !local.edited:
		act.add(syncdomain.ActionLocalDelete, local.path, "")
	case !remote.present:
		if local.path != base.Path {
			act.add(syncdomain.ActionCleanup, base.Path, "")
		}
		act.addCreate(true, local.path, base.ItemType)
		out.claims, out.localContent = local.path, true
	default:
		final := decideIdentityPosition(&act, base.Path, local, remote)
		decideIdentityContent(&act, &out, final, local, remote)
	}
	out.actions = act.actions

	return out, nil
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

// decideIdentityContent adds what the node's content needs at final.
func decideIdentityContent(act *identityActions, out *identityOutcome, final string, local, remote sideChange) {
	out.claims = final
	switch {
	case local.edited && remote.edited && local.hash == remote.hash:
		act.add(syncdomain.ActionBaselineUpdate, final, "")
		out.localContent, out.remoteContent = true, true
	case local.edited && remote.edited:
		act.add(syncdomain.ActionConflictCopy, final, "")
		act.add(syncdomain.ActionDownload, final, "")
		out.localContent, out.remoteContent = true, true
	case local.edited:
		act.add(syncdomain.ActionUpload, final, "")
		out.localContent = true
	case remote.edited:
		act.add(syncdomain.ActionDownload, final, "")
		out.remoteContent = true
	default:
		out.localContent, out.remoteContent = true, true
	}
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
		return out, fmt.Errorf("sync: identity planner built an empty node")
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
		localAt, remoteAt := -1, -1
		for _, i := range indexes {
			switch {
			case outcomes[i].localContent && !outcomes[i].remoteContent:
				localAt = i
			case outcomes[i].remoteContent && !outcomes[i].localContent:
				remoteAt = i
			}
		}
		if localAt < 0 || remoteAt < 0 {
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
