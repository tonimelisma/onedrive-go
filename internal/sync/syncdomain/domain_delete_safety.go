package syncdomain

import "time"

// Delete-safety vocabulary shared by the planner, which decides the hold
// (planner_delete_safety.go), the store, which records it for status, and the
// engine report. The policy itself lives with the planner.

// DeleteSafetySide names the side a held delete would remove content from.
type DeleteSafetySide string

const (
	DeleteSafetySideLocal  DeleteSafetySide = "local"
	DeleteSafetySideRemote DeleteSafetySide = "remote"
)

// DeleteSafetyReason records which rule held a direction's deletes.
type DeleteSafetyReason string

const (
	// DeleteSafetyReasonSideVanished: the opposite side has no entries.
	DeleteSafetyReasonSideVanished DeleteSafetyReason = "side_vanished"
	// DeleteSafetyReasonTooMany: the proportional threshold was exceeded.
	DeleteSafetyReasonTooMany DeleteSafetyReason = "too_many"
	// DeleteSafetyReasonMountPointGone: the delete removes a recorded mount
	// point whose folder is gone from the sync folder, or something below
	// it, so it is held however few such deletes there are.
	DeleteSafetyReasonMountPointGone DeleteSafetyReason = "mount_point_gone"
)

// DeleteSafetyWindow is how long an executed, unconfirmed delete keeps
// counting toward the proportional rule. A hold that judged each plan alone
// would never stop a process that removes files below the threshold on every
// pass (a failing disk, a misbehaving app, a cron job) and so empties a drive
// one small batch at a time. Deletes the user confirmed with
// `sync --confirm-deletes` are not counted: the confirmation reviewed them.
const DeleteSafetyWindow = 24 * time.Hour

// HeldDelete is one delete the hold kept out of the executable plan.
type HeldDelete struct {
	Path     string
	Side     DeleteSafetySide
	Reason   DeleteSafetyReason
	ItemType ItemType
	// ItemID binds a confirmation to the item that was held, so the same
	// path holding a different item later is not covered by it.
	ItemID string
}

// DeleteSafetySideOf reports whether an action type deletes a synced item, and
// the side it removes content from: the planner's hold and its confirmation
// matching, and the store's delete ledger, all ask it. A cleanup removes only
// the baseline row of an item both sides already lost, so it is no such
// delete.
func DeleteSafetySideOf(action ActionType) (DeleteSafetySide, bool) {
	switch action {
	case ActionLocalDelete:
		return DeleteSafetySideLocal, true
	case ActionRemoteDelete:
		return DeleteSafetySideRemote, true
	case ActionDownload, ActionUpload, ActionLocalMove, ActionRemoteMove, ActionFolderCreate,
		ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return "", false
	default:
		panic(OutsideTheEnum(action))
	}
}

// CountHeldDeletesBySide splits held deletes into the side each would remove
// content from, for the pass report.
func CountHeldDeletesBySide(held []HeldDelete) (local int, remote int) {
	for i := range held {
		if held[i].Side == DeleteSafetySideLocal {
			local++
			continue
		}
		remote++
	}
	return local, remote
}

// HeldDeleteSides lists the distinct sides of held, in order of first
// appearance.
func HeldDeleteSides(held []HeldDelete) []DeleteSafetySide {
	var sides []DeleteSafetySide
	for i := range held {
		seen := false
		for _, side := range sides {
			if side == held[i].Side {
				seen = true
				break
			}
		}
		if !seen {
			sides = append(sides, held[i].Side)
		}
	}
	return sides
}
