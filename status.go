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

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show all accounts, drives, and token status",
		Long: `Display the status of all configured accounts and drives.

Shows token validity, sync directory, and enabled/disabled status for each drive.`,
		RunE: runStatus,
	}
}

// statusAccount groups drives under a single account email.
type statusAccount struct {
	Email      string        `json:"email"`
	DriveType  string        `json:"drive_type"`
	TokenState string        `json:"token_state"`
	Drives     []statusDrive `json:"drives"`
}

// statusDrive holds status information for a single drive.
type statusDrive struct {
	CanonicalID string `json:"canonical_id"`
	SyncDir     string `json:"sync_dir"`
	State       string `json:"state"`
}

func runStatus(_ *cobra.Command, _ []string) error {
	logger := buildLogger()
	cfgPath := resolveLoginConfigPath()

	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Drives) == 0 {
		fmt.Println("No accounts configured. Run 'onedrive-go login' to get started.")

		return nil
	}

	accounts := buildStatusAccounts(cfg, logger)

	if flagJSON {
		return printStatusJSON(accounts)
	}

	printStatusText(accounts)

	return nil
}

// buildStatusAccounts groups configured drives by account email and checks
// token validity for each account.
func buildStatusAccounts(cfg *config.Config, logger *slog.Logger) []statusAccount {
	grouped, order := groupDrivesByAccount(cfg)
	accounts := make([]statusAccount, 0, len(order))

	for _, email := range order {
		driveIDs := grouped[email]
		sort.Slice(driveIDs, func(i, j int) bool {
			return driveIDs[i].String() < driveIDs[j].String()
		})

		acct := buildSingleAccountStatus(cfg, email, driveIDs, logger)
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

// buildSingleAccountStatus builds the status for one account and its drives.
func buildSingleAccountStatus(
	cfg *config.Config, email string, driveIDs []driveid.CanonicalID, logger *slog.Logger,
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

	// Check token validity for this account.
	acct.TokenState = checkTokenState(email, driveIDs, logger)

	// Build drive status entries.
	for _, cid := range driveIDs {
		d := cfg.Drives[cid]
		state := driveState(&d, acct.TokenState)

		acct.Drives = append(acct.Drives, statusDrive{
			CanonicalID: cid.String(),
			SyncDir:     d.SyncDir,
			State:       state,
		})
	}

	return acct
}

// checkTokenState determines whether a valid token exists for the given account.
// Returns "valid", "expired", or "missing".
func checkTokenState(account string, driveIDs []driveid.CanonicalID, logger *slog.Logger) string {
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
	ctx := context.Background()

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
// enabled flag and token status.
func driveState(d *config.Drive, tokenState string) string {
	if d.Enabled != nil && !*d.Enabled {
		return "paused"
	}

	if tokenState == tokenStateMissing {
		return "no token"
	}

	return "ready"
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

		fmt.Printf("Account: %s (%s)\n", acct.Email, acct.DriveType)
		fmt.Printf("  Token: %s\n", acct.TokenState)

		for _, d := range acct.Drives {
			fmt.Printf("  %-35s %-25s %s\n", d.CanonicalID, d.SyncDir, d.State)
		}
	}
}
