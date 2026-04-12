package synctest

import (
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// EmptyBaseline returns a baseline with no entries.
func EmptyBaseline() *syncstore.Baseline {
	return syncstore.NewBaselineForTest(nil)
}

// BaselineWith returns a baseline pre-populated with the provided entries.
func BaselineWith(entries ...*synctypes.BaselineEntry) *syncstore.Baseline {
	return syncstore.NewBaselineForTest(entries)
}

// ActionsOfType filters a flat action slice to the requested action type.
func ActionsOfType(actions []synctypes.Action, actionType synctypes.ActionType) []synctypes.Action {
	filtered := make([]synctypes.Action, 0, len(actions))
	for i := range actions {
		if actions[i].Type == actionType {
			filtered = append(filtered, actions[i])
		}
	}
	return filtered
}
