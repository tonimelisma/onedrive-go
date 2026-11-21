package sync

import (
	"context"
	"log/slog"
)

// remotePathLookup resolves an item ID to the path the engine's remote_state
// row stores, for deleted items the baseline does not hold (R-2.4.17). A
// lookup failure reads as unknown; the delete is then skipped as before.
func (e *Engine) remotePathLookup(ctx context.Context) func(itemID string) (string, bool) {
	return func(itemID string) (string, bool) {
		if e == nil || e.baseline == nil {
			return "", false
		}
		row, found, err := e.baseline.GetRemoteStateByID(ctx, e.driveID, itemID)
		if err != nil {
			e.logger.Debug("remote_state lookup for a deleted item failed",
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
