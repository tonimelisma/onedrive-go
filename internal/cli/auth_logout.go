package cli

import (
	"cmp"
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
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// resolveLogoutAccountWithCatalog determines the account selector for logout
// from the offline account view: --account when given, after
// resolveLogoutTarget accepts it; otherwise plain logout auto-selects only
// when exactly one known account still has a usable saved login, and
// purge-only cleanup may auto-select a single known account even when its
// saved login is already gone. The selector is the account's email, or its
// canonical ID when a second account shares that email.
func resolveLogoutAccountWithCatalog(
	ctx context.Context,
	cfg *config.Config,
	stored *config.Catalog,
	accountFlag string,
	purge bool,
	logger *slog.Logger,
) (string, error) {
	if accountFlag != "" {
		if _, err := resolveLogoutTarget(stored, accountFlag); err != nil {
			return "", err
		}

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
		return accountViewSelector(views, &selectable[0]), nil
	}

	if len(selectable) > 1 {
		return "", fmt.Errorf(
			"multiple accounts with saved logins — specify with --account:\n  %s",
			joinAccountViewSelectors(views, selectable),
		)
	}

	if purge && len(known) == 1 {
		return accountViewSelector(views, &known[0]), nil
	}

	return "", fmt.Errorf(
		"no accounts with saved logins are available for plain logout.\n"+
			"Retained data remains for:\n  %s\n"+
			"Run 'onedrive-go logout --purge --account <email>' to remove retained state",
		joinAccountViewSelectors(views, known),
	)
}

// accountViewSelector is what --account takes to name the view's account:
// its email, or its canonical ID when another of the views has the same
// email (a personal and a work or school account at one address).
func accountViewSelector(views []accountView, view *accountView) string {
	for i := range views {
		if views[i].Email == view.Email && !views[i].AccountCanonicalID.Equal(view.AccountCanonicalID) &&
			!view.AccountCanonicalID.IsZero() {
			return view.AccountCanonicalID.String()
		}
	}

	return view.Email
}

// joinAccountViewSelectors lists the accounts' selectors, one per indented
// line.
func joinAccountViewSelectors(views, entries []accountView) string {
	selectors := make([]string, 0, len(entries))
	for i := range entries {
		selectors = append(selectors, accountViewSelector(views, &entries[i]))
	}

	return strings.Join(selectors, "\n  ")
}

// logoutTarget is the account a logout acts on.
type logoutTarget struct {
	// Selector is what --account names it by, as the user gave it or as
	// accountViewSelector chose it; the purge's rerun command repeats it.
	Selector string
	Email    string
	// Account is the catalog account, zero when the catalog holds none at
	// Email (a purge of what an account left behind).
	Account driveid.CanonicalID
	// SharedAddress means a second catalog account has the same email, so
	// a drive or a state database under the address belongs to only one of
	// them.
	SharedAddress bool
}

// resolveLogoutTarget reads an --account selector: an email, or the
// canonical ID of a personal or work or school account. An email that two
// catalog accounts share is refused, naming the canonical IDs that tell them
// apart: logout deletes a saved login and drive sections, and guessing which
// account was meant could delete the other one's.
func resolveLogoutTarget(stored *config.Catalog, selector string) (logoutTarget, error) {
	if strings.Contains(selector, ":") {
		cid, err := driveid.NewCanonicalID(selector)
		if err != nil || (!cid.IsPersonal() && !cid.IsBusiness()) {
			return logoutTarget{}, fmt.Errorf(
				"--account %q is neither an email nor an account ID such as personal:<email> or business:<email>",
				selector)
		}

		return logoutTarget{
			Selector:      selector,
			Email:         cid.Email(),
			Account:       cid,
			SharedAddress: len(catalogAccountsAtEmail(stored, cid.Email())) > 1,
		}, nil
	}

	accounts := catalogAccountsAtEmail(stored, selector)
	switch len(accounts) {
	case 0:
		return logoutTarget{Selector: selector, Email: selector}, nil
	case 1:
		return logoutTarget{Selector: selector, Email: selector, Account: accounts[0]}, nil
	default:
		names := make([]string, 0, len(accounts))
		for _, cid := range accounts {
			names = append(names, "--account "+cid.String())
		}

		return logoutTarget{}, fmt.Errorf(
			"%s is the email of more than one account; name the account to log out:\n  %s",
			selector, strings.Join(names, "\n  "))
	}
}

// catalogAccountsAtEmail lists the canonical IDs of the catalog accounts
// with email, sorted.
func catalogAccountsAtEmail(stored *config.Catalog, email string) []driveid.CanonicalID {
	if stored == nil || email == "" {
		return nil
	}

	var accounts []driveid.CanonicalID
	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.Email != email {
			continue
		}
		if cid, hasID := account.ID(); hasID {
			accounts = append(accounts, cid)
		}
	}

	return accounts
}

// drives is the configured drives the logout removes: every drive under the
// address, or, when a second account shares it, those of the named account
// alone. A drive goes with the owner its catalog record names; without one
// a personal drive is the personal account's and a work or SharePoint
// drive the work or school account's, and a shared folder is neither's, so
// its section stays.
func (t *logoutTarget) drives(cfg *config.Config, stored *config.Catalog) []driveid.CanonicalID {
	all := drivesForAccount(cfg, t.Email)
	if !t.SharedAddress || t.Account.IsZero() {
		return all
	}

	owned := make([]driveid.CanonicalID, 0, len(all))
	for _, cid := range all {
		if driveBelongsToAccount(cid, t.Account, stored) {
			owned = append(owned, cid)
		}
	}

	return owned
}

// ownsStateDB reports whether the state database at path is one the logout
// may purge (accountStateDBs).
func (t *logoutTarget) ownsStateDB(stored *config.Catalog) func(string) bool {
	if !t.SharedAddress || t.Account.IsZero() {
		return nil
	}

	return func(path string) bool {
		return stateDBBelongsToAccount(path, t.Account, stored)
	}
}

func driveBelongsToAccount(cid, account driveid.CanonicalID, stored *config.Catalog) bool {
	if record, found := stored.DriveByCanonicalID(cid); found {
		if owner, hasOwner := record.Owner(); hasOwner {
			return owner.Equal(account)
		}
	}

	switch {
	case cid.IsPersonal():
		return account.IsPersonal()
	case cid.IsBusiness(), cid.IsSharePoint():
		return account.IsBusiness()
	default:
		return false
	}
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

func (out logoutJSONOutput) withEmptyLists() logoutJSONOutput {
	out.Drives = emptyIfNil(out.Drives)

	return out
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
	target, err := resolveLogoutTarget(stored, account)
	if err != nil {
		return logoutJSONOutput{Account: account, Purged: purge}, err
	}
	affected := target.drives(cfg, stored)
	result := logoutJSONOutput{Account: target.Email, Purged: purge}
	for _, cid := range affected {
		result.Drives = append(result.Drives, cid.String())
	}

	removedToken, err := removeLogoutToken(ctx, w, stored, &target, affected, purge, logger)
	if err != nil {
		return result, err
	}
	result.TokenRemoved = !removedToken.IsZero()

	// Each drive's folder as its engine locked it, read before the config
	// forgets it, for the listing below and the purge's lock.
	syncDirs := make(map[driveid.CanonicalID]string, len(affected))
	for _, cid := range affected {
		syncDirs[cid] = driveSyncDir(cfg, cid, logger)
	}

	if err := printAffectedDrives(w, affected, syncDirs); err != nil {
		return result, err
	}

	removedDrives, configErr := removeAccountDriveConfigs(ctx, cc.CfgPath, affected, logger)
	if len(removedDrives) > 0 || result.TokenRemoved {
		// The reload retires the drives' runners, which closes their stores
		// before a purge deletes them, and drops the running watch's token
		// sources, so none of them refreshes the removed sign-in.
		notifyDaemonIfRunning(ctx, cc)
	}

	if purge {
		return result, executePurgeLogout(ctx, w, stored, &target, affected, removedDrives, syncDirs, configErr, logger)
	}
	if configErr != nil {
		return result, fmt.Errorf("removing drive configs: %w", configErr)
	}

	return result, executePlainLogout(ctx, w, removedToken, logger)
}

// logoutPurgeCommand is the command that finishes a purge logout whose
// state databases were kept: once the account is gone it finds them by
// email (purgeOrphanedFiles).
func logoutPurgeCommand(account string) string {
	return "onedrive-go logout --purge --account " + account
}

// removeLogoutToken deletes the account's saved login. It returns the
// canonical ID the token was found by, the account itself or a drive of it,
// or zero when there was no token to delete. A named catalog account's token
// is its own, whichever drives share its address.
func removeLogoutToken(
	ctx context.Context,
	w io.Writer,
	stored *config.Catalog,
	target *logoutTarget,
	affected []driveid.CanonicalID,
	purge bool,
	logger *slog.Logger,
) (driveid.CanonicalID, error) {
	account := target.Email
	tokenCanonicalID := target.Account
	if tokenCanonicalID.IsZero() {
		tokenCanonicalID = canonicalIDForToken(account, affected)
	}
	if tokenCanonicalID.IsZero() {
		storedCID, err := config.AccountCanonicalIDByEmail(stored, account)
		if err != nil {
			return driveid.CanonicalID{}, fmt.Errorf("finding the saved login of %s: %w", account, err)
		}
		tokenCanonicalID = storedCID
	}

	tokenPath := config.DriveTokenPath(tokenCanonicalID)
	if tokenPath != "" {
		if err := graph.Logout(ctx, tokenPath, logger); err != nil {
			return driveid.CanonicalID{}, fmt.Errorf("remove token file: %w", err)
		}
		if purge {
			if err := tokenfile.RemoveLock(tokenPath); err != nil {
				return driveid.CanonicalID{}, fmt.Errorf("remove token lock file: %w", err)
			}
		}
		return tokenCanonicalID, writef(w, "Token removed for %s.\n", account)
	}

	if !purge {
		return driveid.CanonicalID{}, fmt.Errorf(
			"no saved login was found for %s — run 'onedrive-go logout --purge --account %s' to remove retained state",
			account,
			target.Selector,
		)
	}

	return driveid.CanonicalID{}, writef(w, "Token already removed for %s.\n", account)
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
	stored *config.Catalog,
	target *logoutTarget,
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
	rerun := logoutPurgeCommand(target.Selector)
	if err := deleteAccountDriveStates(ctx, w, removedDrives, syncDirs, rerun, logger); err != nil {
		errs = append(errs, fmt.Errorf("purging account drives: %w", err))
	}
	handled := make(map[string]bool, len(affected))
	for _, cid := range affected {
		handled[config.DriveStatePath(cid)] = true
	}
	orphans := purgeScope{email: target.Email, rerunSelector: target.Selector, owns: target.ownsStateDB(stored)}
	if err := purgeOrphanedFiles(ctx, w, orphans, handled, logger); err != nil {
		errs = append(errs, fmt.Errorf("purging orphaned files: %w", err))
	}
	if err := config.ApplyPurgeLogout(ctx, config.DefaultDataDir(), target.Account); err != nil {
		errs = append(errs, fmt.Errorf("updating catalog after purge logout: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	return writeln(w, "Sync directories untouched — your files remain on disk.")
}

// executePlainLogout finishes a logout without --purge, after the drives'
// config sections were removed: their state databases are kept. removedToken
// is what removeLogoutToken found the deleted token by.
func executePlainLogout(ctx context.Context, w io.Writer, removedToken driveid.CanonicalID, logger *slog.Logger) error {
	if err := writeln(w, "\nState databases kept. Run 'onedrive-go login' to re-authenticate."); err != nil {
		return err
	}

	// Clearing the persisted auth requirement has one owner, so logout goes
	// through the same path as every other clear and is logged with its source.
	// It clears the account whose token was deleted, by canonical ID: another
	// account at the same address keeps its sign-in and its requirement.
	account, err := config.TokenAccountCanonicalID(removedToken)
	if err != nil {
		return fmt.Errorf("updating catalog after logout: %w", err)
	}
	if err := clearAccountAuthRequirementForSource(ctx, account, config.AuthClearSourceLogout, logger); err != nil {
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
	scope purgeScope,
	handled map[string]bool,
	logger *slog.Logger,
) error {
	paths, err := config.DiscoverStateDBsForEmail(scope.email, logger)
	if err != nil {
		return fmt.Errorf("finding state databases to purge: %w", err)
	}

	var errs []error
	rerun := logoutPurgeCommand(cmp.Or(scope.rerunSelector, scope.email))
	for _, path := range paths {
		if handled[path] || (scope.owns != nil && !scope.owns(path)) {
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

// purgeScope is the state databases a purge logout looks for under an
// address: all of them, or, when owns is set, those owns accepts (the named
// account's, when a second account shares the address).
type purgeScope struct {
	email         string
	rerunSelector string
	owns          func(path string) bool
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

// printAffectedDrives lists drives that can no longer sync after logout, each
// with the folder it synced, also when its section left sync_dir to the
// default.
func printAffectedDrives(w io.Writer, affected []driveid.CanonicalID, syncDirs map[driveid.CanonicalID]string) error {
	if len(affected) == 0 {
		return nil
	}

	if err := writeln(w, "Affected drives (can no longer sync):"); err != nil {
		return err
	}

	for _, id := range affected {
		if err := writef(w, "  %s (%s)\n", id.String(), syncDirs[id]); err != nil {
			return err
		}
	}

	return nil
}
