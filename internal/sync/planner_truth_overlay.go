package sync

func plannerSuppressesUnavailableTruth(status *pathTruthStatus) bool {
	return status.SuppressesStructuralActions()
}
