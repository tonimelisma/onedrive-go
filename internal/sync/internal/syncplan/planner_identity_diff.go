package syncplan

import (
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
)

// ExplainPlanDifferences sets each difference's reason, from the shape of
// the inputs both plans came from: a path at or below an item that moved on
// either side (its baseline path and where each side holds it), at or below
// an entry whose type differs from its baseline row's, at or below a path
// with an observation issue, or below a conflict copy either plan makes. It returns the differences no reason covers.
func ExplainPlanDifferences(in *IdentityInputs, current, shadow *syncdomain.ActionPlan, diffs []PlanDifference) []PlanDifference {
	if in == nil || in.Baseline == nil {
		return diffs
	}
	moved, replaced := identityDifferenceRoots(in)
	copies := append(conflictCopyPaths(current), conflictCopyPaths(shadow)...)
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
		default:
			unexplained = append(unexplained, *d)
		}
	}

	return unexplained
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
