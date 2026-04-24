package cli

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
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

const (
	statusProjectionStandalone = "standalone"
	statusProjectionChild      = "child"
)

// statusAccount groups runtime mounts under a single account email.
type statusAccount struct {
	Email          string            `json:"email"`
	DriveType      string            `json:"drive_type"`
	UserID         string            `json:"user_id,omitempty"`
	AuthState      string            `json:"auth_state"`
	AuthReason     string            `json:"auth_reason,omitempty"`
	AuthAction     string            `json:"auth_action,omitempty"`
	DisplayName    string            `json:"display_name,omitempty"`
	OrgName        string            `json:"org_name,omitempty"`
	DegradedReason string            `json:"degraded_reason,omitempty"`
	DegradedAction string            `json:"degraded_action,omitempty"`
	LiveDrives     []statusLiveDrive `json:"live_drives,omitempty"`
	Mounts         []statusMount     `json:"mounts"`
}

type statusLiveDrive struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DriveType  string `json:"drive_type"`
	QuotaUsed  int64  `json:"quota_used"`
	QuotaTotal int64  `json:"quota_total"`
}

// statusMount holds status information for one runtime mount.
type statusMount struct {
	MountID        string         `json:"mount_id"`
	NamespaceID    string         `json:"namespace_id,omitempty"`
	ProjectionKind string         `json:"projection_kind"`
	CanonicalID    string         `json:"canonical_id,omitempty"`
	DisplayName    string         `json:"display_name,omitempty"`
	SyncDir        string         `json:"sync_dir"`
	State          string         `json:"state"`
	StateReason    string         `json:"state_reason,omitempty"`
	StateDetail    string         `json:"state_detail,omitempty"`
	SyncState      *syncStateInfo `json:"sync_state,omitempty"`
}

// syncStateInfo holds the full per-mount status payload rendered by `status`.
type syncStateInfo struct {
	FileCount             int                   `json:"file_count"`
	ConditionCount        int                   `json:"condition_count"`
	RemoteDrift           int                   `json:"remote_drift"`
	Retrying              int                   `json:"retrying"`
	Conditions            []statusConditionJSON `json:"conditions,omitempty"`
	ExamplesLimit         int                   `json:"examples_limit"`
	Verbose               bool                  `json:"verbose"`
	Perf                  *perf.Snapshot        `json:"perf,omitempty"`
	PerfUnavailableReason string                `json:"perf_unavailable_reason,omitempty"`
}

// statusSummary aggregates health info across all runtime mounts.
type statusSummary struct {
	TotalMounts           int `json:"total_mounts"`
	Ready                 int `json:"ready"`
	Paused                int `json:"paused"`
	AccountsRequiringAuth int `json:"accounts_requiring_auth"`
	TotalConditions       int `json:"total_conditions"`
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
	snapshot, err := loadAccountViewSnapshot(context.Background(), cc)
	if err != nil {
		return err
	}

	if len(snapshot.Accounts) == 0 && len(snapshot.Config.Drives) == 0 {
		return writeln(cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	filteredSnapshot, err := filterStatusSnapshot(snapshot, cc.Flags.Account, cc.Flags.Drive, logger)
	if err != nil {
		return err
	}
	if len(filteredSnapshot.Accounts) == 0 && len(filteredSnapshot.Config.Drives) == 0 {
		if cc.Flags.Account != "" {
			return writeln(cc.Output(), "No matching accounts selected.")
		}
		if len(cc.Flags.Drive) > 0 {
			return writeln(cc.Output(), "No matching drives selected.")
		}
		return writeln(cc.Output(), "No accounts configured. Run 'onedrive-go login' to get started.")
	}

	accounts := statusAccounts(cc, filteredSnapshot, history)
	liveOverlayLoader := cc.statusLiveOverlayLoader
	if liveOverlayLoader == nil {
		liveOverlayLoader = loadStatusLiveOverlay
	}
	applyStatusLiveOverlay(accounts, liveOverlayLoader(context.Background(), cc, filteredSnapshot.Accounts))
	applyStatusPerfOverlay(accounts, loadStatusPerfOverlay(context.Background(), perfEnabled))
	if cc.Flags.JSON {
		return printStatusJSON(cc.Output(), accounts)
	}

	return printStatusText(cc.Output(), accounts, history)
}

func filterStatusSnapshot(
	snapshot accountViewSnapshot,
	account string,
	selectors []string,
	logger *slog.Logger,
) (accountViewSnapshot, error) {
	if account == "" && len(selectors) == 0 {
		return snapshot, nil
	}

	filtered := *snapshot.Config
	filtered.Drives = make(map[driveid.CanonicalID]config.Drive)

	selectedAccounts, err := filterStatusDrives(
		&filtered,
		snapshot.Config,
		account,
		selectors,
		logger,
	)
	if err != nil {
		return accountViewSnapshot{}, err
	}

	return accountViewSnapshot{
		Config:         &filtered,
		Stored:         snapshot.Stored,
		MountInventory: snapshot.MountInventory,
		Accounts:       filterSnapshotAccounts(snapshot.Accounts, selectedAccounts),
	}, nil
}

func filterStatusDrives(
	filtered *config.Config,
	full *config.Config,
	account string,
	selectors []string,
	logger *slog.Logger,
) (map[string]struct{}, error) {
	selectedAccounts := make(map[string]struct{})
	if account != "" {
		selectedAccounts[account] = struct{}{}
	}

	if len(selectors) > 0 {
		selectedDrives, err := config.ResolveDrives(full, selectors, true, logger)
		if err != nil {
			return nil, fmt.Errorf("resolving status drive selectors: %w", err)
		}

		addResolvedStatusDrives(filtered, full, selectedAccounts, selectedDrives, account)
		return selectedAccounts, nil
	}

	addAccountStatusDrives(filtered, selectedAccounts, full, account)
	return selectedAccounts, nil
}

func addResolvedStatusDrives(
	filtered *config.Config,
	full *config.Config,
	selectedAccounts map[string]struct{},
	selectedDrives []*config.ResolvedDrive,
	account string,
) {
	for i := range selectedDrives {
		rd := selectedDrives[i]
		if account != "" && rd.CanonicalID.Email() != account {
			continue
		}
		filtered.Drives[rd.CanonicalID] = full.Drives[rd.CanonicalID]
		if account == "" {
			selectedAccounts[rd.CanonicalID.Email()] = struct{}{}
		}
	}
}

func addAccountStatusDrives(
	filtered *config.Config,
	selectedAccounts map[string]struct{},
	full *config.Config,
	account string,
) {
	for cid, drive := range full.Drives {
		if account != "" && cid.Email() != account {
			continue
		}
		filtered.Drives[cid] = drive
		selectedAccounts[cid.Email()] = struct{}{}
	}
}

func filterSnapshotAccounts(accounts []accountView, selectedAccounts map[string]struct{}) []accountView {
	if len(selectedAccounts) == 0 {
		return accounts
	}

	filteredAccounts := make([]accountView, 0, len(accounts))
	for i := range accounts {
		if _, keep := selectedAccounts[accounts[i].Email]; keep {
			filteredAccounts = append(filteredAccounts, accounts[i])
		}
	}

	return filteredAccounts
}

// accountNameReader abstracts reading display name and org name from account
// profile files. Enables testing without real files on disk.
type accountNameReader interface {
	ReadAccountNames(account string, driveIDs []driveid.CanonicalID) (displayName, orgName string)
}

// syncStateQuerier abstracts querying per-mount sync state from state DBs.
// Enables testing without real SQLite databases on disk.
type syncStateQuerier interface {
	QuerySyncState(statePath string) *syncStateInfo
}

// liveSyncStateQuerier queries per-mount sync state from real state DBs.
type liveSyncStateQuerier struct {
	logger        *slog.Logger
	history       bool
	verbose       bool
	examplesLimit int
}

func (q *liveSyncStateQuerier) QuerySyncState(statePath string) *syncStateInfo {
	return querySyncStateWithOptions(statePath, q.logger, q.history, q.verbose, q.examplesLimit)
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

		acct := buildSingleAccountStatusWith(cfg, email, driveIDs, nil, names, checker, syncQ)
		accounts = append(accounts, acct)
	}

	return accounts
}

func buildStatusAccountsFromViews(
	cfg *config.Config,
	inventory *config.MountInventory,
	views []accountView,
	syncQ syncStateQuerier,
) []statusAccount {
	childrenByParent := groupChildMountsByParent(inventory)
	accounts := make([]statusAccount, 0, len(views))

	for i := range views {
		view := views[i]
		driveIDs := append([]driveid.CanonicalID(nil), view.ConfiguredDriveIDs...)
		sort.Slice(driveIDs, func(i, j int) bool { return driveIDs[i].String() < driveIDs[j].String() })
		acct := statusAccount{
			Email:       view.Email,
			DriveType:   view.DriveType,
			UserID:      view.UserID,
			AuthState:   view.AuthHealth.State,
			AuthReason:  string(view.AuthHealth.Reason),
			AuthAction:  view.AuthHealth.Action,
			DisplayName: view.DisplayName,
			OrgName:     view.OrgName,
			Mounts:      make([]statusMount, 0, len(driveIDs)),
		}

		for _, cid := range driveIDs {
			d := cfg.Drives[cid]
			acct.Mounts = append(acct.Mounts, buildConfiguredStatusMount(cid, d, syncQ))
			children := childrenByParent[cid]
			for childIndex := range children {
				acct.Mounts = append(acct.Mounts, buildChildStatusMount(d, &children[childIndex], syncQ))
			}
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
	childrenByParent map[driveid.CanonicalID][]config.MountRecord,
	names accountNameReader,
	checker accountAuthChecker,
	syncQ syncStateQuerier,
) statusAccount {
	acct := statusAccount{
		Email:  email,
		Mounts: make([]statusMount, 0, len(driveIDs)),
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
	acct.AuthReason = string(authHealth.Reason)
	acct.AuthAction = authHealth.Action

	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		acct.Mounts = append(acct.Mounts, buildConfiguredStatusMount(cid, d, syncQ))
		children := childrenByParent[cid]
		for childIndex := range children {
			acct.Mounts = append(acct.Mounts, buildChildStatusMount(d, &children[childIndex], syncQ))
		}
	}

	return acct
}

// readAccountMeta reads display name and org name from catalog account fields.
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

func groupChildMountsByParent(inventory *config.MountInventory) map[driveid.CanonicalID][]config.MountRecord {
	grouped := make(map[driveid.CanonicalID][]config.MountRecord)
	if inventory == nil {
		return grouped
	}

	for mountID := range inventory.Mounts {
		record := inventory.Mounts[mountID]
		parentID, err := driveid.NewCanonicalID(record.NamespaceID)
		if err != nil {
			continue
		}
		grouped[parentID] = append(grouped[parentID], record)
	}

	for parentID := range grouped {
		sort.Slice(grouped[parentID], func(i, j int) bool {
			if grouped[parentID][i].RelativeLocalPath == grouped[parentID][j].RelativeLocalPath {
				return grouped[parentID][i].MountID < grouped[parentID][j].MountID
			}

			return grouped[parentID][i].RelativeLocalPath < grouped[parentID][j].RelativeLocalPath
		})
	}

	return grouped
}

func buildConfiguredStatusMount(
	cid driveid.CanonicalID,
	drive config.Drive,
	syncQ syncStateQuerier,
) statusMount {
	driveDisplayName := drive.DisplayName
	if driveDisplayName == "" {
		driveDisplayName = config.DefaultDisplayName(cid)
	}

	mount := statusMount{
		MountID:        cid.String(),
		ProjectionKind: statusProjectionStandalone,
		CanonicalID:    cid.String(),
		DisplayName:    driveDisplayName,
		SyncDir:        drive.SyncDir,
		State:          driveState(&drive),
	}
	if syncQ != nil {
		mount.SyncState = syncQ.QuerySyncState(config.DriveStatePath(cid))
	}

	return mount
}

func buildChildStatusMount(
	parentDrive config.Drive,
	record *config.MountRecord,
	syncQ syncStateQuerier,
) statusMount {
	displayName := record.LocalAlias
	if displayName == "" {
		displayName = path.Base(record.RelativeLocalPath)
	}

	state := driveState(&parentDrive)
	switch record.State {
	case config.MountStateConflict, config.MountStateUnavailable, config.MountStatePendingRemoval:
		state = string(record.State)
	case config.MountStateActive, "":
		if record.Paused {
			state = driveStatePaused
		} else if !parentDrive.IsPaused(time.Now()) {
			state = driveStateReady
		}
	default:
		state = string(record.State)
	}
	if record.Paused && state == driveStateReady {
		state = driveStatePaused
	}

	mount := statusMount{
		MountID:        record.MountID,
		NamespaceID:    record.NamespaceID,
		ProjectionKind: statusProjectionChild,
		DisplayName:    displayName,
		SyncDir:        filepath.Join(parentDrive.SyncDir, filepath.FromSlash(record.RelativeLocalPath)),
		State:          state,
		StateReason:    record.StateReason,
		StateDetail:    childMountStateDetail(record.State, record.StateReason),
	}
	if syncQ != nil {
		mount.SyncState = syncQ.QuerySyncState(config.MountStatePath(record.MountID))
	}

	return mount
}

func childMountStateDetail(state config.MountState, reason string) string {
	switch reason {
	case config.MountStateReasonShortcutBindingUnavailable:
		return "OneDrive did not return a usable shortcut target. " +
			"Wait for Microsoft Graph to recover, or remove and recreate the shortcut if it no longer resolves."
	case config.MountStateReasonDuplicateContentRoot:
		return "Another child shortcut owns this same content root. Remove or pause one duplicate shortcut."
	case config.MountStateReasonExplicitStandaloneContentRoot:
		return "This content root is already configured as a standalone mount. Remove the child shortcut or remove or pause the standalone mount."
	case config.MountStateReasonShortcutRemoved:
		return "The shortcut was removed. The child mount will be finalized after its runner stops and state is purged."
	case config.MountStateReasonLocalProjectionConflict:
		return "The old and new shortcut paths both exist locally. Move or merge one path before resuming this child mount."
	case config.MountStateReasonLocalProjectionUnavailable:
		return "The local shortcut path could not be moved. Fix the filesystem error, then rerun sync."
	}

	switch state {
	case config.MountStateActive:
		return ""
	case config.MountStateConflict:
		return "Resolve the child mount conflict, then rerun sync."
	case config.MountStateUnavailable:
		return "Wait for the shortcut or local projection to become available, then rerun sync."
	case config.MountStatePendingRemoval:
		return "Wait for runner stop and child state cleanup to finish."
	default:
		return ""
	}
}

func querySyncState(
	statePath string,
	logger *slog.Logger,
) *syncStateInfo {
	return querySyncStateWithOptions(statePath, logger, false, false, defaultVisiblePaths)
}

func querySyncStateWithOptions(
	statePath string,
	logger *slog.Logger,
	history bool,
	verbose bool,
	examplesLimit int,
) *syncStateInfo {
	_ = history
	snapshot := readDriveStatusSnapshot(statePath, logger)
	info := buildSyncStateInfo(&snapshot, verbose, examplesLimit)
	return &info
}
