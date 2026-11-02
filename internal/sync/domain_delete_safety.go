package sync

import "time"

// Delete-safety vocabulary shared by the planner, which decides the hold
// (planner_delete_safety.go), the store, which records it for status, and the
// engine report. The policy itself lives with the planner.

// deleteSafetySide names the side a held delete would remove content from.
type deleteSafetySide string

const (
	deleteSafetySideLocal  deleteSafetySide = "local"
	deleteSafetySideRemote deleteSafetySide = "remote"
)

// deleteSafetyReason records which rule held a direction's deletes.
type deleteSafetyReason string

const (
	// deleteSafetyReasonSideVanished: the opposite side has no entries.
	deleteSafetyReasonSideVanished deleteSafetyReason = "side_vanished"
	// deleteSafetyReasonTooMany: the proportional threshold was exceeded.
	deleteSafetyReasonTooMany deleteSafetyReason = "too_many"
)

// deleteSafetyWindow is how long an executed, unconfirmed delete keeps
// counting toward the proportional rule. The hold used to judge each plan
// alone, so a process removing files below the threshold per pass (a failing
// disk, a misbehaving app, a cron job) emptied a drive one small batch at a
// time and was never held. Deletes the user confirmed with
// `sync --confirm-deletes` are not counted: the confirmation reviewed them.
const deleteSafetyWindow = 24 * time.Hour

// heldDelete is one delete the hold kept out of the executable plan.
type heldDelete struct {
	Path     string
	Side     deleteSafetySide
	Reason   deleteSafetyReason
	ItemType ItemType
	// ItemID binds a confirmation to the item that was held, so the same
	// path holding a different item later is not covered by it.
	ItemID string
}

func deleteSafetySideFor(deleteType actionType) deleteSafetySide {
	if deleteType == ActionLocalDelete {
		return deleteSafetySideLocal
	}
	return deleteSafetySideRemote
}

// countHeldDeletesBySide splits held deletes into the side each would remove
// content from, for the pass report.
func countHeldDeletesBySide(held []heldDelete) (local int, remote int) {
	for i := range held {
		if held[i].Side == deleteSafetySideLocal {
			local++
			continue
		}
		remote++
	}
	return local, remote
}

// heldDeleteSides lists the distinct sides of held, in order of first
// appearance.
func heldDeleteSides(held []heldDelete) []deleteSafetySide {
	var sides []deleteSafetySide
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
