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

// resolveLogoutAccount determines the account email for logout from the
// validated offline account view. Plain logout auto-selects only when exactly
// one known account still has a usable saved login. Purge-only cleanup may
// auto-select a single known account even when its saved login is already gone.
func resolveLogoutAccount(ctx context.Context, cfg *config.Config, purge bool, logger *slog.Logger) (string, error) {
	stored, err := config.LoadCatalog()
	if err != nil {
		return "", fmt.Errorf("loading catalog: %w", err)
	}
	return resolveLogoutAccountWithCatalog(ctx, cfg, stored, "", purge, logger)
}

func resolveLogoutAccountWithCatalog(
	ctx context.Context,
	cfg *config.Config,
	stored *config.Catalog,
	accountFlag string,
	purge bool,
	logger *slog.Logger,
) (string, error) {
	if accountFlag != "" {
		return accountFlag, nil
	}

	views, err := buildAccountViews(ctx, cfg, stored, logger)
	if err != nil {
		return "", err
	}
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

// joinAccountViewEmails lists the accounts' emails, one per indented line.
func joinAccountViewEmails(entries []accountView) string {
	emails := make([]string, 0, len(entries))
	for i := range entries {
		emails = append(emails, entries[i].Email)
	}

	return strings.Join(emails, "\n  ")
}

// logoutJSONOutput is `logout --json`: the account signed out, the drives
// removed from the config with it, and whether --purge also deleted their
// state. token_removed is false only for a purge after the token was already
// gone.
type logoutJSONOutput struct {
	Account      string   `json:"account"`
	TokenRemoved bool     `json:"token_removed"`
	Purged       bool     `json:"purged"`
	Drives       []string `json:"drives"`
}

// executeLogout performs the actual logout: finds affected drives, deletes
// the token, removes their config sections, tells a running watch, and with
// purge then deletes their state databases. The human confirmation goes to
// the result writer; the returned result is what --json prints.
func executeLogout(
	ctx context.Context,
	cc *CLIContext,
	cfg *config.Config,
	stored *config.Catalog,
	account string,
	purge bool,
) (logoutJSONOutput, error) {
	w := cc.resultWriter()
	logger := cc.Logger
	affected := drivesForAccount(cfg, account)
	result := logoutJSONOutput{Account: account, Purged: purge, Drives: make([]string, 0, len(affected))}
	for _, cid := range affected {
		result.Drives = append(result.Drives, cid.String())
	}

	removed, err := removeLogoutToken(w, stored, account, affected, purge, logger)
	if err != nil {
		return result, err
	}
	result.TokenRemoved = removed

	if err := printAffectedDrives(w, cfg, affected); err != nil {
		return result, err
	}

	// Each drive's folder as its engine locked it, read before the config
	// forgets it, for the purge's lock.
	syncDirs := make(map[driveid.CanonicalID]string, len(affected))
	for _, cid := range affected {
		syncDirs[cid] = driveSyncDir(cfg, cid, logger)
	}

	removedDrives, configErr := removeAccountDriveConfigs(cc.CfgPath, affected, logger)
	if len(removedDrives) > 0 {
		// The reload retires the drives' runners, which closes their stores
		// before a purge deletes them.
		notifyDaemonIfRunning(ctx, cc)
	}

	if purge {
		return result, executePurgeLogout(ctx, w, account, affected, removedDrives, syncDirs, configErr, logger)
	}
	if configErr != nil {
		return result, fmt.Errorf("removing drive configs: %w", configErr)
	}

	return result, executePlainLogout(w, account, logger)
}

// logoutPurgeCommand is the command that finishes a purge logout whose
// state databases were kept: once the account is gone it finds them by
// email (purgeOrphanedFiles).
func logoutPurgeCommand(account string) string {
	return "onedrive-go logout --purge --account " + account
}

// removeLogoutToken deletes the account's saved login, reporting whether
// there was one to delete.
func removeLogoutToken(
	w io.Writer,
	stored *config.Catalog,
	account string,
	affected []driveid.CanonicalID,
	purge bool,
	logger *slog.Logger,
) (bool, error) {
	tokenCanonicalID := canonicalIDForToken(account, affected)
	if tokenCanonicalID.IsZero() {
		tokenCanonicalID = config.AccountCanonicalIDByEmail(stored, account)
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)
	if tokenPath != "" {
		if err := graph.Logout(tokenPath, logger); err != nil {
			return false, fmt.Errorf("remove token file: %w", err)
		}
		return true, writef(w, "Token removed for %s.\n", account)
	}

	if !purge {
		return false, fmt.Errorf(
			"no saved login was found for %s — run 'onedrive-go logout --purge --account %s' to remove retained state",
			account,
			account,
		)
	}

	return false, writef(w, "Token already removed for %s.\n", account)
}

// executePurgeLogout deletes the state databases of a logged-out account:
// those of the configured drives whose section is gone (removedDrives; a
// drive whose section could not be removed keeps its state, configErr says
// why) and any the account left behind earlier, then removes the account
// from the catalog. Every step runs whatever an earlier one returned, and
// the command fails with all of them: a store an engine still has open is
// kept, and its error names the logout that finishes the purge.
func executePurgeLogout(
	ctx context.Context,
	w io.Writer,
	account string,
	affected []driveid.CanonicalID,
	removedDrives []driveid.CanonicalID,
	syncDirs map[driveid.CanonicalID]string,
	configErr error,
	logger *slog.Logger,
) error {
	var errs []error
	if configErr != nil {
		errs = append(errs, fmt.Errorf("removing drive configs: %w", configErr))
	}
	rerun := logoutPurgeCommand(account)
	if err := deleteAccountDriveStates(ctx, w, removedDrives, syncDirs, rerun, logger); err != nil {
		errs = append(errs, fmt.Errorf("purging account drives: %w", err))
	}
	handled := make(map[string]bool, len(affected))
	for _, cid := range affected {
		handled[config.DriveStatePath(cid)] = true
	}
	if err := purgeOrphanedFiles(ctx, w, account, handled, logger); err != nil {
		errs = append(errs, fmt.Errorf("purging orphaned files: %w", err))
	}
	if err := config.ApplyPurgeLogout(config.DefaultDataDir(), account); err != nil {
		errs = append(errs, fmt.Errorf("updating catalog after purge logout: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

// executePlainLogout finishes a logout without --purge, after the drives'
// config sections were removed: their state databases are kept.
func executePlainLogout(w io.Writer, account string, logger *slog.Logger) error {
	if err := writeln(w, "\nState databases kept. Run 'onedrive-go login' to re-authenticate."); err != nil {
		return err
	}

	// Clearing the persisted auth requirement has one owner, so logout goes
	// through the same path as every other clear and is logged with its source.
	if err := clearAccountAuthRequirementForSource(account, config.AuthClearSourceLogout, logger); err != nil {
		return fmt.Errorf("updating catalog after logout: %w", err)
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

// purgeOrphanedFiles removes the state databases for the given email that
// handled does not name (the configured drives' were purged already). Account
// and cached profile metadata live in the managed catalog, so purge cleanup
// only needs to clear retained per-drive state. Idempotent: a store that is
// not there is nothing to do. This handles cleanup after a prior non-purge
// logout, where config sections and tokens were removed but state DBs
// remained, and finishes a purge that kept a store an engine had open. Each
// store goes with its whole family under its store lock (deleteStateDB);
// their folders are no longer known.
func purgeOrphanedFiles(
	ctx context.Context,
	w io.Writer,
	email string,
	handled map[string]bool,
	logger *slog.Logger,
) error {
	paths, err := config.DiscoverStateDBsForEmail(email, logger)
	if err != nil {
		return fmt.Errorf("finding state databases to purge: %w", err)
	}

	var errs []error
	rerun := logoutPurgeCommand(email)
	for _, path := range paths {
		if handled[path] {
			continue
		}
		removed, deleteErr := deleteStateDB(ctx, path, "", rerun, logger)
		if deleteErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(path), deleteErr))
			continue
		}
		if removed {
			if writeErr := writef(w, "Purged orphaned state DB: %s\n", filepath.Base(path)); writeErr != nil {
				errs = append(errs, writeErr)
			}
		}
	}

	return errors.Join(errs...)
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
// DriveTokenPath handles the SharePoint→business mapping internally. An
// account that cannot form a business ID gives the zero ID on purpose: the
// caller then finds the token through the catalog (removeLogoutToken).
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
