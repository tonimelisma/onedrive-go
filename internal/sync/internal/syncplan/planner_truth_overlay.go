package syncplan

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func plannerSuppressesUnavailableTruth(status *syncdomain.PathTruthStatus) bool {
	return status.SuppressesStructuralActions()
}
