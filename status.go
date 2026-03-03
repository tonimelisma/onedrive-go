package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"

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
	driveStateReady      = "ready"
	driveStatePaused     = "paused"
	driveStateNoToken    = "no token"
	driveStateNeedsSetup = "needs setup"
	syncDirNotSet        = "(not set)"
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
	CanonicalID string `json:"canonical_id"`
	DisplayName string `json:"display_name,omitempty"`
	SyncDir     string `json:"sync_dir"`
	State       string `json:"state"`
}

func runStatus(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger
	cfgPath := cc.CfgPath

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

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
		return printStatusJSON(accounts)
	}

	printStatusText(accounts)

	return nil
}

// accountMetaReader abstracts reading account metadata (display name, org name)
// from token files. Enables testing without real token files on disk.
type accountMetaReader interface {
	ReadMeta(account string, driveIDs []driveid.CanonicalID) (displayName, orgName string)
}

// tokenStateChecker abstracts token validity checks.
// Enables testing without real OAuth tokens.
type tokenStateChecker interface {
	CheckToken(ctx context.Context, account string, driveIDs []driveid.CanonicalID) string
}

// liveAccountMeta reads metadata from actual token files on disk.
type liveAccountMeta struct {
	logger *slog.Logger
}

func (m *liveAccountMeta) ReadMeta(account string, driveIDs []driveid.CanonicalID) (string, string) {
	return readAccountMeta(account, driveIDs, m.logger)
}

// liveTokenChecker checks token validity via the graph package.
type liveTokenChecker struct {
	logger *slog.Logger
}

func (c *liveTokenChecker) CheckToken(ctx context.Context, account string, driveIDs []driveid.CanonicalID) string {
	return checkTokenState(ctx, account, driveIDs, c.logger)
}

// buildStatusAccounts groups configured drives by account email and checks
// token validity for each account.
func buildStatusAccounts(cfg *config.Config, logger *slog.Logger) []statusAccount {
	return buildStatusAccountsWith(cfg, &liveAccountMeta{logger: logger}, &liveTokenChecker{logger: logger})
}

// buildStatusAccountsWith is the testable core of buildStatusAccounts.
// Accepts interfaces for metadata reading and token checking.
func buildStatusAccountsWith(
	cfg *config.Config, meta accountMetaReader, checker tokenStateChecker,
) []statusAccount {
	grouped, order := groupDrivesByAccount(cfg)
	accounts := make([]statusAccount, 0, len(order))

	for _, email := range order {
		driveIDs := grouped[email]
		sort.Slice(driveIDs, func(i, j int) bool {
			return driveIDs[i].String() < driveIDs[j].String()
		})

		acct := buildSingleAccountStatusWith(cfg, email, driveIDs, meta, checker)
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
// using injected interfaces for metadata and token checking.
func buildSingleAccountStatusWith(
	cfg *config.Config, email string, driveIDs []driveid.CanonicalID,
	meta accountMetaReader, checker tokenStateChecker,
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

	// Read display name and org name from token metadata.
	acct.DisplayName, acct.OrgName = meta.ReadMeta(email, driveIDs)

	// Check token validity for this account.
	acct.TokenState = checker.CheckToken(context.Background(), email, driveIDs)

	// Build drive status entries.
	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		state := driveState(&d, acct.TokenState)

		syncDir := d.SyncDir
		if syncDir == "" {
			state = driveStateNeedsSetup
		}

		// Use explicit display_name from config, falling back to auto-derived.
		driveDisplayName := d.DisplayName
		if driveDisplayName == "" {
			driveDisplayName = config.DefaultDisplayName(cid)
		}

		acct.Drives = append(acct.Drives, statusDrive{
			CanonicalID: cid.String(),
			DisplayName: driveDisplayName,
			SyncDir:     syncDir,
			State:       state,
		})
	}

	return acct
}

// readAccountMeta reads display name and org name from token file metadata.
func readAccountMeta(account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) (displayName, orgName string) {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		tokenID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenID, nil)
	if tokenPath == "" {
		return "", ""
	}

	meta, err := graph.LoadTokenMeta(tokenPath)
	if err != nil {
		logger.Debug("could not read token meta for status", "error", err)

		return "", ""
	}

	return meta["display_name"], meta["org_name"]
}

// checkTokenState determines whether a valid token exists for the given account.
// Returns "valid", "expired", or "missing".
func checkTokenState(ctx context.Context, account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) string {
	tokenID := canonicalIDForToken(account, driveIDs)
	if tokenID.IsZero() {
		// No drives in config — probe the filesystem for an existing token.
		tokenID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenID, nil)
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
// paused flag and token status.
func driveState(d *config.Drive, tokenState string) string {
	if d.Paused != nil && *d.Paused {
		return driveStatePaused
	}

	if tokenState == tokenStateMissing {
		return driveStateNoToken
	}

	return driveStateReady
}

func printStatusJSON(accounts []statusAccount) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(accounts); err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}

	return nil
}

func printStatusText(accounts []statusAccount) {
	for i, acct := range accounts {
		if i > 0 {
			fmt.Println()
		}

		label := acct.Email
		if acct.DisplayName != "" {
			label = fmt.Sprintf("%s (%s)", acct.DisplayName, acct.Email)
		}

		fmt.Printf("Account: %s [%s]\n", label, acct.DriveType)

		if acct.OrgName != "" {
			fmt.Printf("  Org:   %s\n", acct.OrgName)
		}

		fmt.Printf("  Token: %s\n", acct.TokenState)

		for _, d := range acct.Drives {
			syncDir := d.SyncDir
			if syncDir == "" {
				syncDir = syncDirNotSet
			}

			driveLabel := d.CanonicalID
			if d.DisplayName != "" && d.DisplayName != d.CanonicalID {
				driveLabel = fmt.Sprintf("%s (%s)", d.DisplayName, d.CanonicalID)
			}

			fmt.Printf("  %-50s %-25s %s\n", driveLabel, syncDir, d.State)
		}
	}
}
