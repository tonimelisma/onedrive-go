package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// resolveLogoutAccount determines the account email for logout from the
// validated offline account view. Plain logout auto-selects only when exactly
// one known account still has a usable saved login. Purge-only cleanup may
// auto-select a single known account even when its saved login is already gone.
func resolveLogoutAccount(cfg *config.Config, purge bool, logger *slog.Logger) (string, error) {
	stored, err := config.LoadCatalog()
	if err != nil {
		return "", fmt.Errorf("loading catalog: %w", err)
	}
	return resolveLogoutAccountWithCatalog(cfg, stored, "", purge, logger)
}

func resolveLogoutAccountWithCatalog(
	cfg *config.Config,
	stored *config.Catalog,
	accountFlag string,
	purge bool,
	logger *slog.Logger,
) (string, error) {
	if accountFlag != "" {
		return accountFlag, nil
	}

	views := buildAccountViews(context.Background(), cfg, stored, logger)
	known := knownAccountViews(views)
	if len(known) == 0 {
		return "", fmt.Errorf("no accounts configured — nothing to log out")
	}

	selectable := logoutSelectableAccountViews(views)
	if len(selectable) == 1 {
		return selectable[0].Email, nil
	}

	if len(selectable) > 1 {
		return "", fmt.Errorf(
			"multiple accounts with saved logins — specify with --account:\n  %s",
			joinAccountViewEmails(selectable),
		)
	}

	if purge && len(known) == 1 {
		return known[0].Email, nil
	}

	return "", fmt.Errorf(
		"no accounts with saved logins are available for plain logout.\n"+
			"Retained data remains for:\n  %s\n"+
			"Run 'onedrive-go logout --purge --account <email>' to remove retained state",
		joinAccountViewEmails(known),
	)
}

func joinAccountViewEmails(entries []accountView) string {
	result := ""
	for i := range entries {
		if i > 0 {
			result += "\n  "
		}
		result += entries[i].Email
	}

	return result
}

// executeLogout performs the actual logout: finds affected drives, deletes
// token, and optionally purges config sections and state databases.
func executeLogout(
	cfg *config.Config,
	stored *config.Catalog,
	cfgPath string,
	w io.Writer,
	account string,
	purge bool,
	logger *slog.Logger,
) error {
	affected := drivesForAccount(cfg, account)
	if err := removeLogoutToken(w, stored, account, affected, purge, logger); err != nil {
		return err
	}

	if err := printAffectedDrives(w, cfg, affected); err != nil {
		return err
	}

	if purge {
		return executePurgeLogout(cfgPath, w, account, affected, logger)
	}

	return executePlainLogout(cfgPath, w, account, affected, logger)
}

func removeLogoutToken(
	w io.Writer,
	stored *config.Catalog,
	account string,
	affected []driveid.CanonicalID,
	purge bool,
	logger *slog.Logger,
) error {
	tokenCanonicalID := canonicalIDForToken(account, affected)
	if tokenCanonicalID.IsZero() {
		tokenCanonicalID = config.AccountCanonicalIDByEmail(stored, account)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)
	if tokenPath != "" {
		if err := graph.Logout(tokenPath, logger); err != nil {
			return fmt.Errorf("remove token file: %w", err)
		}
		return writef(w, "Token removed for %s.\n", account)
	}

	if !purge {
		return fmt.Errorf(
			"no saved login was found for %s — run 'onedrive-go logout --purge --account %s' to remove retained state",
			account,
			account,
		)
	}

	return writef(w, "Token already removed for %s.\n", account)
}

func executePurgeLogout(
	cfgPath string,
	w io.Writer,
	account string,
	affected []driveid.CanonicalID,
	logger *slog.Logger,
) error {
	if err := purgeAccountDrives(w, cfgPath, affected, logger); err != nil {
		return fmt.Errorf("purging account drives: %w", err)
	}
	if err := purgeOrphanedFiles(w, account, logger); err != nil {
		return fmt.Errorf("purging orphaned files: %w", err)
	}

	if err := config.ApplyPurgeLogout(config.DefaultDataDir(), account); err != nil {
		return fmt.Errorf("updating catalog after purge logout: %w", err)
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

func executePlainLogout(
	cfgPath string,
	w io.Writer,
	account string,
	affected []driveid.CanonicalID,
	logger *slog.Logger,
) error {
	if err := removeAccountDriveConfigs(cfgPath, affected, logger); err != nil {
		return fmt.Errorf("removing drive configs: %w", err)
	}

	if err := writeln(w, "\nState databases kept. Run 'onedrive-go login' to re-authenticate."); err != nil {
		return err
	}

	if err := config.ApplyPlainLogout(config.DefaultDataDir(), account); err != nil {
		return fmt.Errorf("updating catalog after logout: %w", err)
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

// purgeOrphanedFiles removes state databases for the given email. Inventory and
// cached profile metadata live in the managed catalog, so purge cleanup only
// needs to clear retained per-drive state files. Idempotent — ignores files
// that don't exist. This handles cleanup after a prior non-purge logout where
// config sections and tokens were removed but state DBs remained.
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
