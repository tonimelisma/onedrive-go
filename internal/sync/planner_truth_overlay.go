package sync

func plannerSuppressesUnavailableTruth(status *PathTruthStatus) bool {
	return status.SuppressesStructuralActions()
}
