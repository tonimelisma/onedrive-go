package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Token state constants for status reporting.
const (
	tokenStateMissing = "missing"
	tokenStateExpired = "expired"
	tokenStateValid   = "valid"
)

// Drive state constants for status and drive list display.
const (
	driveStateReady   = "ready"
	driveStatePaused  = "paused"
	driveStateNoToken = "no token"
	syncDirNotSet     = "(not set)"
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
	TokenState  string        `json:"token_state"`
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
	TotalDrives      int `json:"total_drives"`
	Ready            int `json:"ready"`
	Paused           int `json:"paused"`
	NoToken          int `json:"no_token"`
	TotalIssues      int `json:"total_issues"`
	TotalPendingSync int `json:"total_pending_sync"`
	TotalRetrying    int `json:"total_retrying"`
}

// statusOutput wraps the full status response for JSON output.
type statusOutput struct {
	Accounts []statusAccount `json:"accounts"`
	Summary  statusSummary   `json:"summary"`
}

func runStatus(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	cfgPath := cc.CfgPath

	cfg, warnings, err := config.LoadOrDefaultLenient(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	config.LogWarnings(warnings, logger)

	if len(cfg.Drives) == 0 {
		// Config-mandatory: no drives means check for tokens to provide smart guidance.
		tokens := config.DiscoverTokens(logger)
		if len(tokens) > 0 {
			fmt.Println("No drives configured. Run 'onedrive-go drive add' to add a drive.")
		} else {
			fmt.Println("No accounts configured. Run 'onedrive-go login' to get started.")
		}

		return nil
	}

	accounts := buildStatusAccounts(cfg, logger)

	if cc.Flags.JSON {
		return printStatusJSON(os.Stdout, accounts)
	}

	printStatusText(os.Stdout, accounts)

	return nil
}

// accountNameReader abstracts reading display name and org name from account
// profile files. Enables testing without real files on disk.
type accountNameReader interface {
	ReadAccountNames(account string, driveIDs []driveid.CanonicalID) (displayName, orgName string)
}

// tokenStateChecker abstracts token validity checks.
// Enables testing without real OAuth tokens.
type tokenStateChecker interface {
	CheckToken(ctx context.Context, account string, driveIDs []driveid.CanonicalID) string
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

// liveTokenChecker checks token validity via the graph package.
type liveTokenChecker struct {
	logger *slog.Logger
}

func (c *liveTokenChecker) CheckToken(ctx context.Context, account string, driveIDs []driveid.CanonicalID) string {
	return checkTokenState(ctx, account, driveIDs, c.logger)
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
		&liveTokenChecker{logger: logger},
		&liveSyncStateQuerier{logger: logger},
	)
}

// buildStatusAccountsWith is the testable core of buildStatusAccounts.
// Accepts interfaces for metadata reading, token checking, and sync state querying.
func buildStatusAccountsWith(
	cfg *config.Config, names accountNameReader, checker tokenStateChecker, syncQ syncStateQuerier,
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
	names accountNameReader, checker tokenStateChecker, syncQ syncStateQuerier,
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

	// Check token validity for this account.
	acct.TokenState = checker.CheckToken(context.Background(), email, driveIDs)

	// Build drive status entries.
	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		state := driveState(&d, acct.TokenState)

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

	return config.ResolveAccountNames(tokenID, logger)
}

// checkTokenState determines whether a valid token exists for the given account.
// Returns "valid", "expired", or "missing".
func checkTokenState(ctx context.Context, account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) string {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		// No drives in config — probe the filesystem for an existing token.
		tokenID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenID)
	if tokenPath == "" {
		return tokenStateMissing
	}

	// Try loading a token source to check validity. The TokenSourceFromPath call
	// will detect expired tokens internally.
	_, err := graph.TokenSourceFromPath(ctx, tokenPath, logger)
	if err != nil {
		if errors.Is(err, graph.ErrNotLoggedIn) {
			return tokenStateMissing
		}

		return tokenStateExpired
	}

	return tokenStateValid
}

// driveState returns the human-readable state for a drive based on its
// paused flag and token status. Uses IsPaused for expiry-aware pause checks —
// expired timed pauses correctly report as "ready" rather than "paused."
func driveState(d *config.Drive, tokenState string) string {
	if d.IsPaused(time.Now()) {
		return driveStatePaused
	}

	if tokenState == tokenStateMissing {
		return driveStateNoToken
	}

	return driveStateReady
}

// querySyncState opens a state DB read-only and queries sync metadata, baseline
// entry count, and unresolved conflict count. Returns nil if the DB doesn't exist
// (drive never synced) or if an error occurs opening it.
func querySyncState(statePath string, logger *slog.Logger) *syncStateInfo {
	if _, err := os.Stat(statePath); err != nil {
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

	// Read sync metadata. Table may not exist in pre-migration DBs.
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM sync_metadata")
	if err == nil {
		defer rows.Close()

		for rows.Next() {
			var k, v string
			if scanErr := rows.Scan(&k, &v); scanErr == nil {
				switch k {
				case "last_sync_time":
					info.LastSyncTime = v
				case "last_sync_duration_ms":
					info.LastSyncDuration = v
				case "last_sync_error":
					info.LastError = v
				}
			}
		}

		if rowErr := rows.Err(); rowErr != nil {
			logger.Debug("error reading sync metadata", slog.String("error", rowErr.Error()))
		}
	}

	// Count baseline entries.
	if scanErr := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM baseline").Scan(&info.FileCount); scanErr != nil {
		logger.Debug("could not count baseline entries", slog.String("error", scanErr.Error()))
	}

	// Count issues = unresolved conflicts + actionable failures.
	var conflicts, actionable int

	conflictSQL := "SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'"
	if scanErr := db.QueryRowContext(ctx, conflictSQL).Scan(&conflicts); scanErr != nil {
		logger.Debug("could not count conflicts", slog.String("error", scanErr.Error()))
	}

	actionableSQL := "SELECT COUNT(*) FROM sync_failures WHERE category = 'actionable'"
	if scanErr := db.QueryRowContext(ctx, actionableSQL).Scan(&actionable); scanErr != nil {
		logger.Debug("could not count actionable failures", slog.String("error", scanErr.Error()))
	}

	info.Issues = conflicts + actionable

	// Count pending sync items (remote_state not yet synced/deleted/filtered).
	pendingSQL := "SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced','deleted','filtered')"
	if scanErr := db.QueryRowContext(ctx, pendingSQL).Scan(&info.PendingSync); scanErr != nil {
		logger.Debug("could not count pending sync items", slog.String("error", scanErr.Error()))
	}

	// Count retrying = transient failures with failure_count >= 3.
	retryingSQL := "SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3"
	if scanErr := db.QueryRowContext(ctx, retryingSQL).Scan(&info.Retrying); scanErr != nil {
		logger.Debug("could not count retrying failures", slog.String("error", scanErr.Error()))
	}

	return info
}

// computeSummary aggregates health information across all status accounts.
func computeSummary(accounts []statusAccount) statusSummary {
	var s statusSummary

	for _, acct := range accounts {
		for _, d := range acct.Drives {
			s.TotalDrives++

			switch d.State {
			case driveStateReady:
				s.Ready++
			case driveStatePaused:
				s.Paused++
			case driveStateNoToken:
				s.NoToken++
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

func printStatusText(w io.Writer, accounts []statusAccount) {
	for i, acct := range accounts {
		if i > 0 {
			fmt.Fprintln(w)
		}

		label := acct.Email
		if acct.DisplayName != "" {
			label = fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
		}

		fmt.Fprintf(w, "Account: %s [%s]\n", label, acct.DriveType)

		if acct.OrgName != "" {
			fmt.Fprintf(w, "  Org:   %s\n", acct.OrgName)
		}

		fmt.Fprintf(w, "  Token: %s\n", acct.TokenState)

		for _, d := range acct.Drives {
			syncDir := d.SyncDir
			if syncDir == "" {
				syncDir = syncDirNotSet
			}

			driveLabel := d.CanonicalID
			if d.DisplayName != "" && d.DisplayName != d.CanonicalID {
				driveLabel = fmt.Sprintf("%s (%s)", d.DisplayName, d.CanonicalID)
			}

			fmt.Fprintf(w, "  %s\n", driveLabel)
			fmt.Fprintf(w, "    Sync dir:  %s\n", syncDir)
			fmt.Fprintf(w, "    State:     %s\n", d.State)

			if d.SyncState != nil {
				printSyncStateText(w, d.SyncState)
			}
		}
	}

	// Print health summary.
	summary := computeSummary(accounts)
	fmt.Fprintln(w)
	printSummaryText(w, summary)
}

func printSyncStateText(w io.Writer, ss *syncStateInfo) {
	if ss.LastSyncTime != "" {
		fmt.Fprintf(w, "    Last sync: %s (%d files)\n",
			ss.LastSyncTime, ss.FileCount)
	} else {
		fmt.Fprintf(w, "    Last sync: never\n")
	}

	if ss.PendingSync > 0 {
		fmt.Fprintf(w, "    Pending:   %d items\n", ss.PendingSync)
	}

	if ss.Issues > 0 {
		fmt.Fprintf(w, "    Issues:    %d (run 'onedrive-go issues')\n", ss.Issues)
	}

	if ss.Retrying > 0 {
		fmt.Fprintf(w, "    Retrying:  %d items\n", ss.Retrying)
	}

	if ss.LastError != "" {
		fmt.Fprintf(w, "    Last error: %s\n", ss.LastError)
	}
}

func printSummaryText(w io.Writer, s statusSummary) {
	var parts []string

	if s.Ready > 0 {
		parts = append(parts, fmt.Sprintf("%d ready", s.Ready))
	}

	if s.Paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", s.Paused))
	}

	if s.NoToken > 0 {
		parts = append(parts, fmt.Sprintf("%d no token", s.NoToken))
	}

	stateStr := strings.Join(parts, ", ")

	extra := fmt.Sprintf("%d issues", s.TotalIssues)

	if s.TotalPendingSync > 0 {
		extra += fmt.Sprintf(", %d pending", s.TotalPendingSync)
	}

	if s.TotalRetrying > 0 {
		extra += fmt.Sprintf(", %d retrying", s.TotalRetrying)
	}

	fmt.Fprintf(w, "Summary: %d drives (%s), %s\n",
		s.TotalDrives, stateStr, extra)
}
