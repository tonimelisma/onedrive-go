package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// resolveLogoutAccount determines the account email for logout. Uses the
// account flag if provided, otherwise auto-selects when there is exactly one
// account. When config is empty (prior logout removed config sections), falls
// back to discovering orphaned account profiles on disk.
func resolveLogoutAccount(cfg *config.Config, accountFlag string, purge bool, logger *slog.Logger) (string, error) {
	if accountFlag != "" {
		return accountFlag, nil
	}

	// Collect unique account emails from configured drives.
	accounts := uniqueAccounts(cfg)

	if len(accounts) == 1 {
		return accounts[0], nil
	}

	if len(accounts) > 1 {
		return "", fmt.Errorf(
			"multiple accounts configured — specify with --account:\n  %s",
			strings.Join(accounts, "\n  "),
		)
	}

	// No accounts in config — check for orphaned account profiles (logged out
	// but not purged). This enables `logout --purge` after a prior `logout`.
	orphanEmails := discoverOrphanedEmails(logger)

	if len(orphanEmails) == 0 {
		return "", fmt.Errorf("no accounts configured — nothing to log out")
	}

	if !purge {
		return "", fmt.Errorf(
			"no accounts configured, but orphaned data remains for:\n  %s\n"+
				"run 'onedrive-go logout --purge --account <email>' to remove",
			strings.Join(orphanEmails, "\n  "),
		)
	}

	if len(orphanEmails) == 1 {
		return orphanEmails[0], nil
	}

	return "", fmt.Errorf(
		"multiple orphaned accounts — specify with --account:\n  %s",
		strings.Join(orphanEmails, "\n  "),
	)
}

// discoverOrphanedEmails returns unique emails from account profiles on disk
// that lack a token file. Used by resolveLogoutAccount to find accounts that
// were logged out but not purged.
func discoverOrphanedEmails(logger *slog.Logger) []string {
	profiles := config.DiscoverAccountProfiles(logger)

	seen := make(map[string]bool)
	var emails []string

	for _, cid := range profiles {
		email := cid.Email()
		if seen[email] {
			continue
		}

		// Check if token still exists — if so, not orphaned.
		tokenPath := config.DriveTokenPath(cid)
		if tokenPath != "" && managedPathExists(tokenPath) {
			continue
		}

		seen[email] = true
		emails = append(emails, email)
	}

	return emails
}

// uniqueAccounts extracts unique account emails from all configured drives.
func uniqueAccounts(cfg *config.Config) []string {
	seen := make(map[string]bool)
	var accounts []string

	for id := range cfg.Drives {
		email := id.Email()
		if !seen[email] {
			seen[email] = true
			accounts = append(accounts, email)
		}
	}

	return accounts
}

// executeLogout performs the actual logout: finds affected drives, deletes
// token, and optionally purges config sections and state databases.
func executeLogout(cfg *config.Config, cfgPath string, w io.Writer, account string, purge bool, logger *slog.Logger) error {
	// Find all drives belonging to this account.
	affected := drivesForAccount(cfg, account)

	// Determine canonical ID for the token path. We need any drive ID with this
	// account email to derive the token path (all drives for one account share a token).
	tokenCanonicalID := canonicalIDForToken(account, affected)
	if tokenCanonicalID.IsZero() {
		// No drives in config — probe the filesystem for an existing token.
		tokenCanonicalID = findTokenFallback(account, logger)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)

	// Delete the token file. graph.Logout handles "not found" gracefully
	// (returns nil), so this works even after a prior logout.
	if tokenPath != "" {
		if err := graph.Logout(tokenPath, logger); err != nil {
			return fmt.Errorf("remove token file: %w", err)
		}

		if err := writef(w, "Token removed for %s.\n", account); err != nil {
			return err
		}
	} else if !purge {
		return fmt.Errorf("cannot determine token path for account %q", account)
	} else {
		// Purge after prior logout — token already gone, that's fine.
		if err := writef(w, "Token already removed for %s.\n", account); err != nil {
			return err
		}
	}

	if err := printAffectedDrives(w, cfg, affected); err != nil {
		return err
	}

	if purge {
		if err := purgeAccountDrives(w, cfgPath, affected, logger); err != nil {
			return fmt.Errorf("purging account drives: %w", err)
		}

		// Also purge orphaned files (state DBs, drive metadata, account profiles)
		// that may remain from a prior non-purge logout or from drives that were
		// removed from config but left data on disk.
		if err := purgeOrphanedFiles(w, account, logger); err != nil {
			return fmt.Errorf("purging orphaned files: %w", err)
		}

		return writeln(w, "Sync directories untouched — your files remain on disk.")
	}

	if clearErr := clearAccountAuthScopes(context.Background(), account, logger); clearErr != nil {
		logger.Warn("clearing stale auth scopes during logout", "account", account, "error", clearErr)
	}

	if err := removeAccountDriveConfigs(cfgPath, affected, logger); err != nil {
		return fmt.Errorf("removing drive configs: %w", err)
	}

	if err := writeln(w, "\nState databases kept. Run 'onedrive-go login' to re-authenticate."); err != nil {
		return err
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

// purgeOrphanedFiles removes state databases, drive metadata files, and
// account profile files for the given email. Idempotent — ignores files
// that don't exist. This handles cleanup after a prior non-purge logout
// where config sections and tokens were removed but data files remained.
func purgeOrphanedFiles(w io.Writer, email string, logger *slog.Logger) error {
	var errs []error

	// Remove orphaned state databases.
	for _, path := range config.DiscoverStateDBsForEmail(email, logger) {
		if err := removeOrphanedPath(
			path,
			logger,
			"failed to remove orphaned state DB",
			"removed orphaned state DB",
			func(removedPath string) error {
				return writef(w, "Purged orphaned state DB: %s\n", filepath.Base(removedPath))
			},
		); err != nil {
			errs = append(errs, err)
		}
	}

	// Remove orphaned drive metadata files.
	for _, path := range config.DiscoverDriveMetadataForEmail(email, logger) {
		if err := removeOrphanedPath(
			path,
			logger,
			"failed to remove orphaned drive metadata",
			"removed orphaned drive metadata",
			nil,
		); err != nil {
			errs = append(errs, err)
		}
	}

	// Remove account profile files. Discover actual profiles on disk rather
	// than guessing drive types, so we handle any type that exists.
	for _, profileCID := range config.DiscoverAccountProfiles(logger) {
		if profileCID.Email() != email {
			continue
		}

		profilePath := config.AccountFilePath(profileCID)
		if profilePath == "" {
			continue
		}

		if err := removeOrphanedPath(
			profilePath,
			logger,
			"failed to remove account profile",
			"removed account profile",
			nil,
		); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func removeOrphanedPath(
	path string,
	logger *slog.Logger,
	warnMsg string,
	infoMsg string,
	onRemoved func(path string) error,
) error {
	removed, err := removeManagedPath(path)
	if err != nil {
		logger.Warn(warnMsg, "path", path, "error", err)
		return fmt.Errorf("removing %s: %w", path, err)
	}
	if !removed {
		return nil
	}

	logger.Info(infoMsg, "path", path)
	if onRemoved != nil {
		if err := onRemoved(path); err != nil {
			return err
		}
	}

	return nil
}

// drivesForAccount returns all canonical IDs whose email matches the given account.
func drivesForAccount(cfg *config.Config, account string) []driveid.CanonicalID {
	var ids []driveid.CanonicalID

	for id := range cfg.Drives {
		if id.Email() == account {
			ids = append(ids, id)
		}
	}

	return ids
}

// canonicalIDForToken picks a canonical ID to use for token path derivation.
// SharePoint drives share the business token, so we prefer a non-sharepoint ID.
// DriveTokenPath handles the SharePoint→business mapping internally.
func canonicalIDForToken(account string, driveIDs []driveid.CanonicalID) driveid.CanonicalID {
	for _, cid := range driveIDs {
		if !cid.IsSharePoint() {
			return cid
		}
	}

	// All drives are SharePoint — derive the business token ID.
	if len(driveIDs) > 0 {
		cid, err := driveid.Construct(driveid.DriveTypeBusiness, account)
		if err != nil {
			return driveid.CanonicalID{}
		}

		return cid
	}

	return driveid.CanonicalID{}
}

// printAffectedDrives lists drives that can no longer sync after logout.
func printAffectedDrives(w io.Writer, cfg *config.Config, affected []driveid.CanonicalID) error {
	if len(affected) == 0 {
		return nil
	}

	if err := writeln(w, "Affected drives (can no longer sync):"); err != nil {
		return err
	}

	for _, id := range affected {
		syncDir := cfg.Drives[id].SyncDir
		if err := writef(w, "  %s (%s)\n", id.String(), syncDir); err != nil {
			return err
		}
	}

	return nil
}
