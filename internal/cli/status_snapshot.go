package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

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
	LastSyncTime             string                      `json:"last_sync_time,omitempty"`
	LastSyncDuration         string                      `json:"last_sync_duration,omitempty"`
	FileCount                int                         `json:"file_count"`
	IssueCount               int                         `json:"issue_count"`
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

func runStatusCommand(cc *CLIContext, history bool, showPerf ...bool) error {
	logger := cc.Logger
	perfEnabled := len(showPerf) > 0 && showPerf[0]
	snapshot, err := loadAccountCatalogSnapshot(context.Background(), cc)
	if err != nil {
		return err
	}

	if len(snapshot.Config.Drives) == 0 {
		tokens := config.DiscoverTokens(logger)
		if len(tokens) > 0 {
			return writeln(cc.Output(), "No drives configured. Run 'onedrive-go drive add' to add a drive.")
		}

		return writeln(cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	filteredSnapshot, err := filterStatusSnapshot(snapshot, cc.Flags.Drive, logger)
	if err != nil {
		return err
	}
	if len(filteredSnapshot.Config.Drives) == 0 {
		return writeln(cc.Output(), "No matching drives selected.")
	}

	accounts := statusAccounts(cc, filteredSnapshot, history)
	applyStatusPerfOverlay(accounts, loadStatusPerfOverlay(context.Background(), perfEnabled))
	if cc.Flags.JSON {
		return printStatusJSON(cc.Output(), accounts)
	}

	return printStatusText(cc.Output(), accounts, history)
}

func filterStatusSnapshot(
	snapshot accountCatalogSnapshot,
	selectors []string,
	logger *slog.Logger,
) (accountCatalogSnapshot, error) {
	if len(selectors) == 0 {
		return snapshot, nil
	}

	selectedDrives, err := config.ResolveDrives(snapshot.Config, selectors, true, logger)
	if err != nil {
		return accountCatalogSnapshot{}, fmt.Errorf("resolving status drive selectors: %w", err)
	}

	filtered := *snapshot.Config
	filtered.Drives = make(map[driveid.CanonicalID]config.Drive, len(selectedDrives))
	for i := range selectedDrives {
		rd := selectedDrives[i]
		filtered.Drives[rd.CanonicalID] = snapshot.Config.Drives[rd.CanonicalID]
	}

	return accountCatalogSnapshot{
		Config:  &filtered,
		Catalog: snapshot.Catalog,
	}, nil
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
func buildStatusAccountsWith(
	cfg *config.Config,
	names accountNameReader,
	checker accountAuthChecker,
	syncQ syncStateQuerier,
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
			driveDisplayName := d.DisplayName
			if driveDisplayName == "" {
				driveDisplayName = config.DefaultDisplayName(cid)
			}

			sd := statusDrive{
				CanonicalID: cid.String(),
				DisplayName: driveDisplayName,
				SyncDir:     d.SyncDir,
				State:       driveState(&d),
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

func buildSingleAccountStatusWith(
	cfg *config.Config,
	email string,
	driveIDs []driveid.CanonicalID,
	names accountNameReader,
	checker accountAuthChecker,
	syncQ syncStateQuerier,
) statusAccount {
	acct := statusAccount{
		Email:  email,
		Drives: make([]statusDrive, 0, len(driveIDs)),
	}

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

	acct.DisplayName, acct.OrgName = names.ReadAccountNames(email, driveIDs)

	authHealth := checker.CheckAccountAuth(context.Background(), email, driveIDs)
	acct.AuthState = authHealth.State
	acct.AuthReason = authHealth.Reason
	acct.AuthAction = authHealth.Action

	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		driveDisplayName := d.DisplayName
		if driveDisplayName == "" {
			driveDisplayName = config.DefaultDisplayName(cid)
		}

		sd := statusDrive{
			CanonicalID: cid.String(),
			DisplayName: driveDisplayName,
			SyncDir:     d.SyncDir,
			State:       driveState(&d),
		}

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
// paused flag only. Auth health is shown at the account level.
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
