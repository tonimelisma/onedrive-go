package sync

import (
	"context"
	"log/slog"
)

// remotePathLookup resolves an item ID to the path the engine's remote_state
// row stores, for deleted items the baseline does not hold (R-2.4.17). A
// lookup failure reads as unknown; the delete is then skipped as before.
func (eng *Engine) remotePathLookup(ctx context.Context) func(itemID string) (string, bool) {
	return func(itemID string) (string, bool) {
		if eng == nil || eng.baseline == nil {
			return "", false
		}
		row, found, err := eng.baseline.GetRemoteStateByID(ctx, eng.driveID, itemID)
		if err != nil {
			eng.logger.Debug("remote_state lookup for a deleted item failed",
				slog.String("item_id", itemID),
				slog.Any("error", err),
			)

			return "", false
		}
		if !found || row == nil {
			return "", false
		}

		return row.Path, true
	}
}
