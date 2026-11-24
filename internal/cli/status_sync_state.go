package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// readDriveStatusSnapshot reads one drive's status snapshot. A drive with no
// state database has an empty snapshot: it has never synced here. A state
// database that cannot be checked or read is returned as an error, after one
// Warn, so the caller renders the drive as unreadable rather than as a drive
// with zero files and no issues. One another onedrive-go version wrote is
// returned without the Warn: it is expected after an upgrade or a downgrade,
// and status names it (storeVersionOf).
func readDriveStatusSnapshot(
	ctx context.Context,
	statePath string,
	logger *slog.Logger,
	filter syncdomain.ContentFilterConfig,
) (syncstore.DriveStatusSnapshot, error) {
	exists, err := managedPathExists(statePath)
	if err == nil && !exists {
		return syncstore.DriveStatusSnapshot{}, nil
	}

	var snapshot syncstore.DriveStatusSnapshot
	if err == nil {
		snapshot, err = syncstore.ReadDriveStatusSnapshotWithFilter(ctx, statePath, logger, filter)
	}
	if err != nil {
		if logger != nil && storeVersionOf(err) == "" {
			logger.Warn("cannot read the sync state for this drive; its status is unavailable",
				slog.String("path", statePath), slog.Any("error", err))
		}
		return syncstore.DriveStatusSnapshot{}, fmt.Errorf("read sync state %s: %w", statePath, err)
	}

	return snapshot, nil
}

// storeVersionOf is storeVersionOlder or storeVersionNewer when err says
// another onedrive-go version wrote the state database, else "".
func storeVersionOf(err error) string {
	var incompatibleErr *syncstore.StateStoreIncompatibleError
	if !errors.As(err, &incompatibleErr) {
		return ""
	}

	switch incompatibleErr.Reason {
	case syncstore.StateStoreIncompatibleReasonOlderVersion:
		return storeVersionOlder
	case syncstore.StateStoreIncompatibleReasonNewerVersion:
		return storeVersionNewer
	case syncstore.StateStoreIncompatibleReasonOpenFailed,
		syncstore.StateStoreIncompatibleReasonIncompatibleSchema,
		syncstore.StateStoreIncompatibleReasonCorrupt:
		return ""
	default:
		return ""
	}
}

func loadShortcutRootStatusSnapshots(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) map[driveid.CanonicalID][]syncshortcut.ShortcutRootStatusView {
	roots := make(map[driveid.CanonicalID][]syncshortcut.ShortcutRootStatusView)
	if cfg == nil {
		return roots
	}
	for cid := range cfg.Drives {
		statePath := config.DriveStatePath(cid)
		exists, err := managedPathExists(statePath)
		if err != nil {
			// Debug: the drive's own row reports the unreadable state
			// database (readDriveStatusSnapshot warns once for it).
			if logger != nil {
				logger.Debug("skip shortcut root status snapshot: cannot check the state database",
					"drive", cid.String(),
					"error", err,
				)
			}
			continue
		}
		if !exists {
			continue
		}
		records, err := syncstore.ReadShortcutRootStatusSnapshot(ctx, statePath, cid.String(), cfg.Drives[cid].SyncDir, logger)
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
	snapshot *syncstore.DriveStatusSnapshot,
	root statusStoreRoot,
	verbose bool,
	examplesLimit int,
) syncStateInfo {
	if snapshot == nil {
		snapshot = &syncstore.DriveStatusSnapshot{}
	}

	if examplesLimit <= 0 {
		examplesLimit = defaultVisiblePaths
	}

	info := syncStateInfo{
		FileCount:     snapshot.BaselineFileCount,
		RemoteDrift:   snapshot.RemoteDriftItems,
		LocalDrift:    snapshot.LocalDriftItems,
		Retrying:      snapshot.RetryingItems,
		Conditions:    buildStatusConditionJSON(snapshot, root, verbose, examplesLimit),
		ExamplesLimit: examplesLimit,
		Verbose:       verbose,
		NeverSynced:   snapshot.NeverSynced,
	}

	info.RetryingPaths = retryingPaths(snapshot.RetryingWork, verbose, examplesLimit)
	info.ConditionCount = conditionTotal(info.Conditions)

	return info
}

// retryingPaths names the work behind the "Retrying" count, which on its own
// gave no way to find out what kept failing.
func retryingPaths(rows []syncdomain.RetryWorkRow, verbose bool, limit int) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		if !verbose && len(paths) >= limit {
			break
		}
		path := rows[i].Path
		if rows[i].OldPath != "" && rows[i].OldPath != rows[i].Path {
			path = rows[i].OldPath + " -> " + rows[i].Path
		}
		paths = append(paths, fmt.Sprintf("%s (%s, %s)", path, rows[i].ActionType.String(), countOf(rows[i].AttemptCount, "attempt")))
	}

	return paths
}
