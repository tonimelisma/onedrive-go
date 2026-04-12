package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// Drive state constants for status and drive list display.
const (
	driveStateReady  = "ready"
	driveStatePaused = "paused"
	syncDirNotSet    = "(not set)"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show sync status, drive health, and pending user decisions",
		Long: `Display the status of all configured accounts and drives.

Status always shows the same per-drive sync-health contract for every displayed
drive. Use --drive to filter which drives are shown, --history to include
resolved conflict history, and --verbose to expand sampled path and row lists.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runStatus,
	}

	cmd.Flags().Bool("history", false, "include resolved conflict history for the displayed drives")
	cmd.Flags().Bool("perf", false, "include live performance snapshots from the active sync owner")

	return cmd
}

// statusAccount groups drives under a single account email.
type statusAccount struct {
	Email       string        `json:"email"`
	DriveType   string        `json:"drive_type"`
	AuthState   string        `json:"auth_state"`
	AuthReason  string        `json:"auth_reason,omitempty"`
	AuthAction  string        `json:"auth_action,omitempty"`
	DisplayName string        `json:"display_name,omitempty"`
	OrgName     string        `json:"org_name,omitempty"`
	Drives      []statusDrive `json:"drives"`
}

// statusDrive holds status information for a single drive.
type statusDrive struct {
	CanonicalID string         `json:"canonical_id"`
	DisplayName string         `json:"display_name,omitempty"`
	SyncDir     string         `json:"sync_dir"`
	State       string         `json:"state"`
	SyncState   *syncStateInfo `json:"sync_state,omitempty"`
}

// syncStateInfo holds the full per-drive status payload rendered by `status`.
type syncStateInfo struct {
	LastSyncTime     string `json:"last_sync_time,omitempty"`
	LastSyncDuration string `json:"last_sync_duration,omitempty"`
	FileCount        int    `json:"file_count"`
	IssueCount       int    `json:"issue_count"`
	// RemoteDrift is the count of remote-side differences we have already
	// observed and persisted, but which are not yet reflected in baseline.
	RemoteDrift              int                         `json:"remote_drift"`
	Retrying                 int                         `json:"retrying"`
	LastError                string                      `json:"last_error,omitempty"`
	IssueGroups              []failureGroupJSON          `json:"issue_groups,omitempty"`
	DeleteSafety             []deleteSafetyJSON          `json:"delete_safety,omitempty"`
	DeleteSafetyTotal        int                         `json:"delete_safety_total"`
	Conflicts                []statusConflictJSON        `json:"conflicts,omitempty"`
	ConflictsTotal           int                         `json:"conflicts_total"`
	ConflictHistory          []statusConflictHistoryJSON `json:"conflict_history,omitempty"`
	ConflictHistoryTotal     int                         `json:"conflict_history_total"`
	NextActions              []string                    `json:"next_actions,omitempty"`
	ExamplesLimit            int                         `json:"examples_limit"`
	Verbose                  bool                        `json:"verbose"`
	StateStoreStatus         string                      `json:"state_store_status"`
	StateStoreError          string                      `json:"state_store_error,omitempty"`
	StateStoreRecoveryHint   string                      `json:"state_store_recovery_hint,omitempty"`
	HeldDeletesWaiting       int                         `json:"held_deletes_waiting,omitempty"`
	ApprovedDeletesWaiting   int                         `json:"approved_deletes_waiting,omitempty"`
	QueuedConflictRequests   int                         `json:"queued_conflict_requests,omitempty"`
	ApplyingConflictRequests int                         `json:"applying_conflict_requests,omitempty"`
	Perf                     *perf.Snapshot              `json:"perf,omitempty"`
	PerfUnavailableReason    string                      `json:"perf_unavailable_reason,omitempty"`
}

// statusSummary aggregates health info across all drives.
type statusSummary struct {
	TotalDrives           int `json:"total_drives"`
	Ready                 int `json:"ready"`
	Paused                int `json:"paused"`
	AccountsRequiringAuth int `json:"accounts_requiring_auth"`
	TotalIssues           int `json:"total_issues"`
	TotalRemoteDrift      int `json:"total_remote_drift"`
	TotalRetrying         int `json:"total_retrying"`
}

// statusOutput wraps the full status response for JSON output.
type statusOutput struct {
	Accounts []statusAccount `json:"accounts"`
	Summary  statusSummary   `json:"summary"`
}

func runStatus(cmd *cobra.Command, _ []string) error {
	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return fmt.Errorf("read --history flag: %w", err)
	}
	showPerf, err := cmd.Flags().GetBool("perf")
	if err != nil {
		return fmt.Errorf("read --perf flag: %w", err)
	}

	return runStatusCommand(mustCLIContext(cmd.Context()), history, showPerf)
}

// accountNameReader abstracts reading display name and org name from account
// profile files. Enables testing without real files on disk.
type accountNameReader interface {
	ReadAccountNames(account string, driveIDs []driveid.CanonicalID) (displayName, orgName string)
}

// syncStateQuerier abstracts querying per-drive sync state from state DBs.
// Enables testing without real SQLite databases on disk.
type syncStateQuerier interface {
	QuerySyncState(cid driveid.CanonicalID) *syncStateInfo
}

// liveSyncStateQuerier queries per-drive sync state from real state DBs.
type liveSyncStateQuerier struct {
	logger        *slog.Logger
	history       bool
	verbose       bool
	examplesLimit int
}

func (q *liveSyncStateQuerier) QuerySyncState(cid driveid.CanonicalID) *syncStateInfo {
	statePath := config.DriveStatePath(cid)
	return querySyncStateWithOptions(cid.String(), statePath, q.logger, q.history, q.verbose, q.examplesLimit)
}

// buildStatusAccountsWith is the testable core of buildStatusAccounts.
// Accepts interfaces for metadata reading, token checking, and sync state querying.
func buildStatusAccountsWith(
	cfg *config.Config, names accountNameReader, checker accountAuthChecker, syncQ syncStateQuerier,
) []statusAccount {
	grouped, order := groupDrivesByAccount(cfg)
	accounts := make([]statusAccount, 0, len(order))

	for _, email := range order {
		driveIDs := grouped[email]
		sort.Slice(driveIDs, func(i, j int) bool {
			return driveIDs[i].String() < driveIDs[j].String()
		})

		acct := buildSingleAccountStatusWith(cfg, email, driveIDs, names, checker, syncQ)
		accounts = append(accounts, acct)
	}

	return accounts
}

func buildStatusAccountsFromCatalog(
	cfg *config.Config,
	catalog []accountCatalogEntry,
	syncQ syncStateQuerier,
) []statusAccount {
	grouped, order := groupDrivesByAccount(cfg)
	accounts := make([]statusAccount, 0, len(order))

	for _, email := range order {
		driveIDs := grouped[email]
		sort.Slice(driveIDs, func(i, j int) bool {
			return driveIDs[i].String() < driveIDs[j].String()
		})

		entry := accountCatalogEntry{}
		if foundEntry, found := catalogEntryByEmail(catalog, email); found {
			entry = foundEntry
		}
		acct := statusAccount{
			Email:       email,
			DriveType:   entry.DriveType,
			AuthState:   entry.AuthHealth.State,
			AuthReason:  entry.AuthHealth.Reason,
			AuthAction:  entry.AuthHealth.Action,
			DisplayName: entry.DisplayName,
			OrgName:     entry.OrgName,
			Drives:      make([]statusDrive, 0, len(driveIDs)),
		}

		for _, cid := range driveIDs {
			d := cfg.Drives[cid]
			state := driveState(&d)

			driveDisplayName := d.DisplayName
			if driveDisplayName == "" {
				driveDisplayName = config.DefaultDisplayName(cid)
			}

			sd := statusDrive{
				CanonicalID: cid.String(),
				DisplayName: driveDisplayName,
				SyncDir:     d.SyncDir,
				State:       state,
			}

			if syncQ != nil {
				sd.SyncState = syncQ.QuerySyncState(cid)
			}

			acct.Drives = append(acct.Drives, sd)
		}

		accounts = append(accounts, acct)
	}

	return accounts
}

// groupDrivesByAccount collects drive IDs keyed by account email and returns
// a stable ordering of unique emails.
func groupDrivesByAccount(cfg *config.Config) (map[string][]driveid.CanonicalID, []string) {
	grouped := make(map[string][]driveid.CanonicalID)
	var order []string

	for id := range cfg.Drives {
		email := id.Email()
		if _, seen := grouped[email]; !seen {
			order = append(order, email)
		}

		grouped[email] = append(grouped[email], id)
	}

	sort.Strings(order)

	return grouped, order
}

// buildSingleAccountStatusWith builds the status for one account and its drives,
// using injected interfaces for metadata, token checking, and sync state querying.
func buildSingleAccountStatusWith(
	cfg *config.Config, email string, driveIDs []driveid.CanonicalID,
	names accountNameReader, checker accountAuthChecker, syncQ syncStateQuerier,
) statusAccount {
	acct := statusAccount{
		Email:  email,
		Drives: make([]statusDrive, 0, len(driveIDs)),
	}

	// Derive drive type from the first non-sharepoint drive.
	for _, cid := range driveIDs {
		dt := cid.DriveType()
		if dt != "sharepoint" {
			acct.DriveType = dt

			break
		}
	}

	if acct.DriveType == "" && len(driveIDs) > 0 {
		acct.DriveType = driveIDs[0].DriveType()
	}

	// Read display name and org name from account profile.
	acct.DisplayName, acct.OrgName = names.ReadAccountNames(email, driveIDs)

	// Project offline auth health for this account from local token state and
	// persisted auth:account scope blocks.
	authHealth := checker.CheckAccountAuth(context.Background(), email, driveIDs)
	acct.AuthState = authHealth.State
	acct.AuthReason = authHealth.Reason
	acct.AuthAction = authHealth.Action

	// Build drive status entries.
	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		state := driveState(&d)

		// Use explicit display_name from config, falling back to auto-derived.
		driveDisplayName := d.DisplayName
		if driveDisplayName == "" {
			driveDisplayName = config.DefaultDisplayName(cid)
		}

		sd := statusDrive{
			CanonicalID: cid.String(),
			DisplayName: driveDisplayName,
			SyncDir:     d.SyncDir,
			State:       state,
		}

		// Query sync state from state DB (nil if never synced).
		if syncQ != nil {
			sd.SyncState = syncQ.QuerySyncState(cid)
		}

		acct.Drives = append(acct.Drives, sd)
	}

	return acct
}

// readAccountMeta reads display name and org name from the account profile.
func readAccountMeta(account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) (displayName, orgName string) {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		tokenID = findTokenFallback(account, logger)
	}

	orgName, displayName = config.ResolveAccountNames(tokenID, logger)
	return displayName, orgName
}

func readAccountDisplayName(account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) string {
	displayName, _ := readAccountMeta(account, driveIDs, logger)
	return displayName
}

// driveState returns the human-readable state for a drive based on its
// paused flag only. Auth health is now shown at the account level so status
// does not conflate operational drive state with saved-login state.
func driveState(d *config.Drive) string {
	if d.IsPaused(time.Now()) {
		return driveStatePaused
	}

	return driveStateReady
}

func querySyncState(
	canonicalID string,
	statePath string,
	logger *slog.Logger,
) *syncStateInfo {
	return querySyncStateWithOptions(canonicalID, statePath, logger, false, false, defaultVisiblePaths)
}

func querySyncStateWithOptions(
	canonicalID string,
	statePath string,
	logger *slog.Logger,
	history bool,
	verbose bool,
	examplesLimit int,
) *syncStateInfo {
	snapshot, storeInfo := readDriveStatusSnapshot(statePath, logger, history, canonicalID)
	info := buildSyncStateInfo(canonicalID, &snapshot, storeInfo, verbose, examplesLimit)
	return &info
}
