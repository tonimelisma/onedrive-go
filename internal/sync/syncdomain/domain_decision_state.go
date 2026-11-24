package syncdomain

import (
	"errors"
	"fmt"
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
		panic(fmt.Sprintf("syncdomain: side type of %s", OutsideTheEnum(itemType)))
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

// errInvalidDecisionState reports a state the comparison query cannot
// produce: a fact about a side the path does not have, or one that
// contradicts the side types.
var errInvalidDecisionState = errors.New("syncdomain: decision state contradicts its side types")

// Valid reports whether the comparison query can produce s: each rule is one
// line of a flag's definition there (a flag about an absent side is false; a
// type change is a content change; a folder has no content to change; ...).
// The decision table is held to its properties over the valid states. The
// rules are evaluated into an array on the stack, so a check allocates
// nothing however often the planner and its tests call it.
func (s DecisionState) Valid() error {
	for _, rule := range decisionStateChecks(s) {
		if !rule.holds {
			return fmt.Errorf("%w: %s", errInvalidDecisionState, rule.name)
		}
	}

	return nil
}

// decisionStateCheck is one line of the flag definitions, evaluated for one
// state.
type decisionStateCheck struct {
	name  string
	holds bool
}

// decisionStateRuleCount is how many lines the flag definitions have.
const decisionStateRuleCount = 22

func implies(a, b bool) bool { return !a || b }

// decisionStateChecks evaluates every line of the flag definitions for s.
// Each line is one boolean rule, so the count of branches is the count of
// rules' && and ||, not nested logic. gocyclo counts them as branches; a
// table of predicate closures is counted the same (gocyclo charges a
// function literal's branches to the function that holds it), and 22 named
// predicates would scatter one definition over 22 functions, so the flat
// list stays and carries the exception.
//
//nolint:gocyclo // a flat list of 22 one-line rules; see above
func decisionStateChecks(s DecisionState) [decisionStateRuleCount]decisionStateCheck {
	return [decisionStateRuleCount]decisionStateCheck{
		// The content-change flags.
		{
			name:  "local_changed needs baseline and local",
			holds: implies(s.LocalChanged, s.Baseline.Present() && s.Local.Present()),
		},
		{
			name:  "remote_changed needs baseline and remote",
			holds: implies(s.RemoteChanged, s.Baseline.Present() && s.Remote.Present()),
		},
		{
			name:  "a local type change is a local change",
			holds: implies(s.LocalTypeChanged(), s.LocalChanged),
		},
		{
			name:  "a remote type change is a remote change",
			holds: implies(s.RemoteTypeChanged(), s.RemoteChanged),
		},
		{
			name:  "a synced folder that is still a folder locally has no local change",
			holds: implies(s.Baseline == SideFolder && s.Local == SideFolder, !s.LocalChanged),
		},
		{
			name:  "a synced folder that is still a folder online has no remote change",
			holds: implies(s.Baseline == SideFolder && s.Remote == SideFolder, !s.RemoteChanged),
		},
		// The side facts.
		{
			name:  "local_item_replaced needs baseline and local",
			holds: implies(s.LocalItemReplaced, s.Baseline.Present() && s.Local.Present()),
		},
		{
			name:  "remote_item_replaced needs baseline and remote",
			holds: implies(s.RemoteItemReplaced, s.Baseline.Present() && s.Remote.Present()),
		},
		{
			name: "local_subtree_changed is asked of a folder OneDrive replaced with a file",
			holds: implies(s.LocalSubtreeChanged, s.Baseline == SideFolder && s.Local == SideFolder &&
				s.Remote.Present() && s.Remote != SideFolder),
		},
		{
			name: "remote_subtree_changed is asked of a folder the user replaced with a file",
			holds: implies(s.RemoteSubtreeChanged, s.Baseline == SideFolder && s.Remote == SideFolder &&
				s.Local.Present() && s.Local != SideFolder),
		},
		{
			name:  "kept_local_folder is an unsynced local-only folder",
			holds: implies(s.KeptLocalFolder, !s.Baseline.Present() && !s.Remote.Present() && s.Local == SideFolder),
		},
		{
			name:  "current_equal needs both sides of one type",
			holds: implies(s.CurrentEqual, s.Local.Present() && s.Local == s.Remote),
		},
		{
			name:  "two folders are equal",
			holds: implies(s.Local == SideFolder && s.Remote == SideFolder, s.CurrentEqual),
		},
		{
			name:  "metadata_drift needs all three sides",
			holds: implies(s.MetadataDrift, s.Baseline.Present() && s.Local.Present() && s.Remote.Present()),
		},
		// The placement flags.
		{
			name:  "a local move source is a baseline path",
			holds: implies(s.LocalMoveSource, s.Baseline.Present()),
		},
		{
			name:  "a local move destination is an unsynced local path",
			holds: implies(s.LocalMoveDest, s.Local.Present() && !s.Baseline.Present()),
		},
		{
			name:  "a remote move source still holds the synced item locally",
			holds: implies(s.RemoteMoveSource, s.Baseline.Present() && s.Local == s.Baseline),
		},
		{
			name:  "a remote move destination is a remote path no unsynced local entry holds",
			holds: implies(s.RemoteMoveDest, s.Remote.Present() && implies(s.Local.Present(), s.Baseline.Present())),
		},
		{
			name:  "a shadowed remote move is a remote path",
			holds: implies(s.RemoteMoveShadowed, s.Remote.Present()),
		},
		{
			name:  "a remote create waiting for an ancestor's move is a remote path",
			holds: implies(s.RemoteCreateAwaitsAncestorMove, s.Remote.Present()),
		},
		{
			name:  "a local copy waiting for an ancestor's move is a local path",
			holds: implies(s.LocalCopyAwaitsAncestorMove, s.Local.Present()),
		},
		{
			name:  "a delete waiting for an ancestor's move is a synced path gone locally",
			holds: implies(s.DeleteAwaitsAncestorMove, s.Baseline.Present() && !s.Local.Present()),
		},
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

// PlannerDecisionInput is one input of the decision table as the plan error
// prints it (DecisionState.Inputs).
type PlannerDecisionInput struct {
	Column string
	// Value is a side's item type (empty when absent) or a flag as 0 or 1.
	Value string
}
