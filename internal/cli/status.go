package cli

import (
	"context"
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
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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

Without --drive, status shows the account and drive summary view. With exactly
one selected drive, status becomes the detailed per-drive inspection view for
ordinary failures, delete safety, conflicts, and state-store health.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runStatus,
	}

	cmd.Flags().Bool("history", false, "include resolved conflict history in single-drive detailed status")

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

// syncStateInfo holds sync state for a single drive, queried from the state DB.
type syncStateInfo struct {
	LastSyncTime               string             `json:"last_sync_time,omitempty"`
	LastSyncDuration           string             `json:"last_sync_duration,omitempty"`
	FileCount                  int                `json:"file_count"`
	Issues                     int                `json:"issues"` // conflicts + actionable failures
	IssueGroups                []statusIssueGroup `json:"issue_groups,omitempty"`
	PendingSync                int                `json:"pending_sync"`
	Retrying                   int                `json:"retrying"` // transient failures with failure_count >= 3
	LastError                  string             `json:"last_error,omitempty"`
	PendingHeldDeleteApprovals int                `json:"pending_held_delete_approvals,omitempty"`
	PendingConflictRequests    int                `json:"pending_conflict_requests,omitempty"`
	ApplyingConflictRequests   int                `json:"applying_conflict_requests,omitempty"`
	ActionHints                []string           `json:"action_hints,omitempty"`
}

type statusIssueGroup struct {
	SummaryKey string `json:"summary_key"`
	Title      string `json:"title"`
	Count      int    `json:"count"`
	ScopeKind  string `json:"scope_kind"`
	Scope      string `json:"scope,omitempty"`
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
	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return fmt.Errorf("read --history flag: %w", err)
	}

	return newStatusService(mustCLIContext(cmd.Context())).run(history)
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
	logger *slog.Logger
}

func (q *liveSyncStateQuerier) QuerySyncState(cid driveid.CanonicalID) *syncStateInfo {
	statePath := config.DriveStatePath(cid)
	return querySyncState(statePath, q.logger)
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

// querySyncState opens a state DB read-only and queries sync metadata, baseline
// entry count, and unresolved conflict count. Returns nil if the DB doesn't exist
// (drive never synced) or if an error occurs opening it.
func querySyncState(statePath string, logger *slog.Logger) *syncStateInfo {
	if !managedPathExists(statePath) {
		return nil
	}

	ctx := context.Background()
	snapshot, err := syncstore.ReadStatusSnapshot(ctx, statePath, logger)
	if err != nil {
		logger.Debug("could not open state DB for status", slog.String("error", err.Error()), slog.String("path", statePath))
		return nil
	}
	info := &syncStateInfo{
		FileCount:                  snapshot.BaselineEntryCount,
		Issues:                     snapshot.Issues.VisibleTotal(),
		PendingSync:                snapshot.PendingSyncItems,
		Retrying:                   snapshot.Issues.RetryingCount(),
		PendingHeldDeleteApprovals: snapshot.DurableIntents.PendingHeldDeleteApprovals,
		PendingConflictRequests:    snapshot.DurableIntents.PendingConflictRequests,
		ApplyingConflictRequests:   snapshot.DurableIntents.ApplyingConflictRequests,
	}
	info.LastSyncTime = snapshot.SyncMetadata["last_sync_time"]
	info.LastSyncDuration = snapshot.SyncMetadata["last_sync_duration_ms"]
	info.LastError = snapshot.SyncMetadata["last_sync_error"]
	info.IssueGroups = statusIssueGroups(snapshot.Issues.Groups)
	info.ActionHints = statusActionHints(snapshot.DurableIntents)

	return info
}

func statusActionHints(counts syncstore.DurableIntentCounts) []string {
	var hints []string

	if counts.PendingHeldDeleteApprovals > 0 {
		hints = append(hints, "Run `onedrive-go sync` or start `onedrive-go sync --watch` to execute approved deletes.")
	}
	if counts.PendingConflictRequests > 0 {
		hints = append(hints, "Run `onedrive-go sync` or start `onedrive-go sync --watch` to execute queued conflict resolutions.")
	}

	return hints
}

func statusIssueGroups(groups []syncstore.IssueGroupCount) []statusIssueGroup {
	if len(groups) == 0 {
		return nil
	}

	out := make([]statusIssueGroup, 0, len(groups))
	for _, group := range groups {
		descriptor := synctypes.DescribeSummary(group.Key)
		out = append(out, statusIssueGroup{
			SummaryKey: string(group.Key),
			Title:      descriptor.Title,
			Count:      group.Count,
			ScopeKind:  group.ScopeKind,
			Scope:      group.Scope,
		})
	}

	return out
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
	if err := printStatusLastSyncLine(w, ss); err != nil {
		return err
	}

	if err := writeOptionalStatusCountLine(w, ss.PendingSync, "    Pending:   %d items\n"); err != nil {
		return err
	}

	if ss.Issues > 0 {
		if err := writef(w, "    Issues:    %d\n", ss.Issues); err != nil {
			return err
		}
		if err := printStatusIssueGroups(w, ss.IssueGroups); err != nil {
			return err
		}
	}

	if err := writeOptionalStatusCountLine(w, ss.Retrying, "    Retrying:  %d items\n"); err != nil {
		return err
	}

	if err := printStatusDurableIntentLines(w, ss); err != nil {
		return err
	}

	if ss.LastError != "" {
		if err := writef(w, "    Last error: %s\n", ss.LastError); err != nil {
			return err
		}
	}

	return printStatusActionHints(w, ss.ActionHints)
}

func printStatusLastSyncLine(w io.Writer, ss *syncStateInfo) error {
	if ss.LastSyncTime == "" {
		return writef(w, "    Last sync: never\n")
	}

	return writef(w, "    Last sync: %s (%d files)\n", ss.LastSyncTime, ss.FileCount)
}

func writeOptionalStatusCountLine(w io.Writer, count int, format string) error {
	if count <= 0 {
		return nil
	}

	return writef(w, format, count)
}

func printStatusDurableIntentLines(w io.Writer, ss *syncStateInfo) error {
	if err := writeOptionalStatusCountLine(
		w,
		ss.PendingHeldDeleteApprovals,
		"    Approved deletes waiting: %d\n",
	); err != nil {
		return err
	}

	if err := writeOptionalStatusCountLine(
		w,
		ss.PendingConflictRequests,
		"    Queued conflict resolutions: %d\n",
	); err != nil {
		return err
	}

	if err := writeOptionalStatusCountLine(
		w,
		ss.ApplyingConflictRequests,
		"    Applying conflicts: %d\n",
	); err != nil {
		return err
	}

	return nil
}

func printStatusActionHints(w io.Writer, hints []string) error {
	for _, hint := range hints {
		if err := writef(w, "    Next: %s\n", hint); err != nil {
			return err
		}
	}

	return nil
}

func printStatusIssueGroups(w io.Writer, groups []statusIssueGroup) error {
	if len(groups) == 0 {
		return nil
	}

	if err := writeln(w, "    Issue groups:"); err != nil {
		return err
	}

	for _, group := range groups {
		scopeText := statusIssueScopeText(group)
		if scopeText == "" {
			if err := writef(w, "      - %s: %d\n", group.Title, group.Count); err != nil {
				return err
			}
			continue
		}

		if err := writef(w, "      - %s (%s): %d\n", group.Title, scopeText, group.Count); err != nil {
			return err
		}
	}

	return nil
}

func statusIssueScopeText(group statusIssueGroup) string {
	if group.ScopeKind == "" {
		return ""
	}
	if group.Scope == "" {
		return group.ScopeKind + " scope"
	}

	return group.ScopeKind + " " + group.Scope
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
