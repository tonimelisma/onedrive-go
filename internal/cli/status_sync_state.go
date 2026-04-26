package cli

import (
	"context"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func readDriveStatusSnapshot(
	statePath string,
	logger *slog.Logger,
) syncengine.DriveStatusSnapshot {
	if !managedPathExists(statePath) {
		return syncengine.DriveStatusSnapshot{}
	}

	snapshot, err := syncengine.ReadDriveStatusSnapshot(context.Background(), statePath, logger)
	if err != nil {
		return syncengine.DriveStatusSnapshot{}
	}

	return snapshot
}

func loadShortcutRootStatusSnapshots(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) map[driveid.CanonicalID][]syncengine.ShortcutRootRecord {
	roots := make(map[driveid.CanonicalID][]syncengine.ShortcutRootRecord)
	if cfg == nil {
		return roots
	}
	for cid := range cfg.Drives {
		statePath := config.DriveStatePath(cid)
		if !managedPathExists(statePath) {
			continue
		}
		records, err := syncengine.ReadShortcutRootStatusSnapshot(ctx, statePath, logger)
		if err != nil {
			if logger != nil {
				logger.Debug("skip shortcut root status snapshot",
					"drive", cid.String(),
					"error", err,
				)
			}
			continue
		}
		roots[cid] = records
	}
	return roots
}

func buildSyncStateInfo(
	snapshot *syncengine.DriveStatusSnapshot,
	verbose bool,
	examplesLimit int,
) syncStateInfo {
	if snapshot == nil {
		snapshot = &syncengine.DriveStatusSnapshot{}
	}

	if examplesLimit <= 0 {
		examplesLimit = defaultVisiblePaths
	}

	info := syncStateInfo{
		FileCount:     snapshot.BaselineEntryCount,
		RemoteDrift:   snapshot.RemoteDriftItems,
		Retrying:      snapshot.RetryingItems,
		Conditions:    buildStatusConditionJSON(snapshot, verbose, examplesLimit),
		ExamplesLimit: examplesLimit,
		Verbose:       verbose,
	}

	info.ConditionCount = conditionTotal(info.Conditions)

	return info
}
