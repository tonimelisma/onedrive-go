package cli

import (
	"context"
	"log/slog"

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
