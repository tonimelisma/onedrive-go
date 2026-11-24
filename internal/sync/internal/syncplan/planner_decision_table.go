package syncplan

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// sideTypeCount is the number of SideType members.
const sideTypeCount = int(syncdomain.SideRoot) + 1

// Decision is the decision table's answer for one path: the comparison kind,
// which move composition also reads, and what to do.
type Decision struct {
	Comparison     ComparisonKind
	Reconciliation ReconciliationKind
}

// Decide is the decision table: the comparison kind of a state, then what to
// do about it. Pure and total: a state no rule covers (no side present) is
// unclassified, and planning refuses it (R-6.2.16).
func Decide(s syncdomain.DecisionState) Decision {
	comparison := classify(s)

	return Decision{Comparison: comparison, Reconciliation: reconcile(comparison, reconcileInputsOf(s))}
}

// classify is the first stage: the comparison kind. The rules are tried in
// order; the first that holds decides.
func classify(s syncdomain.DecisionState) ComparisonKind {
	if s.Baseline.Present() {
		return classifySynced(s)
	}

	return classifyUnsynced(s)
}

// classifySynced classifies a path the baseline holds.
func classifySynced(s syncdomain.DecisionState) ComparisonKind {
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
func classifyUnsynced(s syncdomain.DecisionState) ComparisonKind {
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
func classifyLocalOnly(s syncdomain.DecisionState) ComparisonKind {
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
func classifyRemoteOnly(s syncdomain.DecisionState) ComparisonKind {
	switch {
	case s.RemoteMoveShadowed:
		return ComparisonRemoteMoveShadowed
	case s.RemoteCreateAwaitsAncestorMove:
		return comparisonDeleteAwaitsAncestorMove
	default:
		return ComparisonRemoteOnlyCreate
	}
}

// reconcileInputs is what the second stage reads besides the comparison kind:
// the path's item type, each side's type and the flags.
type reconcileInputs struct {
	ItemType, Local, Remote syncdomain.SideType

	LocalChanged, RemoteChanged               bool
	LocalSubtreeChanged, RemoteSubtreeChanged bool
	LocalTypeChanged, RemoteTypeChanged       bool
	RemoteItemReplaced                        bool
	MetadataDrift                             bool
}

// reconcileInputsOf derives the second stage's inputs from a state.
func reconcileInputsOf(s syncdomain.DecisionState) reconcileInputs {
	return reconcileInputs{
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

// reconcile is the second stage: what to do about a path of the given
// comparison kind. Every genuine "nothing to do" is an explicit noop; a kind
// no rule covers is unclassified, and planning fails on it (R-6.2.16).
func reconcile(kind ComparisonKind, in reconcileInputs) ReconciliationKind {
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
		// Decided by reconcile itself from the flags.
		return ReconcileUnclassified
	default:
		return ReconcileUnclassified
	}
}

// reconcileCreate decides a create on one side: a folder is created, a file
// transferred. A root-typed entry is no item either side creates (the
// observers never report one as new), so it is unclassified and planning
// refuses it rather than transferring a drive root.
func reconcileCreate(itemType syncdomain.SideType, folder, file ReconciliationKind) ReconciliationKind {
	switch itemType {
	case syncdomain.SideFolder:
		return folder
	case syncdomain.SideFile:
		return file
	case syncdomain.SideAbsent, syncdomain.SideRoot:
		return ReconcileUnclassified
	default:
		return ReconcileUnclassified
	}
}

// reconcileDiverged decides a path that changed on some side and whose two
// sides differ.
func reconcileDiverged(in reconcileInputs) ReconciliationKind {
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
func reconcileDivergedTypes(in reconcileInputs) (ReconciliationKind, bool) {
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
func reconcileLocalMissing(in reconcileInputs) ReconciliationKind {
	switch {
	// A folder deleted locally while OneDrive replaced it (a new item at the
	// same path, or a new type) is not the folder the user deleted.
	case in.Remote == syncdomain.SideFolder && (in.RemoteTypeChanged || in.RemoteItemReplaced):
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
func reconcileRemoteMissing(in reconcileInputs) ReconciliationKind {
	switch {
	case in.LocalTypeChanged && in.Local == syncdomain.SideFolder:
		return ReconcileFolderCreateRemote
	// A folder with a new filesystem identity but nothing changed below it
	// accepts OneDrive's delete too (D-057).
	case !in.LocalChanged:
		return ReconcileLocalDelete
	default:
		return ReconcileConflictEditDelete
	}
}
