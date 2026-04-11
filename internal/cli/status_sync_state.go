package cli

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	statusNextIndent        = "      "
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
) (syncstore.DriveStatusSnapshot, driveStateStoreInfo) {
	if !managedPathExists(statePath) {
		return syncstore.DriveStatusSnapshot{}, driveStateStoreInfo{
			Status: stateStoreStatusMissing,
		}
	}

	snapshot, err := syncstore.ReadDriveStatusSnapshot(context.Background(), statePath, history, logger)
	if err != nil {
		return syncstore.DriveStatusSnapshot{}, driveStateStoreInfo{
			Status:       stateStoreStatusDamaged,
			Error:        err.Error(),
			RecoveryHint: recoverAwareStateStoreHint(canonicalID),
		}
	}

	return snapshot, driveStateStoreInfo{Status: stateStoreStatusHealthy}
}

func buildSyncStateInfo(
	canonicalID string,
	snapshot *syncstore.DriveStatusSnapshot,
	storeInfo driveStateStoreInfo,
	verbose bool,
	examplesLimit int,
) syncStateInfo {
	if snapshot == nil {
		snapshot = &syncstore.DriveStatusSnapshot{}
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
		PendingSync:            snapshot.PendingSyncItems,
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
	groups []syncstore.IssueGroupSnapshot,
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
		descriptor := synctypes.DescribeSummary(group.SummaryKey)
		nextActions.add(strings.TrimSpace(descriptor.Action))
		output = append(output, failureGroupJSON{
			SummaryKey: string(group.SummaryKey),
			IssueType:  group.PrimaryIssueType,
			Title:      descriptor.Title,
			Reason:     descriptor.Reason,
			Action:     descriptor.Action,
			ScopeKind:  statusIssueScopeKind(group.ScopeKey),
			Scope:      group.ScopeLabel,
			Count:      group.Count,
			Paths:      sampleStrings(group.Paths, verbose, examplesLimit),
		})
	}

	return output
}

func statusIssueScopeKind(scopeKey synctypes.ScopeKey) string {
	if scopeKey == (synctypes.ScopeKey{}) {
		return ""
	}
	switch scopeKey.Kind {
	case synctypes.ScopeAuthAccount:
		return statusScopeAccount
	case synctypes.ScopeThrottleAccount:
		return statusScopeAccount
	case synctypes.ScopeQuotaOwn:
		return statusScopeDrive
	case synctypes.ScopeQuotaShortcut:
		return statusScopeShortcut
	case synctypes.ScopePermDir:
		return statusScopeDirectory
	case synctypes.ScopePermRemote:
		return statusScopeShortcut
	case synctypes.ScopeThrottleTarget:
		if scopeKey.IsThrottleShared() {
			return statusScopeShortcut
		}
		return statusScopeDrive
	case synctypes.ScopeService:
		return statusScopeService
	case synctypes.ScopeDiskLocal:
		return statusScopeDisk
	}

	return "file"
}

func buildDeleteSafetyJSON(
	canonicalID string,
	rows []syncstore.DeleteSafetySnapshot,
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
	rows []syncstore.ConflictStatusSnapshot,
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
		case synctypes.ConflictStateQueued:
			info.QueuedConflictRequests++
		case synctypes.ConflictStateApplying:
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
	rows []syncstore.ConflictHistorySnapshot,
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
	rows []syncstore.DeleteSafetySnapshot,
	verbose bool,
	examplesLimit int,
) []syncstore.DeleteSafetySnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncstore.DeleteSafetySnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncstore.DeleteSafetySnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

func sampleConflictRows(
	rows []syncstore.ConflictStatusSnapshot,
	verbose bool,
	examplesLimit int,
) []syncstore.ConflictStatusSnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncstore.ConflictStatusSnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncstore.ConflictStatusSnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

func sampleConflictHistoryRows(
	rows []syncstore.ConflictHistorySnapshot,
	verbose bool,
	examplesLimit int,
) []syncstore.ConflictHistorySnapshot {
	if verbose || len(rows) <= examplesLimit {
		out := make([]syncstore.ConflictHistorySnapshot, len(rows))
		copy(out, rows)
		return out
	}

	out := make([]syncstore.ConflictHistorySnapshot, examplesLimit)
	copy(out, rows[:examplesLimit])
	return out
}

func printDriveSyncSections(w io.Writer, ss *syncStateInfo, history bool) error {
	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    ISSUES"); err != nil {
		return err
	}
	if err := printIssueGroupSection(w, ss.IssueGroups); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    DELETE SAFETY"); err != nil {
		return err
	}
	if err := printDeleteSafetySection(w, ss); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "    CONFLICTS"); err != nil {
		return err
	}
	if err := printConflictSection(w, ss.Conflicts, ss.ConflictsTotal); err != nil {
		return err
	}

	if history {
		if err := writeln(w); err != nil {
			return err
		}
		if err := writeln(w, "    CONFLICT HISTORY"); err != nil {
			return err
		}
		if err := printConflictHistorySection(w, ss.ConflictHistory, ss.ConflictHistoryTotal); err != nil {
			return err
		}
	}

	return nil
}

func printIssueGroupSection(w io.Writer, groups []failureGroupJSON) error {
	if len(groups) == 0 {
		return writeln(w, "    No ordinary issues.")
	}

	for i := range groups {
		group := groups[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s (%d %s)\n", group.Title, group.Count, itemNoun(group.Count)); err != nil {
			return err
		}
		if err := writef(w, "      %s %s\n", group.Reason, group.Action); err != nil {
			return err
		}
		if group.Scope != "" {
			if err := writef(w, "      Scope: %s\n", group.Scope); err != nil {
				return err
			}
		}
		if err := printIssueGroupPaths(w, group.Paths, group.Count); err != nil {
			return err
		}
		if err := printStatusNextLine(w, group.Action); err != nil {
			return err
		}
	}

	return nil
}

func printIssueGroupPaths(w io.Writer, paths []string, totalCount int) error {
	if len(paths) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}
	for i := range paths {
		if err := writef(w, "      %s\n", paths[i]); err != nil {
			return err
		}
	}

	remaining := totalCount - len(paths)
	if remaining > 0 {
		if err := writef(w, "      ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printDeleteSafetySection(w io.Writer, ss *syncStateInfo) error {
	if ss == nil || ss.DeleteSafetyTotal == 0 {
		return writeln(w, "    No delete-safety entries.")
	}

	held := filterDeleteSafety(ss.DeleteSafety, stateHeldDeleteHeld)
	heldTotal := ss.HeldDeletesWaiting
	if heldTotal > 0 {
		if err := writef(w, "    Held deletes requiring approval: %d\n", heldTotal); err != nil {
			return err
		}
		if err := printDeleteSafetyRows(w, held, heldTotal); err != nil {
			return err
		}
		if len(held) > 0 {
			if err := printStatusNextLine(w, held[0].ActionHint); err != nil {
				return err
			}
		}
	}

	approved := filterDeleteSafety(ss.DeleteSafety, stateHeldDeleteApproved)
	approvedTotal := ss.ApprovedDeletesWaiting
	if approvedTotal > 0 {
		if heldTotal > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    Approved deletes waiting for sync: %d\n", approvedTotal); err != nil {
			return err
		}
		if err := printDeleteSafetyRows(w, approved, approvedTotal); err != nil {
			return err
		}
		if len(approved) > 0 {
			if err := printStatusNextLine(w, approved[0].ActionHint); err != nil {
				return err
			}
		}
	}

	return nil
}

func printDeleteSafetyRows(w io.Writer, rows []deleteSafetyJSON, totalCount int) error {
	for i := range rows {
		if err := writef(w, "      %s\n", rows[i].Path); err != nil {
			return err
		}
	}

	remaining := totalCount - len(rows)
	if remaining > 0 {
		if err := writef(w, "      ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printConflictSection(w io.Writer, conflicts []statusConflictJSON, totalCount int) error {
	if totalCount == 0 {
		return writeln(w, "    No unresolved conflicts.")
	}

	for i := range conflicts {
		conflict := conflicts[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s [%s]\n", conflict.Path, conflict.ConflictType); err != nil {
			return err
		}
		switch conflict.State {
		case synctypes.ConflictStateUnresolved:
			if err := writeln(w, "      Decision: needed"); err != nil {
				return err
			}
		default:
			if err := writef(w, "      Decision: %s (%s)\n", conflict.RequestedResolution, conflict.State); err != nil {
				return err
			}
		}
		if conflict.LastRequestError != "" {
			if err := writef(w, "      Last attempt: %s\n", conflict.LastRequestError); err != nil {
				return err
			}
		}
		if err := printStatusNextLine(w, conflict.ActionHint); err != nil {
			return err
		}
	}

	remaining := totalCount - len(conflicts)
	if remaining > 0 {
		if err := writef(w, "    ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printConflictHistorySection(
	w io.Writer,
	history []statusConflictHistoryJSON,
	totalCount int,
) error {
	if totalCount == 0 {
		return writeln(w, "    No resolved conflicts.")
	}

	for i := range history {
		entry := history[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "    %s [%s]\n", entry.Path, entry.ConflictType); err != nil {
			return err
		}
		if err := writef(w, "      Resolved: %s", entry.Resolution); err != nil {
			return err
		}
		if entry.ResolvedBy != "" {
			if err := writef(w, " by %s", entry.ResolvedBy); err != nil {
				return err
			}
		}
		if entry.ResolvedAt != "" {
			if err := writef(w, " at %s", entry.ResolvedAt); err != nil {
				return err
			}
		}
		if err := writeln(w); err != nil {
			return err
		}
	}

	remaining := totalCount - len(history)
	if remaining > 0 {
		if err := writef(w, "    ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printStatusNextLine(w io.Writer, hint string) error {
	if strings.TrimSpace(hint) == "" {
		return nil
	}

	return writef(w, "%sNext: %s\n", statusNextIndent, hint)
}

const (
	stateHeldDeleteHeld     = "held"
	stateHeldDeleteApproved = "approved"
)

func filterDeleteSafety(rows []deleteSafetyJSON, state string) []deleteSafetyJSON {
	filtered := make([]deleteSafetyJSON, 0, len(rows))
	for i := range rows {
		if rows[i].State == state {
			filtered = append(filtered, rows[i])
		}
	}

	return filtered
}

func conflictRequestDisplayState(conflict syncstore.ConflictStatusSnapshot) string {
	if conflict.RequestState == "" {
		return synctypes.ConflictStateUnresolved
	}

	return conflict.RequestState
}
