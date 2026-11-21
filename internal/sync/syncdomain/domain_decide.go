package syncdomain

import (
	"errors"
	"fmt"
	"slices"
)

// SideType is what one side (the baseline, the local tree, OneDrive) holds at
// a path: nothing, or an item of one type.
type SideType uint8

const (
	SideAbsent SideType = iota
	SideFile
	SideFolder
	SideRoot
)

// sideTypeCount is the number of SideType members.
const sideTypeCount = int(SideRoot) + 1

// String returns the name the comparison query spells the side's item type
// with: the empty string for an absent side.
func (t SideType) String() string {
	switch t {
	case SideAbsent:
		return ""
	case SideFile:
		return StrFile
	case SideFolder:
		return StrFolder
	case SideRoot:
		return StrRoot
	default:
		return fmt.Sprintf("SideType(%d)", uint8(t))
	}
}

// Present reports whether the side holds an item.
func (t SideType) Present() bool {
	return t != SideAbsent
}

// SideTypeOf is the side type of a present item of type itemType.
func SideTypeOf(itemType ItemType) SideType {
	switch itemType {
	case ItemTypeFile:
		return SideFile
	case ItemTypeFolder:
		return SideFolder
	case ItemTypeRoot:
		return SideRoot
	default:
		panic(fmt.Sprintf("sync: side type of %s", OutsideTheEnum(itemType)))
	}
}

// ParseSideType reads the item type the comparison query reports for one
// side, the empty string for an absent one.
func ParseSideType(s string) (SideType, error) {
	if s == "" {
		return SideAbsent, nil
	}
	itemType, err := ParseItemType(s)
	if err != nil {
		return SideAbsent, err
	}

	return SideTypeOf(itemType), nil
}

// AllSideTypes lists every SideType member.
func AllSideTypes() []SideType {
	return []SideType{SideAbsent, SideFile, SideFolder, SideRoot}
}

// DecisionState is everything the decision table reads about one path: what
// each side holds there and the per-path facts the comparison query derives
// from the three tables (content changes, replacements, move pairing, the
// ancestor-move waits). All fields are comparable, so a state is a map key.
type DecisionState struct {
	Baseline, Local, Remote SideType

	// LocalMoveSource: the synced item at this path moved locally to one
	// destination (exactly one pairing candidate).
	LocalMoveSource bool
	// LocalMoveDest: this unsynced local path is where one synced item moved.
	LocalMoveDest bool
	// RemoteMoveSource / RemoteMoveDest: the same for a move on OneDrive.
	RemoteMoveSource bool
	RemoteMoveDest   bool

	LocalItemReplaced, RemoteItemReplaced bool

	DeleteAwaitsAncestorMove       bool
	LocalCopyAwaitsAncestorMove    bool
	RemoteCreateAwaitsAncestorMove bool
	KeptLocalFolder                bool
	RemoteMoveShadowed             bool

	CurrentEqual                bool
	LocalChanged, RemoteChanged bool
	LocalSubtreeChanged         bool
	RemoteSubtreeChanged        bool
	MetadataDrift               bool
}

// Decision is the decision table's answer for one path: the comparison kind,
// which move composition also reads, and what to do.
type Decision struct {
	Comparison     ComparisonKind
	Reconciliation ReconciliationKind
}

// Decide is the decision table: the comparison kind of a state, then what to
// do about it. Pure and total: a state no rule covers (no side present) is
// unclassified, and planning refuses it (R-6.2.16).
func Decide(s DecisionState) Decision {
	comparison := classify(s)

	return Decision{Comparison: comparison, Reconciliation: Reconcile(comparison, ReconcileInputsOf(s))}
}

// ItemType is the type the decision reads for the path: the first present of
// the baseline, local and remote sides, SideAbsent when none is.
func (s DecisionState) ItemType() SideType {
	for _, side := range []SideType{s.Baseline, s.Local, s.Remote} {
		if side.Present() {
			return side
		}
	}

	return SideAbsent
}

// LocalTypeChanged reports a local item of another type than the synced one.
func (s DecisionState) LocalTypeChanged() bool {
	return s.Baseline.Present() && s.Local.Present() && s.Baseline != s.Local
}

// RemoteTypeChanged reports a remote item of another type than the synced one.
func (s DecisionState) RemoteTypeChanged() bool {
	return s.Baseline.Present() && s.Remote.Present() && s.Baseline != s.Remote
}

// classify is the first stage: the comparison kind. The rules are tried in
// order; the first that holds decides.
func classify(s DecisionState) ComparisonKind {
	if s.Baseline.Present() {
		return classifySynced(s)
	}

	return classifyUnsynced(s)
}

// classifySynced classifies a path the baseline holds.
func classifySynced(s DecisionState) ComparisonKind {
	local, remote := s.Local.Present(), s.Remote.Present()
	switch {
	// The synced item moved; a new entry at its old path (rename and
	// recreate) is planned as a create by move composition.
	case s.LocalMoveSource && (!local || s.LocalItemReplaced):
		return ComparisonLocalMoveSource
	case !local && !remote && s.DeleteAwaitsAncestorMove:
		return comparisonDeleteAwaitsAncestorMove
	case !local && !remote:
		return comparisonBothMissing
	case s.RemoteMoveSource:
		return ComparisonRemoteMoveSource
	case !local:
		return comparisonLocalMissing
	case !remote:
		return comparisonRemoteMissing
	case !s.LocalChanged && !s.RemoteChanged:
		return comparisonUnchanged
	// Changed on some side, and the two sides are proven equal: nothing
	// transfers, whichever side changed.
	case s.CurrentEqual:
		return ComparisonEqualAgain
	default:
		// Only rows that changed on some side and whose content differs
		// get here; reconcileDiverged relies on it.
		return ComparisonDiverged
	}
}

// classifyUnsynced classifies a path the baseline does not hold.
func classifyUnsynced(s DecisionState) ComparisonKind {
	local, remote := s.Local.Present(), s.Remote.Present()
	switch {
	case local && s.LocalMoveDest:
		return ComparisonLocalMoveDest
	case remote && s.RemoteMoveDest:
		return ComparisonRemoteMoveDest
	case local && remote && s.CurrentEqual:
		return comparisonCreateEqual
	case local && remote:
		return comparisonCreateConflict
	case local:
		return classifyLocalOnly(s)
	case remote:
		return classifyRemoteOnly(s)
	default:
		return ComparisonUnclassified
	}
}

// classifyLocalOnly classifies an entry only the local tree holds.
func classifyLocalOnly(s DecisionState) ComparisonKind {
	switch {
	case s.LocalCopyAwaitsAncestorMove:
		return comparisonDeleteAwaitsAncestorMove
	case s.KeptLocalFolder:
		return comparisonKeptLocalFolder
	default:
		return ComparisonLocalOnlyCreate
	}
}

// classifyRemoteOnly classifies an item only OneDrive holds.
func classifyRemoteOnly(s DecisionState) ComparisonKind {
	switch {
	case s.RemoteMoveShadowed:
		return ComparisonRemoteMoveShadowed
	case s.RemoteCreateAwaitsAncestorMove:
		return comparisonDeleteAwaitsAncestorMove
	default:
		return ComparisonRemoteOnlyCreate
	}
}

// ReconcileInputs is what the second stage reads besides the comparison kind:
// the path's item type, each side's type and the flags.
type ReconcileInputs struct {
	ItemType, Local, Remote SideType

	LocalChanged, RemoteChanged               bool
	LocalSubtreeChanged, RemoteSubtreeChanged bool
	LocalTypeChanged, RemoteTypeChanged       bool
	RemoteItemReplaced                        bool
	MetadataDrift                             bool
}

// ReconcileInputsOf derives the second stage's inputs from a state.
func ReconcileInputsOf(s DecisionState) ReconcileInputs {
	return ReconcileInputs{
		ItemType:             s.ItemType(),
		Local:                s.Local,
		Remote:               s.Remote,
		LocalChanged:         s.LocalChanged,
		RemoteChanged:        s.RemoteChanged,
		LocalSubtreeChanged:  s.LocalSubtreeChanged,
		RemoteSubtreeChanged: s.RemoteSubtreeChanged,
		LocalTypeChanged:     s.LocalTypeChanged(),
		RemoteTypeChanged:    s.RemoteTypeChanged(),
		RemoteItemReplaced:   s.RemoteItemReplaced,
		MetadataDrift:        s.MetadataDrift,
	}
}

// Reconcile is the second stage: what to do about a path of the given
// comparison kind. Every genuine "nothing to do" is an explicit noop; a kind
// no rule covers is unclassified, and planning fails on it (R-6.2.16).
func Reconcile(kind ComparisonKind, in ReconcileInputs) ReconciliationKind {
	switch kind {
	case ComparisonDiverged:
		return reconcileDiverged(in)
	case comparisonLocalMissing:
		return reconcileLocalMissing(in)
	case comparisonRemoteMissing:
		return reconcileRemoteMissing(in)
	case ComparisonLocalOnlyCreate:
		return reconcileCreate(in.ItemType, ReconcileFolderCreateRemote, ReconcileUpload)
	case ComparisonRemoteOnlyCreate:
		return reconcileCreate(in.ItemType, ReconcileFolderCreateLocal, ReconcileDownload)
	case comparisonUnchanged:
		// Nothing transfers, but the baseline must learn the new metadata:
		// the scanner's size+mtime fast path compares against it (R-2.2.3).
		if in.MetadataDrift {
			return ReconcileBaselineUpdate
		}
		return ReconcileNoop
	case ComparisonUnclassified:
		return ReconcileUnclassified
	case comparisonBothMissing, ComparisonLocalMoveSource, ComparisonLocalMoveDest,
		ComparisonRemoteMoveSource, ComparisonRemoteMoveDest, ComparisonRemoteMoveShadowed,
		comparisonKeptLocalFolder, comparisonDeleteAwaitsAncestorMove, comparisonCreateEqual,
		ComparisonEqualAgain, comparisonCreateConflict:
		return reconcileByKind(kind)
	default:
		return ReconcileUnclassified
	}
}

// reconcileByKind decides the kinds whose decision reads no flag.
func reconcileByKind(kind ComparisonKind) ReconciliationKind {
	switch kind {
	case comparisonBothMissing:
		return ReconcileBaselineRemove
	case ComparisonLocalMoveSource, ComparisonLocalMoveDest:
		return ReconcileLocalMove
	case ComparisonRemoteMoveSource, ComparisonRemoteMoveDest:
		return ReconcileRemoteMove
	// The local move carries the item (shadowed); a folder kept after its
	// online deletion is no new content; a delete waiting for an ancestor's
	// move is planned next pass, once that move re-pathed the baseline.
	case ComparisonRemoteMoveShadowed, comparisonKeptLocalFolder, comparisonDeleteAwaitsAncestorMove:
		return ReconcileNoop
	case comparisonCreateEqual, ComparisonEqualAgain:
		return ReconcileBaselineUpdate
	case comparisonCreateConflict:
		return ReconcileConflictCreateCreate
	case ComparisonUnclassified, ComparisonDiverged, comparisonLocalMissing, comparisonRemoteMissing,
		ComparisonLocalOnlyCreate, ComparisonRemoteOnlyCreate, comparisonUnchanged:
		// Decided by Reconcile itself from the flags.
		return ReconcileUnclassified
	default:
		return ReconcileUnclassified
	}
}

// reconcileCreate decides a create on one side: a folder is created, a file
// transferred. A root-typed entry is no item either side creates (the
// observers never report one as new), so it is unclassified and planning
// refuses it rather than transferring a drive root.
func reconcileCreate(itemType SideType, folder, file ReconciliationKind) ReconciliationKind {
	switch itemType {
	case SideFolder:
		return folder
	case SideFile:
		return file
	case SideAbsent, SideRoot:
		return ReconcileUnclassified
	default:
		return ReconcileUnclassified
	}
}

// reconcileDiverged decides a path that changed on some side and whose two
// sides differ.
func reconcileDiverged(in ReconcileInputs) ReconciliationKind {
	if kind, replaced := reconcileDivergedTypes(in); replaced {
		return kind
	}
	switch {
	case in.LocalChanged && !in.RemoteChanged:
		return ReconcileUpload
	case !in.LocalChanged && in.RemoteChanged:
		return ReconcileDownload
	case in.LocalChanged && in.RemoteChanged:
		return ReconcileConflictEditEdit
	default:
		// Changed on neither side: classifySynced never calls that diverged.
		return ReconcileUnclassified
	}
}

// reconcileDivergedTypes decides a diverged path whose item changed type on
// some side, first: the type change, not the content, decides it. The bool
// reports whether a rule matched.
func reconcileDivergedTypes(in ReconcileInputs) (ReconciliationKind, bool) {
	switch {
	// Different types with changes on both sides keep both: the local entry
	// becomes a conflict copy and OneDrive's item takes the path; replacing
	// either side would delete the other's content (D-059). A new
	// filesystem identity alone is not a local change (D-057).
	case in.Local != in.Remote &&
		(in.LocalChanged || in.LocalSubtreeChanged) && (in.RemoteChanged || in.RemoteSubtreeChanged):
		return ReconcileConflictKeepBoth, true
	// Both sides replaced the item with the same new type: two creates at
	// one path, not a type change one side wins.
	case in.LocalTypeChanged && in.RemoteTypeChanged && in.Local == in.Remote:
		return ReconcileConflictCreateCreate, true
	// A type change replaces the item; when the other side also changed,
	// local wins (the replaced OneDrive item goes to the recycle bin).
	case in.LocalChanged && (in.LocalTypeChanged || in.RemoteTypeChanged):
		return ReconcileReplaceRemote, true
	case in.RemoteTypeChanged:
		return ReconcileReplaceLocal, true
	default:
		return ReconcileUnclassified, false
	}
}

// reconcileLocalMissing decides a synced path the local tree no longer has.
func reconcileLocalMissing(in ReconcileInputs) ReconciliationKind {
	switch {
	// A folder deleted locally while OneDrive replaced it (a new item at the
	// same path, or a new type) is not the folder the user deleted.
	case in.Remote == SideFolder && (in.RemoteTypeChanged || in.RemoteItemReplaced):
		return ReconcileFolderCreateLocal
	// A folder the user deleted is deleted on OneDrive exactly like a file;
	// action normalization protects remote-only descendants.
	case !in.RemoteChanged && !in.RemoteItemReplaced:
		return ReconcileRemoteDelete
	default:
		return ReconcileDownload
	}
}

// reconcileRemoteMissing decides a synced path OneDrive no longer has.
func reconcileRemoteMissing(in ReconcileInputs) ReconciliationKind {
	switch {
	case in.LocalTypeChanged && in.Local == SideFolder:
		return ReconcileFolderCreateRemote
	// A folder with a new filesystem identity but nothing changed below it
	// accepts OneDrive's delete too (D-057).
	case !in.LocalChanged:
		return ReconcileLocalDelete
	default:
		return ReconcileConflictEditDelete
	}
}

// errInvalidDecisionState reports a state the comparison query cannot
// produce: a fact about a side the path does not have, or one that
// contradicts the side types.
var errInvalidDecisionState = errors.New("sync: decision state contradicts its side types")

// Valid reports whether the comparison query can produce s: each rule is one
// line of a flag's definition there (a flag about an absent side is false; a
// type change is a content change; a folder has no content to change; ...).
// The decision table is held to its properties over the valid states.
func (s DecisionState) Valid() error {
	for _, rule := range decisionStateRules() {
		if !rule.holds(s) {
			return fmt.Errorf("%w: %s", errInvalidDecisionState, rule.name)
		}
	}

	return nil
}

// decisionStateRule is one line of the flag definitions.
type decisionStateRule struct {
	name  string
	holds func(DecisionState) bool
}

func implies(a, b bool) bool { return !a || b }

func decisionStateRules() []decisionStateRule {
	return slices.Concat(contentRules(), sideFactRules(), placementRules())
}

// contentRules are the rules of the content-change flags.
func contentRules() []decisionStateRule {
	return []decisionStateRule{
		{"local_changed needs baseline and local", func(s DecisionState) bool {
			return implies(s.LocalChanged, s.Baseline.Present() && s.Local.Present())
		}},
		{"remote_changed needs baseline and remote", func(s DecisionState) bool {
			return implies(s.RemoteChanged, s.Baseline.Present() && s.Remote.Present())
		}},
		{"a local type change is a local change", func(s DecisionState) bool {
			return implies(s.LocalTypeChanged(), s.LocalChanged)
		}},
		{"a remote type change is a remote change", func(s DecisionState) bool {
			return implies(s.RemoteTypeChanged(), s.RemoteChanged)
		}},
		{"a synced folder that is still a folder locally has no local change", func(s DecisionState) bool {
			return implies(s.Baseline == SideFolder && s.Local == SideFolder, !s.LocalChanged)
		}},
		{"a synced folder that is still a folder online has no remote change", func(s DecisionState) bool {
			return implies(s.Baseline == SideFolder && s.Remote == SideFolder, !s.RemoteChanged)
		}},
	}
}

// sideFactRules are the rules of the replacement, subtree, kept-folder,
// equality and drift flags.
func sideFactRules() []decisionStateRule {
	return []decisionStateRule{
		{"local_item_replaced needs baseline and local", func(s DecisionState) bool {
			return implies(s.LocalItemReplaced, s.Baseline.Present() && s.Local.Present())
		}},
		{"remote_item_replaced needs baseline and remote", func(s DecisionState) bool {
			return implies(s.RemoteItemReplaced, s.Baseline.Present() && s.Remote.Present())
		}},
		{"local_subtree_changed is asked of a folder OneDrive replaced with a file", func(s DecisionState) bool {
			return implies(s.LocalSubtreeChanged, s.Baseline == SideFolder && s.Local == SideFolder &&
				s.Remote.Present() && s.Remote != SideFolder)
		}},
		{"remote_subtree_changed is asked of a folder the user replaced with a file", func(s DecisionState) bool {
			return implies(s.RemoteSubtreeChanged, s.Baseline == SideFolder && s.Remote == SideFolder &&
				s.Local.Present() && s.Local != SideFolder)
		}},
		{"kept_local_folder is an unsynced local-only folder", func(s DecisionState) bool {
			return implies(s.KeptLocalFolder, !s.Baseline.Present() && !s.Remote.Present() && s.Local == SideFolder)
		}},
		{"current_equal needs both sides of one type", func(s DecisionState) bool {
			return implies(s.CurrentEqual, s.Local.Present() && s.Local == s.Remote)
		}},
		{"two folders are equal", func(s DecisionState) bool {
			return implies(s.Local == SideFolder && s.Remote == SideFolder, s.CurrentEqual)
		}},
		{"metadata_drift needs all three sides", func(s DecisionState) bool {
			return implies(s.MetadataDrift, s.Baseline.Present() && s.Local.Present() && s.Remote.Present())
		}},
	}
}

// placementRules are the rules of the move pairing and ancestor-move flags.
func placementRules() []decisionStateRule {
	return []decisionStateRule{
		{"a local move source is a baseline path", func(s DecisionState) bool {
			return implies(s.LocalMoveSource, s.Baseline.Present())
		}},
		{"a local move destination is an unsynced local path", func(s DecisionState) bool {
			return implies(s.LocalMoveDest, s.Local.Present() && !s.Baseline.Present())
		}},
		{"a remote move source still holds the synced item locally", func(s DecisionState) bool {
			return implies(s.RemoteMoveSource, s.Baseline.Present() && s.Local == s.Baseline)
		}},
		{"a remote move destination is a remote path no unsynced local entry holds", func(s DecisionState) bool {
			return implies(s.RemoteMoveDest, s.Remote.Present() && implies(s.Local.Present(), s.Baseline.Present()))
		}},
		{"a shadowed remote move is a remote path", func(s DecisionState) bool {
			return implies(s.RemoteMoveShadowed, s.Remote.Present())
		}},
		{"a remote create waiting for an ancestor's move is a remote path", func(s DecisionState) bool {
			return implies(s.RemoteCreateAwaitsAncestorMove, s.Remote.Present())
		}},
		{"a local copy waiting for an ancestor's move is a local path", func(s DecisionState) bool {
			return implies(s.LocalCopyAwaitsAncestorMove, s.Local.Present())
		}},
		{"a delete waiting for an ancestor's move is a synced path gone locally", func(s DecisionState) bool {
			return implies(s.DeleteAwaitsAncestorMove, s.Baseline.Present() && !s.Local.Present())
		}},
	}
}

// Inputs lists the state as the plan error prints it: each side's presence
// and type, then every flag, as 0 or 1.
func (s DecisionState) Inputs() []PlannerDecisionInput {
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	inputs := make([]PlannerDecisionInput, 0, len(decisionInputColumns()))
	for _, column := range decisionInputColumns() {
		var value string
		switch {
		case column.side != nil:
			value = column.side(s).String()
		default:
			value = bit(column.flag(s))
		}
		inputs = append(inputs, PlannerDecisionInput{Column: column.name, Value: value})
	}

	return inputs
}

// decisionInputColumn is one printed input of a decision state: a side type
// or a flag.
type decisionInputColumn struct {
	name string
	side func(DecisionState) SideType
	flag func(DecisionState) bool
}

func decisionInputColumns() []decisionInputColumn {
	return []decisionInputColumn{
		{name: "baseline_present", flag: func(s DecisionState) bool { return s.Baseline.Present() }},
		{name: "local_present", flag: func(s DecisionState) bool { return s.Local.Present() }},
		{name: "remote_present", flag: func(s DecisionState) bool { return s.Remote.Present() }},
		{name: "baseline_item_type", side: func(s DecisionState) SideType { return s.Baseline }},
		{name: "local_item_type", side: func(s DecisionState) SideType { return s.Local }},
		{name: "remote_item_type", side: func(s DecisionState) SideType { return s.Remote }},
		{name: "local_move_source", flag: func(s DecisionState) bool { return s.LocalMoveSource }},
		{name: "local_move_dest", flag: func(s DecisionState) bool { return s.LocalMoveDest }},
		{name: "remote_move_source", flag: func(s DecisionState) bool { return s.RemoteMoveSource }},
		{name: "remote_move_dest", flag: func(s DecisionState) bool { return s.RemoteMoveDest }},
		{name: "local_item_replaced", flag: func(s DecisionState) bool { return s.LocalItemReplaced }},
		{name: "remote_item_replaced", flag: func(s DecisionState) bool { return s.RemoteItemReplaced }},
		{name: "delete_awaits_ancestor_move", flag: func(s DecisionState) bool { return s.DeleteAwaitsAncestorMove }},
		{name: "local_copy_awaits_ancestor_move", flag: func(s DecisionState) bool { return s.LocalCopyAwaitsAncestorMove }},
		{name: "remote_create_awaits_ancestor_move", flag: func(s DecisionState) bool { return s.RemoteCreateAwaitsAncestorMove }},
		{name: "kept_local_folder", flag: func(s DecisionState) bool { return s.KeptLocalFolder }},
		{name: "remote_move_shadowed", flag: func(s DecisionState) bool { return s.RemoteMoveShadowed }},
		{name: "current_equal", flag: func(s DecisionState) bool { return s.CurrentEqual }},
		{name: "local_changed", flag: func(s DecisionState) bool { return s.LocalChanged }},
		{name: "remote_changed", flag: func(s DecisionState) bool { return s.RemoteChanged }},
		{name: "local_subtree_changed", flag: func(s DecisionState) bool { return s.LocalSubtreeChanged }},
		{name: "remote_subtree_changed", flag: func(s DecisionState) bool { return s.RemoteSubtreeChanged }},
		{name: "metadata_drift", flag: func(s DecisionState) bool { return s.MetadataDrift }},
	}
}
