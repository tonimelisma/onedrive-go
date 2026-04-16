package cli

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	stateStoreStatusHealthy = "healthy"
	stateStoreStatusMissing = "missing"
	stateStoreStatusDamaged = "damaged"
	statusScopeAccount      = "account"
	statusScopeDrive        = "drive"
	statusScopeDirectory    = "directory"
	statusScopeService      = "service"
	statusScopeDisk         = "disk"
)

type driveStateStoreInfo struct {
	Status       string
	Error        string
	RecoveryHint string
}

func readDriveStatusSnapshot(
	statePath string,
	logger *slog.Logger,
	history bool,
	canonicalID string,
) (syncengine.DriveStatusSnapshot, driveStateStoreInfo) {
	if !managedPathExists(statePath) {
		return syncengine.DriveStatusSnapshot{}, driveStateStoreInfo{
			Status: stateStoreStatusMissing,
		}
	}

	snapshot, err := syncengine.ReadDriveStatusSnapshot(context.Background(), statePath, history, logger)
	if err != nil {
		return syncengine.DriveStatusSnapshot{}, driveStateStoreInfo{
			Status:       stateStoreStatusDamaged,
			Error:        err.Error(),
			RecoveryHint: recoverAwareStateStoreHint(canonicalID),
		}
	}

	return snapshot, driveStateStoreInfo{Status: stateStoreStatusHealthy}
}

func statusScopeKindFromScopeKey(scopeKey syncengine.ScopeKey) string {
	if scopeKey.IsZero() {
		return ""
	}

	switch scopeKey.Kind {
	case syncengine.ScopeAuthAccount, syncengine.ScopeThrottleAccount:
		return statusScopeAccount
	case syncengine.ScopeThrottleTarget:
		return statusScopeDrive
	case syncengine.ScopeService:
		return statusScopeService
	case syncengine.ScopeQuotaOwn:
		return statusScopeDrive
	case syncengine.ScopePermRemote:
		return statusScopeDirectory
	case syncengine.ScopePermDir:
		return statusScopeDirectory
	case syncengine.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return "file"
	}
}

func buildSyncStateInfo(
	snapshot *syncengine.DriveStatusSnapshot,
	storeInfo driveStateStoreInfo,
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
		LastSyncTime:           formatStatusSyncTime(snapshot.RunStatus.LastCompletedAt),
		LastSyncDuration:       formatStatusDurationMs(snapshot.RunStatus.LastDurationMs),
		FileCount:              snapshot.BaselineEntryCount,
		RemoteDrift:            snapshot.RemoteDriftItems,
		Retrying:               snapshot.RetryingItems,
		LastError:              snapshot.RunStatus.LastError,
		IssueGroups:            buildFailureGroupJSON(snapshot.IssueGroups, verbose, examplesLimit),
		StateStoreStatus:       storeInfo.Status,
		StateStoreError:        storeInfo.Error,
		StateStoreRecoveryHint: storeInfo.RecoveryHint,
		ExamplesLimit:          examplesLimit,
		Verbose:                verbose,
	}

	info.IssueCount = issueGroupTotal(info.IssueGroups)

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

func buildFailureGroupJSON(
	groups []syncengine.IssueGroupSnapshot,
	verbose bool,
	examplesLimit int,
) []failureGroupJSON {
	if len(groups) == 0 {
		return nil
	}

	output := make([]failureGroupJSON, 0, len(groups))
	for i := range groups {
		group := groups[i]
		descriptor := describeStatusSummary(group.SummaryKey)
		output = append(output, failureGroupJSON{
			SummaryKey: string(group.SummaryKey),
			IssueType:  group.PrimaryIssueType,
			Title:      descriptor.Title,
			Reason:     descriptor.Reason,
			Action:     descriptor.Action,
			ScopeKind:  statusScopeKindFromScopeKey(group.ScopeKey),
			Scope:      group.ScopeLabel,
			Count:      group.Count,
			Paths:      sampleStrings(group.Paths, verbose, examplesLimit),
		})
	}

	sortStatusFailureGroups(output)

	return output
}

func issueGroupTotal(groups []failureGroupJSON) int {
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
