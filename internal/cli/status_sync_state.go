package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func readDriveStatusSnapshot(
	ctx context.Context,
	statePath string,
	logger *slog.Logger,
) syncengine.DriveStatusSnapshot {
	if !managedPathExists(statePath) {
		return syncengine.DriveStatusSnapshot{}
	}

	snapshot, err := syncengine.ReadDriveStatusSnapshot(ctx, statePath, logger)
	if err != nil {
		// Without this, an unreadable state database looked like a drive
		// with nothing to report: zero files and no issues.
		if logger != nil {
			logger.Warn("cannot read the sync state for this drive; its status may be incomplete",
				slog.String("path", statePath), slog.String("error", err.Error()))
		}
		return syncengine.DriveStatusSnapshot{}
	}

	return snapshot
}

func loadShortcutRootStatusSnapshots(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView {
	roots := make(map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView)
	if cfg == nil {
		return roots
	}
	for cid := range cfg.Drives {
		statePath := config.DriveStatePath(cid)
		if !managedPathExists(statePath) {
			continue
		}
		records, err := syncengine.ReadShortcutRootStatusSnapshot(ctx, statePath, cid.String(), cfg.Drives[cid].SyncDir, logger)
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
		FileCount:     snapshot.BaselineFileCount,
		RemoteDrift:   snapshot.RemoteDriftItems,
		Retrying:      snapshot.RetryingItems,
		Conditions:    buildStatusConditionJSON(snapshot, verbose, examplesLimit),
		ExamplesLimit: examplesLimit,
		Verbose:       verbose,
	}

	info.RetryingPaths = retryingPaths(snapshot.RetryingWork, verbose, examplesLimit)
	info.ConditionCount = conditionTotal(info.Conditions)

	return info
}

// retryingPaths names the work behind the "Retrying" count, which on its own
// gave no way to find out what kept failing.
func retryingPaths(rows []syncengine.RetryWorkRow, verbose bool, limit int) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		if !verbose && len(paths) >= limit {
			break
		}
		path := rows[i].Path
		if rows[i].OldPath != "" && rows[i].OldPath != rows[i].Path {
			path = rows[i].OldPath + " -> " + rows[i].Path
		}
		paths = append(paths, fmt.Sprintf("%s (%s, %d attempts)", path, rows[i].ActionType.String(), rows[i].AttemptCount))
	}

	return paths
}
