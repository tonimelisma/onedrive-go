package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Drive state constants for status and drive list display.
const (
	driveStateReady       = "up_to_date"
	driveStateSyncing     = "syncing"
	driveStatePending     = "pending"
	driveStatePaused      = "paused"
	driveStateIssues      = "issues"
	driveStateUnavailable = "unavailable"
	syncDirNotSet         = "(not set)"
)

const (
	statusDriveKindPersonalOneDrive = "personal_onedrive"
	statusDriveKindBusinessOneDrive = "business_onedrive"
	statusDriveKindSharePoint       = "sharepoint_library"
	statusDriveKindSharedFolder     = "shared_folder"
)

const (
	statusProjectionStandalone = "standalone"
	statusProjectionChild      = "child"
)

type statusMount = statusDrive

// statusAccount groups configured drives under a single account email.
type statusAccount struct {
	Email          string                `json:"email"`
	DisplayName    string                `json:"display_name,omitempty"`
	OrgName        string                `json:"org_name,omitempty"`
	SignInRequired *statusSignInRequired `json:"sign_in_required,omitempty"`
	Drives         []statusDrive         `json:"drives"`

	DriveType      string        `json:"-"`
	UserID         string        `json:"-"`
	AuthState      string        `json:"-"`
	AuthReason     string        `json:"-"`
	AuthAction     string        `json:"-"`
	DegradedReason string        `json:"-"`
	DegradedAction string        `json:"-"`
	Mounts         []statusDrive `json:"-"`
}

type statusSignInRequired struct {
	Reason string `json:"reason"`
	Action string `json:"action"`
}

type statusStorage struct {
	UsedBytes  int64  `json:"used_bytes"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	Used       string `json:"used"`
	Total      string `json:"total,omitempty"`
}

// statusDrive holds status information for one configured drive or shared
// folder shortcut. InternalID and NamespaceID are private runtime lookup keys.
type statusDrive struct {
	InternalID             string         `json:"-"`
	NamespaceID            string         `json:"-"`
	Kind                   string         `json:"kind"`
	Name                   string         `json:"name"`
	Folder                 string         `json:"folder"`
	State                  string         `json:"state"`
	Storage                *statusStorage `json:"storage,omitempty"`
	StateReason            string         `json:"state_reason,omitempty"`
	IssueClass             string         `json:"issue_class,omitempty"`
	StateDetail            string         `json:"state_detail,omitempty"`
	ProtectedCurrentPath   string         `json:"protected_current_path,omitempty"`
	ProtectedReservedPaths []string       `json:"protected_reserved_paths,omitempty"`
	RecoveryClass          string         `json:"recovery_class,omitempty"`
	RecoveryAction         string         `json:"recovery_action,omitempty"`
	AutoRetry              *bool          `json:"auto_retry,omitempty"`
	WaitingReplacement     string         `json:"waiting_replacement,omitempty"`
	RuntimeOwner           string         `json:"-"`
	RuntimeState           string         `json:"-"`
	SyncState              *syncStateInfo `json:"sync_state,omitempty"`
	SharedFolders          []statusDrive  `json:"shared_folders,omitempty"`

	MountID        string        `json:"-"`
	ProjectionKind string        `json:"-"`
	CanonicalID    string        `json:"-"`
	DisplayName    string        `json:"-"`
	SyncDir        string        `json:"-"`
	ChildMounts    []statusDrive `json:"-"`
}

// syncStateInfo holds the per-drive sync status payload rendered by `status`.
type syncStateInfo struct {
	FileCount             int                   `json:"file_count"`
	ConditionCount        int                   `json:"issue_count,omitempty"`
	RemoteDrift           int                   `json:"remote_changes,omitempty"`
	Retrying              int                   `json:"retrying"`
	Conditions            []statusConditionJSON `json:"issues,omitempty"`
	ExamplesLimit         int                   `json:"examples_limit,omitempty"`
	Verbose               bool                  `json:"verbose,omitempty"`
	Perf                  *perf.Snapshot        `json:"perf,omitempty"`
	PerfUnavailableReason string                `json:"perf_unavailable_reason,omitempty"`
}

// statusSummary aggregates health info across displayed drives and shared folders.
type statusSummary struct {
	TotalMounts           int    `json:"-"`
	TotalDrives           int    `json:"total_drives"`
	TotalSharedFolders    int    `json:"total_shared_folders,omitempty"`
	Ready                 int    `json:"up_to_date,omitempty"`
	Syncing               int    `json:"syncing,omitempty"`
	Pending               int    `json:"pending,omitempty"`
	Paused                int    `json:"paused,omitempty"`
	Issues                int    `json:"with_issues,omitempty"`
	Unavailable           int    `json:"unavailable,omitempty"`
	AccountsRequiringAuth int    `json:"accounts_requiring_sign_in,omitempty"`
	TotalConditions       int    `json:"total_issues,omitempty"`
	TotalRemoteDrift      int    `json:"total_remote_changes,omitempty"`
	TotalRetrying         int    `json:"total_retrying,omitempty"`
	RuntimeOwner          string `json:"runtime_owner,omitempty"`
	RuntimeActiveDrives   int    `json:"runtime_active_drives,omitempty"`
	RuntimeActiveMounts   int    `json:"-"`
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
	applyStatusLiveOverlay(accounts, liveOverlayLoader(context.Background(), cc, filteredSnapshot))
	applyStatusRuntimeOverlay(accounts, loadStatusRuntimeOverlay(context.Background()))
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
		Config:        &filtered,
		Stored:        snapshot.Stored,
		ShortcutRoots: filterShortcutRootSnapshots(snapshot.ShortcutRoots, filtered.Drives),
		Accounts:      filterSnapshotAccounts(snapshot.Accounts, selectedAccounts),
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
	for cid := range full.Drives {
		if account != "" && cid.Email() != account {
			continue
		}
		filtered.Drives[cid] = full.Drives[cid]
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

func filterShortcutRootSnapshots(
	roots map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView,
	drives map[driveid.CanonicalID]config.Drive,
) map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView {
	filtered := make(map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView)
	for cid := range drives {
		if records, ok := roots[cid]; ok {
			filtered[cid] = append([]syncengine.ShortcutRootStatusView(nil), records...)
		}
	}
	return filtered
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

type childMountStatusInput struct {
	ParentID driveid.CanonicalID
	Root     syncengine.ShortcutRootStatusView
}

func buildStatusAccountsFromViews(
	cfg *config.Config,
	shortcutRoots map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView,
	views []accountView,
	syncQ syncStateQuerier,
) []statusAccount {
	childrenByParent := groupChildMountsByParent(shortcutRoots)
	accounts := make([]statusAccount, 0, len(views))

	for i := range views {
		view := views[i]
		driveIDs := append([]driveid.CanonicalID(nil), view.ConfiguredDriveIDs...)
		sort.Slice(driveIDs, func(i, j int) bool { return driveIDs[i].String() < driveIDs[j].String() })
		acct := statusAccount{
			Email:       view.Email,
			DriveType:   view.DriveType,
			AuthState:   view.AuthHealth.State,
			AuthReason:  string(view.AuthHealth.Reason),
			AuthAction:  view.AuthHealth.Action,
			DisplayName: view.DisplayName,
			OrgName:     view.OrgName,
			Drives:      make([]statusDrive, 0, len(driveIDs)),
		}
		setAccountSignInRequired(&acct)

		for _, cid := range driveIDs {
			d := cfg.Drives[cid]
			drive := buildConfiguredStatusDrive(cid, &d, &acct, syncQ)
			children := childrenByParent[cid]
			for childIndex := range children {
				childDrive := buildChildStatusDrive(
					&d,
					&children[childIndex],
					syncQ,
				)
				drive.SharedFolders = append(drive.SharedFolders, childDrive)
				drive.ChildMounts = append(drive.ChildMounts, childDrive)
			}
			acct.Drives = append(acct.Drives, drive)
			acct.Mounts = append(acct.Mounts, drive)
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
	childrenByParent map[driveid.CanonicalID][]childMountStatusInput,
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
	acct.AuthReason = string(authHealth.Reason)
	acct.AuthAction = authHealth.Action
	setAccountSignInRequired(&acct)

	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		drive := buildConfiguredStatusDrive(cid, &d, &acct, syncQ)
		children := childrenByParent[cid]
		for childIndex := range children {
			childDrive := buildChildStatusDrive(
				&d,
				&children[childIndex],
				syncQ,
			)
			drive.SharedFolders = append(drive.SharedFolders, childDrive)
			drive.ChildMounts = append(drive.ChildMounts, childDrive)
		}
		acct.Drives = append(acct.Drives, drive)
		acct.Mounts = append(acct.Mounts, drive)
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

func setAccountSignInRequired(acct *statusAccount) {
	if acct == nil || acct.AuthState != authStateAuthenticationNeeded {
		return
	}

	acct.SignInRequired = &statusSignInRequired{
		Reason: authReasonText(authstate.Reason(acct.AuthReason)),
		Action: acct.AuthAction,
	}
}

func statusDriveKind(cid driveid.CanonicalID) string {
	switch {
	case cid.IsPersonal():
		return statusDriveKindPersonalOneDrive
	case cid.IsBusiness():
		return statusDriveKindBusinessOneDrive
	case cid.IsSharePoint():
		return statusDriveKindSharePoint
	case cid.IsShared():
		return statusDriveKindSharedFolder
	default:
		return "drive"
	}
}

func statusConfiguredDriveName(cid driveid.CanonicalID, drive *config.Drive, acct *statusAccount) string {
	switch {
	case cid.IsPersonal():
		return possessive(accountDisplayNameForDrive(acct, cid.Email())) + " personal OneDrive"
	case cid.IsBusiness():
		if acct != nil && acct.OrgName != "" {
			return "OneDrive - " + acct.OrgName
		}
		return accountDisplayNameForDrive(acct, cid.Email()) + "'s work OneDrive"
	case cid.IsSharePoint():
		if drive != nil && drive.DisplayName != "" {
			return drive.DisplayName
		}
		return config.DefaultDisplayName(cid)
	case cid.IsShared():
		if drive != nil && drive.Owner != "" {
			return drive.Owner
		}
		if drive != nil && drive.DisplayName != "" {
			return drive.DisplayName
		}
		return "Shared folder"
	default:
		if drive != nil && drive.DisplayName != "" {
			return drive.DisplayName
		}
		return config.DefaultDisplayName(cid)
	}
}

func accountDisplayNameForDrive(acct *statusAccount, fallback string) string {
	if acct != nil && acct.DisplayName != "" {
		return acct.DisplayName
	}
	if fallback != "" {
		return fallback
	}
	return "This account"
}

func possessive(name string) string {
	if name == "" {
		return "This account's"
	}
	if strings.HasSuffix(name, "s") || strings.HasSuffix(name, "S") {
		return name + "'"
	}
	return name + "'s"
}

func finalizeStatusDriveState(drive *statusDrive) {
	if drive == nil || drive.State == driveStatePaused || drive.State == driveStateUnavailable {
		return
	}
	if drive.SyncState == nil {
		return
	}
	if drive.SyncState.ConditionCount > 0 {
		drive.State = driveStateIssues
		return
	}
	if drive.RuntimeState == statusRuntimeStateActive && (drive.SyncState.RemoteDrift > 0 || drive.SyncState.Retrying > 0) {
		drive.State = driveStateSyncing
		return
	}
	if drive.SyncState.RemoteDrift > 0 || drive.SyncState.Retrying > 0 {
		drive.State = driveStatePending
	}
}

func groupChildMountsByParent(
	shortcutRoots map[driveid.CanonicalID][]syncengine.ShortcutRootStatusView,
) map[driveid.CanonicalID][]childMountStatusInput {
	grouped := make(map[driveid.CanonicalID][]childMountStatusInput)
	if shortcutRoots == nil {
		return grouped
	}

	for parentID, roots := range shortcutRoots {
		for i := range roots {
			grouped[parentID] = append(grouped[parentID], childMountStatusInput{
				ParentID: parentID,
				Root:     roots[i],
			})
		}
	}

	for parentID := range grouped {
		sort.Slice(grouped[parentID], func(i, j int) bool {
			if grouped[parentID][i].Root.SortPath == grouped[parentID][j].Root.SortPath {
				return grouped[parentID][i].Root.MountID < grouped[parentID][j].Root.MountID
			}

			return grouped[parentID][i].Root.SortPath < grouped[parentID][j].Root.SortPath
		})
	}

	return grouped
}

func buildConfiguredStatusDrive(
	cid driveid.CanonicalID,
	drive *config.Drive,
	acct *statusAccount,
	syncQ syncStateQuerier,
) statusDrive {
	if drive == nil {
		return statusDrive{}
	}

	status := statusDrive{
		InternalID:     cid.String(),
		Kind:           statusDriveKind(cid),
		Name:           statusConfiguredDriveName(cid, drive, acct),
		Folder:         drive.SyncDir,
		State:          driveState(drive),
		MountID:        cid.String(),
		ProjectionKind: statusProjectionStandalone,
		CanonicalID:    cid.String(),
		DisplayName:    statusConfiguredDriveName(cid, drive, acct),
		SyncDir:        drive.SyncDir,
	}
	if syncQ != nil {
		status.SyncState = syncQ.QuerySyncState(config.DriveStatePath(cid))
	}
	finalizeStatusDriveState(&status)

	return status
}

func buildChildStatusDrive(
	parentDrive *config.Drive,
	child *childMountStatusInput,
	syncQ syncStateQuerier,
) statusDrive {
	if child == nil {
		return statusDrive{}
	}
	root := child.Root
	metadata := root.Metadata
	state := driveState(parentDrive)
	statusDetail := root.StateDetail
	if metadata.DisplayState != "" {
		state = metadata.DisplayState
	}

	status := statusDrive{
		InternalID:     root.MountID,
		NamespaceID:    child.ParentID.String(),
		Kind:           statusDriveKindSharedFolder,
		Name:           root.DisplayName,
		Folder:         root.DisplayLocalRoot,
		State:          state,
		StateReason:    metadata.StateReason,
		IssueClass:     string(metadata.IssueClass),
		StateDetail:    statusDetail,
		RecoveryClass:  string(metadata.RecoveryClass),
		RecoveryAction: metadata.RecoveryAction,
		MountID:        root.MountID,
		ProjectionKind: statusProjectionChild,
		DisplayName:    root.DisplayName,
		SyncDir:        root.DisplayLocalRoot,
	}
	status.WaitingReplacement = root.WaitingReplacementPath
	if metadata.ProtectsPath {
		status.ProtectedCurrentPath = root.ProtectedCurrentLocalRoot
		status.ProtectedReservedPaths = root.ProtectedReservedLocalRoots
	}
	if metadata.DisplayState != "" {
		autoRetry := metadata.AutoRetry
		status.AutoRetry = &autoRetry
	}
	if syncQ != nil {
		status.SyncState = syncQ.QuerySyncState(config.MountStatePath(root.MountID))
	}
	finalizeStatusDriveState(&status)

	return status
}

func buildChildStatusMount(
	parentDrive *config.Drive,
	child *childMountStatusInput,
	syncQ syncStateQuerier,
) statusMount {
	return buildChildStatusDrive(parentDrive, child, syncQ)
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
