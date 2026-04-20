package cli

import (
	"context"
	"log/slog"
	"sort"
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

	groups := groupStatusConditions(snapshot)
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

type statusConditionGroup struct {
	ConditionKey  syncengine.ConditionKey
	ConditionType string
	ScopeKey      syncengine.ScopeKey
	Count         int
	Paths         []string
}

type statusConditionGroupKey struct {
	conditionKey syncengine.ConditionKey
	scopeKey     string
}

type blockedRetryProjection struct {
	count int
	paths []string
}

func groupStatusConditions(snapshot *syncengine.DriveStatusSnapshot) []statusConditionGroup {
	if snapshot == nil {
		return nil
	}

	groupIndex := make(map[statusConditionGroupKey]int)
	groups := make([]statusConditionGroup, 0, len(snapshot.ObservationIssues)+len(snapshot.BlockScopes))

	addStatusObservationConditionGroups(&groups, groupIndex, snapshot.ObservationIssues)
	addStatusBlockScopeGroups(&groups, groupIndex, snapshot.BlockScopes, groupStatusBlockedRetryWork(snapshot.BlockedRetryWork))

	finalizeStatusConditionGroups(groups)

	return groups
}

func addStatusObservationConditionGroups(
	groups *[]statusConditionGroup,
	groupIndex map[statusConditionGroupKey]int,
	observationIssues []syncengine.ObservationIssueRow,
) {
	for i := range observationIssues {
		conditionKey := syncengine.ConditionKeyForObservationIssue(observationIssues[i].IssueType, observationIssues[i].ScopeKey)
		group := ensureStatusConditionGroup(groups, groupIndex, conditionKey, observationIssues[i].IssueType, observationIssues[i].ScopeKey)
		if group == nil {
			continue
		}
		group.Count++
		if observationIssues[i].Path != "" {
			group.Paths = append(group.Paths, observationIssues[i].Path)
		}
	}
}

func addStatusBlockScopeGroups(
	groups *[]statusConditionGroup,
	groupIndex map[statusConditionGroupKey]int,
	blockScopes []*syncengine.BlockScope,
	blockedByScope map[syncengine.ScopeKey]blockedRetryProjection,
) {
	for i := range blockScopes {
		block := blockScopes[i]
		if block == nil {
			continue
		}

		count := blockedByScope[block.Key].count
		if count == 0 {
			count = 1
		}
		conditionKey := syncengine.ConditionKeyForBlockScope(block.ConditionType, block.Key)
		group := ensureStatusConditionGroup(groups, groupIndex, conditionKey, block.ConditionType, block.Key)
		if group == nil {
			continue
		}
		group.Count += count
		if len(blockedByScope[block.Key].paths) > 0 {
			group.Paths = append(group.Paths, blockedByScope[block.Key].paths...)
		}
	}
}

func ensureStatusConditionGroup(
	groups *[]statusConditionGroup,
	groupIndex map[statusConditionGroupKey]int,
	conditionKey syncengine.ConditionKey,
	conditionType string,
	scopeKey syncengine.ScopeKey,
) *statusConditionGroup {
	if conditionKey == "" {
		return nil
	}

	key := statusConditionGroupKey{
		conditionKey: conditionKey,
		scopeKey:     scopeKey.String(),
	}
	if idx, ok := groupIndex[key]; ok {
		return &(*groups)[idx]
	}

	*groups = append(*groups, statusConditionGroup{
		ConditionKey:  conditionKey,
		ConditionType: conditionType,
		ScopeKey:      scopeKey,
	})
	groupIndex[key] = len(*groups) - 1

	return &(*groups)[len(*groups)-1]
}

func groupStatusBlockedRetryWork(rows []syncengine.RetryWorkRow) map[syncengine.ScopeKey]blockedRetryProjection {
	grouped := make(map[syncengine.ScopeKey]blockedRetryProjection)
	for i := range rows {
		scopeKey := rows[i].ScopeKey
		if scopeKey.IsZero() {
			continue
		}

		projection := grouped[scopeKey]
		projection.count++
		if rows[i].Path != "" {
			projection.paths = append(projection.paths, rows[i].Path)
		}
		grouped[scopeKey] = projection
	}

	return grouped
}

func finalizeStatusConditionGroups(groups []statusConditionGroup) {
	for i := range groups {
		sort.Strings(groups[i].Paths)
		groups[i].Paths = uniqueSortedStrings(groups[i].Paths)
	}
}

func uniqueSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}

	result := values[:1]
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			continue
		}
		result = append(result, values[i])
	}

	return result
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
