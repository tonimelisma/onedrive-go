package cli

import (
	"context"
	"log/slog"
	"strconv"
	"time"

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
		LastSyncTime:     formatStatusSyncTime(snapshot.SyncStatus.LastSyncedAt),
		LastSyncDuration: formatStatusDurationMs(snapshot.SyncStatus.LastSyncDurationMs),
		FileCount:        snapshot.BaselineEntryCount,
		RemoteDrift:      snapshot.RemoteDriftItems,
		Retrying:         snapshot.RetryingItems,
		LastError:        snapshot.SyncStatus.LastError,
		Conditions:       buildStatusConditionJSON(snapshot, verbose, examplesLimit),
		ExamplesLimit:    examplesLimit,
		Verbose:          verbose,
	}

	info.ConditionCount = conditionTotal(info.Conditions)

	return info
}

func formatStatusSyncTime(unixNano int64) string {
	if unixNano <= 0 {
		return ""
	}

	return time.Unix(0, unixNano).UTC().Format(time.RFC3339)
}

func formatStatusDurationMs(durationMs int64) string {
	if durationMs <= 0 {
		return ""
	}

	return strconv.FormatInt(durationMs, 10)
}
