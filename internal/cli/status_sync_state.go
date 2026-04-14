package cli

import (
	"context"
	"log/slog"
	"strings"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	stateStoreStatusHealthy = "healthy"
	stateStoreStatusMissing = "missing"
	stateStoreStatusDamaged = "damaged"
	statusScopeAccount      = "account"
	statusScopeDrive        = "drive"
	statusScopeShortcut     = "shortcut"
	statusScopeDirectory    = "directory"
	statusScopeService      = "service"
	statusScopeDisk         = "disk"
)

type deleteSafetyJSON struct {
	Path       string `json:"path"`
	State      string `json:"state"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
	ActionHint string `json:"action_hint,omitempty"`
}

type statusConflictJSON struct {
	ID                  string `json:"id"`
	Path                string `json:"path"`
	ConflictType        string `json:"conflict_type"`
	DetectedAt          string `json:"detected_at"`
	State               string `json:"state"`
	RequestedResolution string `json:"requested_resolution,omitempty"`
	LastRequestedAt     string `json:"last_requested_at,omitempty"`
	LastRequestError    string `json:"last_request_error,omitempty"`
	ActionHint          string `json:"action_hint,omitempty"`
}

type statusConflictHistoryJSON struct {
	ID           string `json:"id"`
	Path         string `json:"path"`
	ConflictType string `json:"conflict_type"`
	DetectedAt   string `json:"detected_at"`
	Resolution   string `json:"resolution"`
	ResolvedAt   string `json:"resolved_at,omitempty"`
	ResolvedBy   string `json:"resolved_by,omitempty"`
}

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
		if scopeKey.IsThrottleShared() {
			return statusScopeShortcut
		}

		return statusScopeDrive
	case syncengine.ScopeService:
		return statusScopeService
	case syncengine.ScopeQuotaOwn:
		return statusScopeDrive
	case syncengine.ScopeQuotaShortcut, syncengine.ScopePermRemoteWrite:
		return statusScopeShortcut
	case syncengine.ScopePermLocalRead, syncengine.ScopePermLocalWrite:
		return statusScopeDirectory
	case syncengine.ScopeDiskLocal:
		return statusScopeDisk
	default:
		return "file"
	}
}

func buildSyncStateInfo(
	canonicalID string,
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

	nextActions := newStatusActionHintSet()
	nextActions.add(storeInfo.RecoveryHint)

	info := syncStateInfo{
		LastSyncTime:           snapshot.SyncMetadata["last_sync_time"],
		LastSyncDuration:       snapshot.SyncMetadata["last_sync_duration_ms"],
		FileCount:              snapshot.BaselineEntryCount,
		RemoteDrift:            snapshot.RemoteDriftItems,
		Retrying:               snapshot.RetryingItems,
		LastError:              snapshot.SyncMetadata["last_sync_error"],
		IssueGroups:            buildFailureGroupJSON(snapshot.IssueGroups, verbose, examplesLimit, nextActions),
		DeleteSafetyTotal:      len(snapshot.DeleteSafety),
		ConflictsTotal:         len(snapshot.Conflicts),
		ConflictHistoryTotal:   len(snapshot.ConflictHistory),
		StateStoreStatus:       storeInfo.Status,
		StateStoreError:        storeInfo.Error,
		StateStoreRecoveryHint: storeInfo.RecoveryHint,
		ExamplesLimit:          examplesLimit,
		Verbose:                verbose,
	}

	info.IssueCount = issueGroupTotal(info.IssueGroups)
	info.DeleteSafety = buildDeleteSafetyJSON(canonicalID, snapshot.DeleteSafety, verbose, examplesLimit, &info, nextActions)
	info.Conflicts = buildConflictJSON(canonicalID, snapshot.Conflicts, verbose, examplesLimit, &info, nextActions)
	info.ConflictHistory = buildConflictHistoryJSON(snapshot.ConflictHistory, verbose, examplesLimit)
	info.NextActions = nextActions.slice()

	return info
}

func buildFailureGroupJSON(
	groups []syncengine.IssueGroupSnapshot,
	verbose bool,
	examplesLimit int,
	nextActions *statusActionHintSet,
) []failureGroupJSON {
	if len(groups) == 0 {
		return nil
	}

	output := make([]failureGroupJSON, 0, len(groups))
	for i := range groups {
		group := groups[i]
		descriptor := describeStatusSummary(group.SummaryKey)
		nextActions.add(strings.TrimSpace(descriptor.Action))
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

func buildDeleteSafetyJSON(
	canonicalID string,
	rows []syncengine.DeleteSafetySnapshot,
	verbose bool,
	examplesLimit int,
	info *syncStateInfo,
	nextActions *statusActionHintSet,
) []deleteSafetyJSON {
	if len(rows) == 0 {
		return nil
	}

	sampled := sampleDeleteSafetyRows(rows, verbose, examplesLimit)
	output := make([]deleteSafetyJSON, 0, len(sampled))
	for i := range rows {
		switch rows[i].State {
		case stateHeldDeleteHeld:
			info.HeldDeletesWaiting++
		case stateHeldDeleteApproved:
			info.ApprovedDeletesWaiting++
		}
	}
	for i := range sampled {
		row := sampled[i]
		actionHint := deleteSafetyActionHint(canonicalID, row.State)
		nextActions.add(actionHint)
		output = append(output, deleteSafetyJSON{
			Path:       row.Path,
			State:      row.State,
			LastSeenAt: formatNanoTimestamp(row.LastSeenAt),
			ApprovedAt: formatNanoTimestamp(row.ApprovedAt),
			ActionHint: actionHint,
		})
	}

	return output
}

func buildConflictJSON(
	canonicalID string,
	rows []syncengine.ConflictStatusSnapshot,
	verbose bool,
	examplesLimit int,
	info *syncStateInfo,
	nextActions *statusActionHintSet,
) []statusConflictJSON {
	if len(rows) == 0 {
		return nil
	}

	sampled := sampleConflictRows(rows, verbose, examplesLimit)
	output := make([]statusConflictJSON, 0, len(sampled))
	for i := range rows {
		switch conflictRequestDisplayState(rows[i]) {
		case syncengine.ConflictStateQueued:
			info.QueuedConflictRequests++
		case syncengine.ConflictStateApplying:
			info.ApplyingConflictRequests++
		}
	}

	for i := range sampled {
		row := sampled[i]
		conflict := statusConflictJSON{
			ID:                  row.ID,
			Path:                row.Path,
			ConflictType:        row.ConflictType,
			DetectedAt:          formatNanoTimestamp(row.DetectedAt),
			State:               conflictRequestDisplayState(row),
			RequestedResolution: row.RequestedResolution,
			LastRequestedAt:     formatNanoTimestamp(row.LastRequestedAt),
			LastRequestError:    row.LastRequestError,
		}
		conflict.ActionHint = statusConflictActionHint(canonicalID, &conflict)
		nextActions.add(conflict.ActionHint)
		output = append(output, conflict)
	}

	return output
}

func buildConflictHistoryJSON(
	rows []syncengine.ConflictHistorySnapshot,
	verbose bool,
	examplesLimit int,
) []statusConflictHistoryJSON {
	if len(rows) == 0 {
		return nil
	}

	sampled := sampleConflictHistoryRows(rows, verbose, examplesLimit)
	output := make([]statusConflictHistoryJSON, 0, len(sampled))
	for i := range sampled {
		row := sampled[i]
		output = append(output, statusConflictHistoryJSON{
			ID:           row.ID,
			Path:         row.Path,
			ConflictType: row.ConflictType,
			DetectedAt:   formatNanoTimestamp(row.DetectedAt),
			Resolution:   row.Resolution,
			ResolvedAt:   formatNanoTimestamp(row.ResolvedAt),
			ResolvedBy:   row.ResolvedBy,
		})
	}

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

func sampleDeleteSafetyRows(
	rows []syncengine.DeleteSafetySnapshot,
	verbose bool,
	examplesLimit int,
) []syncengine.DeleteSafetySnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncengine.DeleteSafetySnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncengine.DeleteSafetySnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

func sampleConflictRows(
	rows []syncengine.ConflictStatusSnapshot,
	verbose bool,
	examplesLimit int,
) []syncengine.ConflictStatusSnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncengine.ConflictStatusSnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncengine.ConflictStatusSnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

func sampleConflictHistoryRows(
	rows []syncengine.ConflictHistorySnapshot,
	verbose bool,
	examplesLimit int,
) []syncengine.ConflictHistorySnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncengine.ConflictHistorySnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncengine.ConflictHistorySnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

const (
	stateHeldDeleteHeld     = "held"
	stateHeldDeleteApproved = "approved"
)

func conflictRequestDisplayState(conflict syncengine.ConflictStatusSnapshot) string {
	if conflict.RequestState == "" {
		return syncengine.ConflictStateUnresolved
	}

	return conflict.RequestState
}
