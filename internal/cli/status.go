package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Drive state constants for status and drive list display.
const (
	driveStateReady  = "ready"
	driveStatePaused = "paused"
	syncDirNotSet    = "(not set)"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show all accounts, drives, and token status",
		Long: `Display the status of all configured accounts and drives.

Shows token validity, sync directory, and paused/ready status for each drive.
Reads from config only — does not discover drives from tokens on disk.`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runStatus,
	}
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

// syncStateInfo holds sync state for a single drive, queried from the state DB.
type syncStateInfo struct {
	LastSyncTime     string `json:"last_sync_time,omitempty"`
	LastSyncDuration string `json:"last_sync_duration,omitempty"`
	FileCount        int    `json:"file_count"`
	Issues           int    `json:"issues"` // conflicts + actionable failures
	PendingSync      int    `json:"pending_sync"`
	Retrying         int    `json:"retrying"` // transient failures with failure_count >= 3
	LastError        string `json:"last_error,omitempty"`
}

// statusSummary aggregates health info across all drives.
type statusSummary struct {
	TotalDrives           int `json:"total_drives"`
	Ready                 int `json:"ready"`
	Paused                int `json:"paused"`
	AccountsRequiringAuth int `json:"accounts_requiring_auth"`
	TotalIssues           int `json:"total_issues"`
	TotalPendingSync      int `json:"total_pending_sync"`
	TotalRetrying         int `json:"total_retrying"`
}

// statusOutput wraps the full status response for JSON output.
type statusOutput struct {
	Accounts []statusAccount `json:"accounts"`
	Summary  statusSummary   `json:"summary"`
}

func runStatus(cmd *cobra.Command, _ []string) error {
	return newStatusService(mustCLIContext(cmd.Context())).run()
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

// liveAccountNames reads display name and org name from account profile files on disk.
type liveAccountNames struct {
	logger *slog.Logger
}

func (m *liveAccountNames) ReadAccountNames(account string, driveIDs []driveid.CanonicalID) (string, string) {
	return readAccountMeta(account, driveIDs, m.logger)
}

// liveSyncStateQuerier queries per-drive sync state from real state DBs.
type liveSyncStateQuerier struct {
	logger *slog.Logger
}

func (q *liveSyncStateQuerier) QuerySyncState(cid driveid.CanonicalID) *syncStateInfo {
	statePath := config.DriveStatePath(cid)
	return querySyncState(statePath, q.logger)
}

// buildStatusAccounts groups configured drives by account email and checks
// token validity for each account.
func buildStatusAccounts(cfg *config.Config, logger *slog.Logger) []statusAccount {
	return buildStatusAccountsWith(cfg,
		&liveAccountNames{logger: logger},
		&liveAccountAuthChecker{logger: logger},
		&liveSyncStateQuerier{logger: logger},
	)
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

// driveState returns the human-readable state for a drive based on its
// paused flag only. Auth health is now shown at the account level so status
// does not conflate operational drive state with saved-login state.
func driveState(d *config.Drive) string {
	if d.IsPaused(time.Now()) {
		return driveStatePaused
	}

	return driveStateReady
}

// querySyncState opens a state DB read-only and queries sync metadata, baseline
// entry count, and unresolved conflict count. Returns nil if the DB doesn't exist
// (drive never synced) or if an error occurs opening it.
func querySyncState(statePath string, logger *slog.Logger) *syncStateInfo {
	if !managedPathExists(statePath) {
		return nil
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)", statePath)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		logger.Debug("could not open state DB for status", slog.String("error", err.Error()), slog.String("path", statePath))
		return nil
	}
	defer db.Close()

	ctx := context.Background()
	info := &syncStateInfo{}

	loadSyncMetadata(ctx, db, info, logger)
	info.FileCount = countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM baseline",
		"could not count baseline entries",
	)
	info.Issues = queryIssueCount(ctx, db, logger)
	info.PendingSync = countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced','deleted','filtered')",
		"could not count pending sync items",
	)
	info.Retrying = countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
		"could not count retrying failures",
	)

	return info
}

func loadSyncMetadata(ctx context.Context, db *sql.DB, info *syncStateInfo, logger *slog.Logger) {
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM sync_metadata")
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if scanErr := rows.Scan(&k, &v); scanErr != nil {
			continue
		}
		switch k {
		case "last_sync_time":
			info.LastSyncTime = v
		case "last_sync_duration_ms":
			info.LastSyncDuration = v
		case "last_sync_error":
			info.LastError = v
		}
	}

	if rowErr := rows.Err(); rowErr != nil {
		logger.Debug("error reading sync metadata", slog.String("error", rowErr.Error()))
	}
}

func countStatusMetric(
	ctx context.Context,
	db *sql.DB,
	logger *slog.Logger,
	query string,
	logMessage string,
) int {
	var count int
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		logger.Debug(logMessage, slog.String("error", err.Error()))
	}

	return count
}

func queryIssueCount(ctx context.Context, db *sql.DB, logger *slog.Logger) int {
	conflicts := countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'",
		"could not count conflicts",
	)
	actionable := countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM sync_failures WHERE category = 'actionable'",
		"could not count actionable failures",
	)
	remoteBlocked := countStatusMetric(ctx, db, logger,
		`SELECT COUNT(DISTINCT scope_key) FROM sync_failures
		WHERE failure_role = 'held' AND scope_key LIKE 'perm:remote:%'`,
		"could not count remote blocked scopes",
	)
	authScopes := countStatusMetric(ctx, db, logger,
		"SELECT COUNT(*) FROM scope_blocks WHERE scope_key = 'auth:account'",
		"could not count auth scope blocks",
	)

	return conflicts + actionable + remoteBlocked + authScopes
}

// computeSummary aggregates health information across all status accounts.
func computeSummary(accounts []statusAccount) statusSummary {
	var s statusSummary

	for i := range accounts {
		acct := &accounts[i]
		if acct.AuthState == authStateAuthenticationNeeded {
			s.AccountsRequiringAuth++
		}

		for j := range acct.Drives {
			d := &acct.Drives[j]
			s.TotalDrives++

			switch d.State {
			case driveStateReady:
				s.Ready++
			case driveStatePaused:
				s.Paused++
			}

			if d.SyncState != nil {
				s.TotalIssues += d.SyncState.Issues
				s.TotalPendingSync += d.SyncState.PendingSync
				s.TotalRetrying += d.SyncState.Retrying
			}
		}
	}

	return s
}

func printStatusJSON(w io.Writer, accounts []statusAccount) error {
	output := statusOutput{
		Accounts: accounts,
		Summary:  computeSummary(accounts),
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(output); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printStatusText(w io.Writer, accounts []statusAccount) error {
	for i := range accounts {
		if err := printAccountStatus(w, &accounts[i], i > 0); err != nil {
			return err
		}
	}

	// Print health summary.
	summary := computeSummary(accounts)
	if err := writeln(w); err != nil {
		return err
	}

	return printSummaryText(w, summary)
}

func printAccountStatus(w io.Writer, acct *statusAccount, leadingBlank bool) error {
	if acct == nil {
		return nil
	}

	if leadingBlank {
		if err := writeln(w); err != nil {
			return err
		}
	}

	if err := writef(w, "Account: %s [%s]\n", statusAccountLabel(acct), acct.DriveType); err != nil {
		return err
	}

	if acct.OrgName != "" {
		if err := writef(w, "  Org:   %s\n", acct.OrgName); err != nil {
			return err
		}
	}

	if err := writef(w, "  Auth:  %s\n", acct.AuthState); err != nil {
		return err
	}
	if acct.AuthReason != "" {
		if err := writef(w, "  Reason: %s\n", authReasonText(acct.AuthReason)); err != nil {
			return err
		}
	}
	if acct.AuthAction != "" {
		if err := writef(w, "  Action: %s\n", acct.AuthAction); err != nil {
			return err
		}
	}

	for _, drive := range acct.Drives {
		if err := printDriveStatus(w, drive); err != nil {
			return err
		}
	}

	return nil
}

func statusAccountLabel(acct *statusAccount) string {
	if acct == nil {
		return ""
	}

	if acct.DisplayName == "" {
		return acct.Email
	}

	return fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
}

func printDriveStatus(w io.Writer, drive statusDrive) error {
	syncDir := drive.SyncDir
	if syncDir == "" {
		syncDir = syncDirNotSet
	}

	if err := writef(w, "  %s\n", statusDriveLabel(drive)); err != nil {
		return err
	}
	if err := writef(w, "    Sync dir:  %s\n", syncDir); err != nil {
		return err
	}
	if err := writef(w, "    State:     %s\n", drive.State); err != nil {
		return err
	}
	if drive.SyncState == nil {
		return nil
	}

	return printSyncStateText(w, drive.SyncState)
}

func statusDriveLabel(drive statusDrive) string {
	if drive.DisplayName == "" || drive.DisplayName == drive.CanonicalID {
		return drive.CanonicalID
	}

	return fmt.Sprintf("%s (%s)", drive.DisplayName, drive.CanonicalID)
}

func printSyncStateText(w io.Writer, ss *syncStateInfo) error {
	if ss.LastSyncTime != "" {
		if err := writef(w, "    Last sync: %s (%d files)\n",
			ss.LastSyncTime, ss.FileCount); err != nil {
			return err
		}
	} else {
		if err := writef(w, "    Last sync: never\n"); err != nil {
			return err
		}
	}

	if ss.PendingSync > 0 {
		if err := writef(w, "    Pending:   %d items\n", ss.PendingSync); err != nil {
			return err
		}
	}

	if ss.Issues > 0 {
		if err := writef(w, "    Issues:    %d (run 'onedrive-go issues')\n", ss.Issues); err != nil {
			return err
		}
	}

	if ss.Retrying > 0 {
		if err := writef(w, "    Retrying:  %d items\n", ss.Retrying); err != nil {
			return err
		}
	}

	if ss.LastError != "" {
		if err := writef(w, "    Last error: %s\n", ss.LastError); err != nil {
			return err
		}
	}

	return nil
}

func printSummaryText(w io.Writer, s statusSummary) error {
	var parts []string

	if s.Ready > 0 {
		parts = append(parts, fmt.Sprintf("%d ready", s.Ready))
	}

	if s.Paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", s.Paused))
	}

	if s.AccountsRequiringAuth > 0 {
		parts = append(parts, fmt.Sprintf("%d accounts requiring auth", s.AccountsRequiringAuth))
	}

	stateStr := strings.Join(parts, ", ")

	extra := fmt.Sprintf("%d issues", s.TotalIssues)

	if s.TotalPendingSync > 0 {
		extra += fmt.Sprintf(", %d pending", s.TotalPendingSync)
	}

	if s.TotalRetrying > 0 {
		extra += fmt.Sprintf(", %d retrying", s.TotalRetrying)
	}

	return writef(w, "Summary: %d drives (%s), %s\n",
		s.TotalDrives, stateStr, extra)
}
