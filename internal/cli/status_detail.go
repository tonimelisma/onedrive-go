package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	stateStoreStatusHealthy = "healthy"
	stateStoreStatusMissing = "missing"
	stateStoreStatusDamaged = "damaged"
)

type detailedStatusDrive struct {
	CanonicalID string `json:"canonical_id"`
	DisplayName string `json:"display_name,omitempty"`
	SyncDir     string `json:"sync_dir"`
	State       string `json:"state"`
	AuthState   string `json:"auth_state,omitempty"`
	AuthReason  string `json:"auth_reason,omitempty"`
	AuthAction  string `json:"auth_action,omitempty"`
}

type detailedStatusOutput struct {
	Drive                  detailedStatusDrive         `json:"drive"`
	LastSyncTime           string                      `json:"last_sync_time,omitempty"`
	LastSyncDuration       string                      `json:"last_sync_duration,omitempty"`
	FileCount              int                         `json:"file_count"`
	PendingSync            int                         `json:"pending_sync"`
	IssueGroups            []failureGroupJSON          `json:"issue_groups"`
	DeleteSafety           []deleteSafetyJSON          `json:"delete_safety"`
	Conflicts              []statusConflictJSON        `json:"conflicts"`
	ConflictHistory        []statusConflictHistoryJSON `json:"conflict_history,omitempty"`
	StateStoreStatus       string                      `json:"state_store_status"`
	StateStoreError        string                      `json:"state_store_error,omitempty"`
	StateStoreRecoveryHint string                      `json:"state_store_recovery_hint,omitempty"`
}

type deleteSafetyJSON struct {
	Path       string `json:"path"`
	State      string `json:"state"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
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

type detailedStateStoreInfo struct {
	Status       string
	Error        string
	RecoveryHint string
}

func (s *statusService) runDetailed(
	snapshot accountReadModelSnapshot,
	selector string,
	history bool,
) error {
	canonicalID, drive, err := config.MatchDrive(snapshot.Config, selector, s.cc.Logger)
	if err != nil {
		return fmt.Errorf("match drive %q: %w", selector, err)
	}

	driveDisplayName := drive.DisplayName
	if driveDisplayName == "" {
		driveDisplayName = config.DefaultDisplayName(canonicalID)
	}

	entry, found := catalogEntryByEmail(snapshot.Catalog, canonicalID.Email())
	driveDetail := detailedStatusDrive{
		CanonicalID: canonicalID.String(),
		DisplayName: driveDisplayName,
		SyncDir:     drive.SyncDir,
		State:       driveState(&drive),
	}
	if found {
		driveDetail.AuthState = entry.AuthHealth.State
		driveDetail.AuthReason = entry.AuthHealth.Reason
		driveDetail.AuthAction = entry.AuthHealth.Action
	}

	statePath := config.DriveStatePath(canonicalID)
	detailSnapshot, storeInfo := readDetailedStatusSnapshot(statePath, s.cc.Logger, history, canonicalID.String())
	output := buildDetailedStatusOutput(driveDetail, detailSnapshot, storeInfo)

	if s.cc.Flags.JSON {
		return printDetailedStatusJSON(s.cc.Output(), &output)
	}

	return printDetailedStatusText(s.cc.Output(), &output, history, s.cc.Flags.Verbose)
}

func readDetailedStatusSnapshot(
	statePath string,
	logger *slog.Logger,
	history bool,
	canonicalID string,
) (syncstore.DetailedStatusSnapshot, detailedStateStoreInfo) {
	if !managedPathExists(statePath) {
		return syncstore.DetailedStatusSnapshot{}, detailedStateStoreInfo{
			Status: stateStoreStatusMissing,
		}
	}

	inspector, err := syncstore.OpenInspector(statePath, logger)
	if err != nil {
		return syncstore.DetailedStatusSnapshot{}, detailedStateStoreInfo{
			Status:       stateStoreStatusDamaged,
			Error:        err.Error(),
			RecoveryHint: recoverAwareStateStoreHint(canonicalID),
		}
	}
	defer func() {
		if closeErr := inspector.Close(); closeErr != nil {
			logger.Debug("close detailed status inspector", "error", closeErr.Error(), "path", statePath)
		}
	}()

	snapshot, err := inspector.ReadDetailedStatusSnapshot(context.Background(), history)
	if err != nil {
		return syncstore.DetailedStatusSnapshot{}, detailedStateStoreInfo{
			Status:       stateStoreStatusDamaged,
			Error:        err.Error(),
			RecoveryHint: recoverAwareStateStoreHint(canonicalID),
		}
	}

	return snapshot, detailedStateStoreInfo{Status: stateStoreStatusHealthy}
}

func buildDetailedStatusOutput(
	drive detailedStatusDrive,
	snapshot syncstore.DetailedStatusSnapshot,
	storeInfo detailedStateStoreInfo,
) detailedStatusOutput {
	output := detailedStatusOutput{
		Drive:                  drive,
		FileCount:              snapshot.BaselineEntryCount,
		PendingSync:            snapshot.PendingSyncItems,
		IssueGroups:            make([]failureGroupJSON, 0, len(snapshot.IssueGroups)),
		DeleteSafety:           make([]deleteSafetyJSON, 0, len(snapshot.DeleteSafety)),
		Conflicts:              make([]statusConflictJSON, 0, len(snapshot.Conflicts)),
		ConflictHistory:        make([]statusConflictHistoryJSON, 0, len(snapshot.ConflictHistory)),
		StateStoreStatus:       storeInfo.Status,
		StateStoreError:        storeInfo.Error,
		StateStoreRecoveryHint: storeInfo.RecoveryHint,
	}

	output.LastSyncTime = snapshot.SyncMetadata["last_sync_time"]
	output.LastSyncDuration = snapshot.SyncMetadata["last_sync_duration_ms"]

	for i := range snapshot.IssueGroups {
		group := snapshot.IssueGroups[i]
		descriptor := synctypes.DescribeSummary(group.SummaryKey)
		paths := group.Paths
		if paths == nil {
			paths = []string{}
		}
		output.IssueGroups = append(output.IssueGroups, failureGroupJSON{
			IssueType: group.PrimaryIssueType,
			Title:     descriptor.Title,
			Reason:    descriptor.Reason,
			Action:    descriptor.Action,
			Scope:     group.ScopeLabel,
			Count:     group.Count,
			Paths:     paths,
		})
	}

	for i := range snapshot.DeleteSafety {
		row := snapshot.DeleteSafety[i]
		output.DeleteSafety = append(output.DeleteSafety, deleteSafetyJSON{
			Path:       row.Path,
			State:      row.State,
			LastSeenAt: formatNanoTimestamp(row.LastSeenAt),
			ApprovedAt: formatNanoTimestamp(row.ApprovedAt),
		})
	}

	for i := range snapshot.Conflicts {
		row := snapshot.Conflicts[i]
		output.Conflicts = append(output.Conflicts, statusConflictJSON{
			ID:                  row.ID,
			Path:                row.Path,
			ConflictType:        row.ConflictType,
			DetectedAt:          formatNanoTimestamp(row.DetectedAt),
			State:               conflictRequestDisplayState(row),
			RequestedResolution: row.RequestedResolution,
			LastRequestedAt:     formatNanoTimestamp(row.LastRequestedAt),
			LastRequestError:    row.LastRequestError,
		})
	}

	for i := range snapshot.ConflictHistory {
		row := snapshot.ConflictHistory[i]
		output.ConflictHistory = append(output.ConflictHistory, statusConflictHistoryJSON{
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

func printDetailedStatusJSON(w io.Writer, output *detailedStatusOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printDetailedStatusText(
	w io.Writer,
	output *detailedStatusOutput,
	history bool,
	verbose bool,
) error {
	if err := printDetailedStatusHeader(w, output); err != nil {
		return err
	}
	if output.StateStoreStatus != stateStoreStatusHealthy {
		return nil
	}

	return printDetailedStatusSections(w, output, history, verbose)
}

func printDetailedStatusHeader(w io.Writer, output *detailedStatusOutput) error {
	if err := writef(w, "Drive: %s\n", detailedStatusDriveLabel(output.Drive)); err != nil {
		return err
	}

	return writeDetailedStatusHeaderLines(w, output)
}

func detailedStatusDriveLabel(drive detailedStatusDrive) string {
	return statusDriveLabel(statusDrive{
		CanonicalID: drive.CanonicalID,
		DisplayName: drive.DisplayName,
	})
}

func writeDetailedStatusHeaderLines(w io.Writer, output *detailedStatusOutput) error {
	lines := detailedStatusHeaderLines(output)
	for i := range lines {
		line := lines[i]
		if err := writef(w, "  %-9s %s\n", line.label, line.value); err != nil {
			return err
		}
	}

	return nil
}

type detailedStatusHeaderLine struct {
	label string
	value string
}

func detailedStatusHeaderLines(output *detailedStatusOutput) []detailedStatusHeaderLine {
	lines := []detailedStatusHeaderLine{
		{label: "Sync dir:", value: defaultStatusSyncDir(output.Drive.SyncDir)},
		{label: "State:", value: output.Drive.State},
	}

	appendOptionalDetailedStatusLine(&lines, "Auth:", output.Drive.AuthState)
	appendOptionalDetailedStatusLine(&lines, "Reason:", authReasonText(output.Drive.AuthReason))
	appendOptionalDetailedStatusLine(&lines, "Action:", output.Drive.AuthAction)

	lines = append(lines,
		detailedStatusHeaderLine{label: "Last sync:", value: defaultDetailedLastSync(output.LastSyncTime)},
	)
	appendOptionalDetailedStatusLine(&lines, "Duration:", output.LastSyncDuration+"ms")
	lines = append(lines,
		detailedStatusHeaderLine{label: "Files:", value: fmt.Sprintf("%d", output.FileCount)},
		detailedStatusHeaderLine{label: "Pending:", value: fmt.Sprintf("%d", output.PendingSync)},
		detailedStatusHeaderLine{label: "State DB:", value: output.StateStoreStatus},
	)
	appendOptionalDetailedStatusLine(&lines, "DB error:", output.StateStoreError)
	appendOptionalDetailedStatusLine(&lines, "Recover:", output.StateStoreRecoveryHint)

	return lines
}

func appendOptionalDetailedStatusLine(lines *[]detailedStatusHeaderLine, label string, value string) {
	if value == "" {
		return
	}

	*lines = append(*lines, detailedStatusHeaderLine{label: label, value: value})
}

func defaultStatusSyncDir(syncDir string) string {
	if syncDir == "" {
		return syncDirNotSet
	}

	return syncDir
}

func defaultDetailedLastSync(lastSync string) string {
	if lastSync == "" {
		return "never"
	}

	return lastSync
}

func printDetailedStatusSections(
	w io.Writer,
	output *detailedStatusOutput,
	history bool,
	verbose bool,
) error {
	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "ISSUES"); err != nil {
		return err
	}
	if err := printDetailedIssueSection(w, output.IssueGroups, verbose); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "DELETE SAFETY"); err != nil {
		return err
	}
	if err := printDetailedDeleteSafetySection(w, output.DeleteSafety, verbose); err != nil {
		return err
	}

	if err := writeln(w); err != nil {
		return err
	}
	if err := writeln(w, "CONFLICTS"); err != nil {
		return err
	}
	if err := printDetailedConflictSection(w, output.Conflicts); err != nil {
		return err
	}

	if history {
		if err := writeln(w); err != nil {
			return err
		}
		if err := writeln(w, "CONFLICT HISTORY"); err != nil {
			return err
		}
		if err := printDetailedConflictHistorySection(w, output.ConflictHistory); err != nil {
			return err
		}
	}

	return nil
}

func printDetailedIssueSection(w io.Writer, groups []failureGroupJSON, verbose bool) error {
	if len(groups) == 0 {
		return writeln(w, "No ordinary issues.")
	}

	for i, group := range groups {
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "%s (%d %s)\n", group.Title, group.Count, itemNoun(group.Count)); err != nil {
			return err
		}
		if err := writef(w, "  %s %s\n", group.Reason, group.Action); err != nil {
			return err
		}
		if group.Scope != "" {
			if err := writef(w, "  Scope: %s\n", group.Scope); err != nil {
				return err
			}
		}
		if err := printFailurePathsFromJSON(w, group.Paths, group.Count, verbose); err != nil {
			return err
		}
	}

	return nil
}

func printDetailedDeleteSafetySection(w io.Writer, rows []deleteSafetyJSON, verbose bool) error {
	if len(rows) == 0 {
		return writeln(w, "No delete-safety entries.")
	}

	held := filterDeleteSafety(rows, stateHeldDeleteHeld)
	approved := filterDeleteSafety(rows, stateHeldDeleteApproved)

	if len(held) > 0 {
		if err := writef(w, "Held deletes requiring approval: %d\n", len(held)); err != nil {
			return err
		}
		if err := printDeleteSafetyPaths(w, held, verbose); err != nil {
			return err
		}
	}

	if len(approved) > 0 {
		if len(held) > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "Approved deletes waiting for sync: %d\n", len(approved)); err != nil {
			return err
		}
		if err := printDeleteSafetyPaths(w, approved, verbose); err != nil {
			return err
		}
	}

	return nil
}

const (
	stateHeldDeleteHeld     = "held"
	stateHeldDeleteApproved = "approved"
)

func filterDeleteSafety(rows []deleteSafetyJSON, state string) []deleteSafetyJSON {
	filtered := make([]deleteSafetyJSON, 0, len(rows))
	for _, row := range rows {
		if row.State == state {
			filtered = append(filtered, row)
		}
	}

	return filtered
}

func printDeleteSafetyPaths(w io.Writer, rows []deleteSafetyJSON, verbose bool) error {
	limit := len(rows)
	if !verbose && limit > defaultVisiblePaths {
		limit = defaultVisiblePaths
	}

	for _, row := range rows[:limit] {
		if err := writef(w, "  %s\n", row.Path); err != nil {
			return err
		}
	}

	remaining := len(rows) - limit
	if remaining > 0 {
		if err := writef(w, "  ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func printDetailedConflictSection(w io.Writer, conflicts []statusConflictJSON) error {
	if len(conflicts) == 0 {
		return writeln(w, "No unresolved conflicts.")
	}

	for i := range conflicts {
		conflict := &conflicts[i]
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "%s [%s]\n", conflict.Path, conflict.ConflictType); err != nil {
			return err
		}
		switch conflict.State {
		case synctypes.ConflictStateUnresolved:
			if err := writeln(w, "  Decision: needed"); err != nil {
				return err
			}
		default:
			if err := writef(w, "  Decision: %s (%s)\n", conflict.RequestedResolution, conflict.State); err != nil {
				return err
			}
		}
		if conflict.LastRequestError != "" {
			if err := writef(w, "  Last attempt: %s\n", conflict.LastRequestError); err != nil {
				return err
			}
		}
	}

	return nil
}

func printDetailedConflictHistorySection(w io.Writer, history []statusConflictHistoryJSON) error {
	if len(history) == 0 {
		return writeln(w, "No resolved conflicts.")
	}

	for i, entry := range history {
		if i > 0 {
			if err := writeln(w); err != nil {
				return err
			}
		}
		if err := writef(w, "%s [%s]\n", entry.Path, entry.ConflictType); err != nil {
			return err
		}
		if err := writef(w, "  Resolved: %s", entry.Resolution); err != nil {
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

	return nil
}

func printFailurePathsFromJSON(w io.Writer, paths []string, count int, verbose bool) error {
	if len(paths) == 0 {
		return nil
	}
	if err := writeln(w); err != nil {
		return err
	}

	limit := len(paths)
	if !verbose && limit > defaultVisiblePaths {
		limit = defaultVisiblePaths
	}
	for _, path := range paths[:limit] {
		if err := writef(w, "  %s\n", path); err != nil {
			return err
		}
	}

	remaining := count - limit
	if remaining > 0 {
		if err := writef(w, "  ... and %d more (use --verbose to see all)\n", remaining); err != nil {
			return err
		}
	}

	return nil
}

func conflictRequestDisplayState(conflict syncstore.ConflictStatusSnapshot) string {
	if conflict.RequestState == "" {
		return synctypes.ConflictStateUnresolved
	}

	return conflict.RequestState
}
