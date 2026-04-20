package cli

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	statusScopeAccount   = "account"
	statusScopeDrive     = "drive"
	statusScopeDirectory = "directory"
	statusScopeService   = "service"
	statusScopeDisk      = "disk"
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

func statusScopeKindFromScopeKey(scopeKey syncengine.ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case syncengine.ScopeThrottleTarget:
		return statusScopeDrive
	case syncengine.ScopeService:
		return statusScopeService
	case syncengine.ScopeQuotaOwn:
		return statusScopeDrive
	case syncengine.ScopePermRemoteRead, syncengine.ScopePermRemoteWrite:
		return statusScopeDirectory
	case syncengine.ScopePermDirRead, syncengine.ScopePermDirWrite:
		return statusScopeDirectory
	case syncengine.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return "file"
	}
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
		LastSyncTime:     formatStatusSyncTime(snapshot.RunStatus.LastCompletedAt),
		LastSyncDuration: formatStatusDurationMs(snapshot.RunStatus.LastDurationMs),
		FileCount:        snapshot.BaselineEntryCount,
		RemoteDrift:      snapshot.RemoteDriftItems,
		Retrying:         snapshot.RetryingItems,
		LastError:        snapshot.RunStatus.LastError,
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

func buildStatusConditionJSON(
	snapshot *syncengine.DriveStatusSnapshot,
	verbose bool,
	examplesLimit int,
) []statusConditionJSON {
	if snapshot == nil {
		return nil
	}

	groups := syncengine.ProjectStoredConditionGroups(snapshot)
	if len(groups) == 0 {
		return nil
	}

	output := make([]statusConditionJSON, 0, len(groups))
	for i := range groups {
		group := groups[i]
		descriptor := describeStatusCondition(group.ConditionKey)
		output = append(output, statusConditionJSON{
			ConditionKey:  string(group.ConditionKey),
			ConditionType: group.ConditionType,
			Title:         descriptor.Title,
			Reason:        descriptor.Reason,
			Action:        descriptor.Action,
			ScopeKind:     statusScopeKindFromScopeKey(group.ScopeKey),
			Scope:         group.ScopeKey.Humanize(),
			Count:         group.Count,
			Paths:         sampleStrings(group.Paths, verbose, examplesLimit),
		})
	}

	sortStatusConditions(output)

	return output
}

func conditionTotal(groups []statusConditionJSON) int {
	total := 0
	for i := range groups {
		total += groups[i].Count
	}

	return total
}

func sampleStrings(values []string, verbose bool, examplesLimit int) []string {
	if len(values) == 0 {
		return nil
	}
	if verbose || len(values) <= examplesLimit {
		out := make([]string, len(values))
		copy(out, values)
		return out
	}

	out := make([]string, examplesLimit)
	copy(out, values[:examplesLimit])
	return out
}
