package syncplan

import (
	"slices"
	"sort"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// PlanDifference is one path where the current plan and the identity-keyed
// shadow plan disagree: the actions each plans there, rendered as
// "type path [<- old path] [(create side)]" and sorted. A delete the current
// plan holds for confirmation counts as planned there.
type PlanDifference struct {
	Path    string
	Current []string
	Shadow  []string
	// Reason says why the two planners are expected to differ here, "" when
	// no known reason covers it (ExplainPlanDifferences).
	Reason string
}

// String renders the difference on one line.
func (d *PlanDifference) String() string {
	reason := d.Reason
	if reason == "" {
		reason = "unexplained"
	}

	return d.Path + ": current [" + strings.Join(d.Current, "; ") + "] shadow [" +
		strings.Join(d.Shadow, "; ") + "] (" + reason + ")"
}

// DiffPlans compares the current plan with the shadow plan, path by path,
// ignoring order and dependency edges: the identity planner orders only the
// actions of one item. It returns the differing paths in path order, without
// reasons.
func DiffPlans(current, shadow *syncdomain.ActionPlan) []PlanDifference {
	currentByPath := planLinesByPath(current)
	shadowByPath := planLinesByPath(shadow)

	paths := make([]string, 0, len(currentByPath)+len(shadowByPath))
	for p := range currentByPath {
		paths = append(paths, p)
	}
	for p := range shadowByPath {
		if _, ok := currentByPath[p]; !ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	var diffs []PlanDifference
	for _, p := range paths {
		currentLines, shadowLines := currentByPath[p], shadowByPath[p]
		if strings.Join(currentLines, "\n") == strings.Join(shadowLines, "\n") {
			continue
		}
		diffs = append(diffs, PlanDifference{Path: p, Current: currentLines, Shadow: shadowLines})
	}

	return diffs
}

func planLinesByPath(plan *syncdomain.ActionPlan) map[string][]string {
	byPath := map[string][]string{}
	if plan == nil {
		return byPath
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		byPath[a.Path] = append(byPath[a.Path], planActionLine(a))
	}
	for i := range plan.HeldDeletes {
		held := &plan.HeldDeletes[i]
		byPath[held.Path] = append(byPath[held.Path], heldDeleteLine(held))
	}
	for p := range byPath {
		sort.Strings(byPath[p])
	}

	return byPath
}

func planActionLine(a *syncdomain.Action) string {
	line := a.Type.String() + " " + a.Path
	if a.OldPath != "" {
		line += " <- " + a.OldPath
	}
	if a.Type == syncdomain.ActionFolderCreate {
		line += " (" + a.CreateSide.String() + ")"
	}

	return line
}

// heldDeleteLine renders a held delete as the delete the shadow plans, which
// has no hold: the difference is the hold alone.
func heldDeleteLine(held *syncdomain.HeldDelete) string {
	if held.Side == syncdomain.DeleteSafetySideLocal {
		return syncdomain.ActionLocalDelete.String() + " " + held.Path
	}

	return syncdomain.ActionRemoteDelete.String() + " " + held.Path
}

// The reasons the two planners are expected to differ. Anything else is a
// difference nobody has reviewed yet.
const (
	reasonMovedItem = "an item moved: the identity planner composes the move with the item's " +
		"other changes in one pass, where the current planner pairs a delete with a create or defers"
	reasonReplacedItem = "an item changed type: the identity planner decides the new item and each " +
		"descendant of the old one on its own, where the current planner replaces the subtree"
	reasonUnavailableTruth = "an observation issue: the current planner suppresses work where truth " +
		"is unavailable, the identity planner does not read the issues"
	reasonConflictCopy = "below a conflict copy: the current planner trims the work the copy's " +
		"rename invalidates, the identity planner leaves it to its own nodes"
	reasonSharedLocalIdentity = "an identity at two paths of one tree: the identity planner keys a " +
		"node by it and pairs at most one of them, where the current planner pairs by path and moves"
	reasonBookkeeping = "baseline bookkeeping only: the current planner records new metadata over " +
		"unchanged content, which the identity planner does not model"
	reasonMoveSource = "the source of a move a known reason covers: one planner moves the item " +
		"away from here and the other plans it in place, so the difference is the move's"
	reasonEmptiedFolder = "a folder above work a known reason covers: the identity planner " +
		"deletes the folder once it deletes or moves out what is below it, where the current planner keeps it"
)

// ExplainPlanDifferences sets each difference's reason, from the shape of
// the inputs both plans came from: a path at or below an item that moved on
// either side (its baseline path and where each side holds it), at or below
// an entry whose type differs from its baseline row's, at or below a path
// with an observation issue, below a conflict copy either plan makes, at an
// identity two rows of one tree share, or a difference of baseline updates
// alone; then a difference that follows from a move one of those covers (its
// source, a folder it empties). It returns the differences no reason covers.
func ExplainPlanDifferences(in *IdentityInputs, current, shadow *syncdomain.ActionPlan, diffs []PlanDifference) []PlanDifference {
	if in == nil || in.Baseline == nil {
		return diffs
	}
	moved, replaced := identityDifferenceRoots(in)
	copies := append(conflictCopyPaths(current), conflictCopyPaths(shadow)...)
	shared := sharedIdentityPaths(in)
	issues := make([]string, 0, len(in.ObservationIssues))
	for i := range in.ObservationIssues {
		issues = append(issues, in.ObservationIssues[i].Path)
	}

	var unexplained []PlanDifference
	for i := range diffs {
		d := &diffs[i]
		switch {
		case touchesAny(d, moved):
			d.Reason = reasonMovedItem
		case touchesAny(d, replaced):
			d.Reason = reasonReplacedItem
		case atOrBelowAny(d.Path, issues):
			d.Reason = reasonUnavailableTruth
		case belowAny(d.Path, copies):
			d.Reason = reasonConflictCopy
		case touchesAny(d, shared):
			d.Reason = reasonSharedLocalIdentity
		case onlyBaselineUpdates(d):
			d.Reason = reasonBookkeeping
		default:
			unexplained = append(unexplained, *d)
		}
	}

	return explainByExplainedMoves(diffs, unexplained)
}

// explainByExplainedMoves gives a reason to the differences that follow from
// work another difference's known reason covers: a move's source path,
// where the planner that does not move plans the item in place (a download
// back, or nothing), and a folder the identity planner deletes, and nothing
// else there, above a move's source or above a delete a known reason covers,
// where the current planner keeps the folder (plans nothing, or re-creates it
// on the side that lost it for the work it keeps below). Every such
// difference is the explained work's consequence, not a disagreement of its
// own. It returns the differences that stay unexplained.
func explainByExplainedMoves(diffs, unexplained []PlanDifference) []PlanDifference {
	if len(unexplained) == 0 {
		return nil
	}
	var sources, emptied []string
	for i := range diffs {
		if diffs[i].Reason == "" {
			continue
		}
		for _, line := range append(append([]string(nil), diffs[i].Current...), diffs[i].Shadow...) {
			if _, old, ok := strings.Cut(line, " <- "); ok {
				sources = append(sources, old)
				emptied = append(emptied, old)
			}
		}
		if shadowOnlyDeletes(&diffs[i]) {
			emptied = append(emptied, diffs[i].Path)
		}
	}

	var still []PlanDifference
	for i := range unexplained {
		d := &unexplained[i]
		reason := ""
		switch {
		case slices.Contains(sources, d.Path):
			reason = reasonMoveSource
		case isAncestorOfAny(d.Path, emptied) && shadowOnlyDeletes(d):
			reason = reasonEmptiedFolder
		}
		if reason == "" {
			still = append(still, *d)
			continue
		}
		for j := range diffs {
			if diffs[j].Path == d.Path {
				diffs[j].Reason = reason
			}
		}
	}

	return still
}

// isAncestorOfAny reports whether some path of paths is below p.
func isAncestorOfAny(p string, paths []string) bool {
	for _, other := range paths {
		if strings.HasPrefix(other, p+"/") {
			return true
		}
	}

	return false
}

// shadowOnlyDeletes reports whether the shadow plan holds actions at the
// difference's path and every one of them deletes it.
func shadowOnlyDeletes(d *PlanDifference) bool {
	if len(d.Shadow) == 0 {
		return false
	}
	local := syncdomain.ActionLocalDelete.String() + " "
	remote := syncdomain.ActionRemoteDelete.String() + " "
	for _, line := range d.Shadow {
		if !strings.HasPrefix(line, local) && !strings.HasPrefix(line, remote) {
			return false
		}
	}

	return true
}

// identityDifferenceRoots returns the paths of moved items (baseline path
// and each side's path) and of replaced entries, from the identity join.
func identityDifferenceRoots(in *IdentityInputs) (moved, replaced []string) {
	nodes := joinIdentityNodes(in)
	for _, node := range nodes {
		if node.base == nil {
			continue
		}
		if node.local != nil && node.local.ItemType != node.base.ItemType {
			replaced = append(replaced, node.base.Path)
		}
		if node.remote != nil && node.remote.ItemType != node.base.ItemType {
			replaced = append(replaced, node.base.Path)
		}
	}
	nodes = splitReplacedNodes(nodes)
	byBasePath := make(map[string]*identityNode, len(nodes))
	for _, node := range nodes {
		if node.base != nil {
			byBasePath[node.base.Path] = node
		}
	}
	for _, node := range nodes {
		if node.base == nil {
			continue
		}
		local, remote := localChange(node, byBasePath), remoteChange(node, byBasePath)
		if local.moved || remote.moved {
			moved = append(moved, node.base.Path)
		}
		if local.moved {
			moved = append(moved, local.path)
		}
		if remote.moved {
			moved = append(moved, remote.path)
		}
	}

	return moved, replaced
}

// sharedIdentityPaths returns the paths of rows whose identity another row
// of the same tree has too: two local rows of one local identity (a hard
// link, or one entry seen at two paths), or two baseline rows of one local
// identity or one item ID. The join keys a node by that identity, so it can
// pair only one of them.
func sharedIdentityPaths(in *IdentityInputs) []string {
	localByKey := make(map[localIdentityKey][]string)
	for i := range in.Local {
		if in.Local[i].LocalHasIdentity {
			key := localIdentityKey{in.Local[i].LocalDevice, in.Local[i].LocalInode}
			localByKey[key] = append(localByKey[key], in.Local[i].Path)
		}
	}
	baseByKey := make(map[localIdentityKey][]string)
	baseByItemID := make(map[string][]string)
	in.Baseline.ForEachPath(func(p string, entry syncdomain.BaselineEntry) {
		if entry.LocalHasIdentity {
			key := localIdentityKey{entry.LocalDevice, entry.LocalInode}
			baseByKey[key] = append(baseByKey[key], p)
		}
		if entry.ItemID != "" {
			baseByItemID[entry.ItemID] = append(baseByItemID[entry.ItemID], p)
		}
	})
	var shared []string
	for _, groups := range []map[localIdentityKey][]string{localByKey, baseByKey} {
		for _, paths := range groups {
			if len(paths) > 1 {
				shared = append(shared, paths...)
			}
		}
	}
	for _, paths := range baseByItemID {
		if len(paths) > 1 {
			shared = append(shared, paths...)
		}
	}
	sort.Strings(shared)

	return shared
}

// onlyBaselineUpdates reports whether every action either plan holds at the
// difference's path is a baseline update.
func onlyBaselineUpdates(d *PlanDifference) bool {
	prefix := syncdomain.ActionBaselineUpdate.String() + " "
	for _, lines := range [][]string{d.Current, d.Shadow} {
		for _, line := range lines {
			if !strings.HasPrefix(line, prefix) {
				return false
			}
		}
	}

	return true
}

func conflictCopyPaths(plan *syncdomain.ActionPlan) []string {
	if plan == nil {
		return nil
	}
	var copies []string
	for i := range plan.Actions {
		if plan.Actions[i].Type == syncdomain.ActionConflictCopy {
			copies = append(copies, plan.Actions[i].Path)
		}
	}

	return copies
}

// ShadowPlanDifferences plans in with the identity-keyed planner and returns
// where its plan differs from current, each difference with its reason when
// a known one covers it, and how many no reason covers. The shadow plan is
// only compared, never executed or recorded.
func ShadowPlanDifferences(in *IdentityInputs, current *syncdomain.ActionPlan) (diffs []PlanDifference, unexplained int, err error) {
	shadow, err := PlanIdentityKeyed(in)
	if err != nil {
		return nil, 0, err
	}
	diffs = DiffPlans(current, shadow)

	return diffs, len(ExplainPlanDifferences(in, current, shadow, diffs)), nil
}

// touchesAny reports whether the difference's path, or an old path one of
// its actions names, is at or below one of roots.
func touchesAny(d *PlanDifference, roots []string) bool {
	if atOrBelowAny(d.Path, roots) {
		return true
	}
	for _, line := range append(append([]string(nil), d.Current...), d.Shadow...) {
		if _, old, ok := strings.Cut(line, " <- "); ok && atOrBelowAny(old, roots) {
			return true
		}
	}

	return false
}

func atOrBelowAny(p string, roots []string) bool {
	for _, root := range roots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}

	return false
}

func belowAny(p string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(p, root+"/") {
			return true
		}
	}

	return false
}
