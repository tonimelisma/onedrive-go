package sync

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

// heldDelete is one delete the hold kept out of the executable plan.
type heldDelete struct {
	Path     string
	Side     deleteSafetySide
	Reason   deleteSafetyReason
	ItemType ItemType
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
