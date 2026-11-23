package cli

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

type accountView struct {
	AccountCanonicalID    driveid.CanonicalID
	Email                 string
	DriveType             string
	UserID                string
	DisplayName           string
	OrgName               string
	Configured            bool
	ConfiguredDriveIDs    []driveid.CanonicalID
	TokenDriveIDs         []driveid.CanonicalID
	RepresentativeTokenID driveid.CanonicalID
	StateDBCount          int
	SavedLoginReason      authstate.Reason
	AuthRequirementReason authstate.Reason
	AuthHealth            accountAuthHealth
}

// buildAccountViews projects config and catalog into one view per catalog
// account, named by its canonical ID. A personal and a work or school account
// can share an address, so the address does not name an account: each of the
// two gets its own view, with its own saved login, sign-in requirement,
// configured drives (grouped by the owner their catalog record names) and
// state databases. Views are ordered by address, then canonical ID. It fails
// when the data directory cannot be listed: the state-database counts and
// saved-login checks below would otherwise describe a directory nobody read
// as one with nothing in it.
func buildAccountViews(
	ctx context.Context,
	cfg *config.Config,
	stored *config.Catalog,
	logger *slog.Logger,
) ([]accountView, error) {
	if stored == nil {
		stored = config.DefaultCatalog()
	}
	byKey := populateStoredAccounts(stored)
	populateConfiguredAccountViews(byKey, cfg, stored)

	views := make([]*accountView, 0, len(byKey))
	for _, view := range byKey {
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b *accountView) int {
		return cmp.Or(cmp.Compare(a.Email, b.Email), compareCanonicalID(a.AccountCanonicalID, b.AccountCanonicalID))
	})
	accountsAtAddress := make(map[string]int, len(views))
	for _, view := range views {
		accountsAtAddress[view.Email]++
	}

	result := make([]accountView, 0, len(views))
	for _, view := range views {
		slices.SortFunc(view.ConfiguredDriveIDs, compareCanonicalID)
		slices.SortFunc(view.TokenDriveIDs, compareCanonicalID)

		view.RepresentativeTokenID = representativeTokenID(view.TokenDriveIDs)
		if view.DriveType == "" {
			view.DriveType = accountViewDriveType(view)
		}
		if view.DisplayName == "" && view.OrgName == "" {
			view.DisplayName, view.OrgName = accountViewNames(view, logger)
		}
		statePaths, err := accountStateDBs(view, accountsAtAddress[view.Email] > 1, stored, logger)
		if err != nil {
			return nil, err
		}
		view.StateDBCount = len(statePaths)
		view.SavedLoginReason = inspectSavedLogin(ctx, view.Email, accountViewAllDriveIDs(view), logger)
		view.AuthHealth = evaluateAccountViewAuth(view.SavedLoginReason, view.AuthRequirementReason)

		result = append(result, *view)
	}

	return result, nil
}

// accountViewKey is the key of an account's view: its canonical ID, or its
// address for a catalog account that holds none.
func accountViewKey(view *accountView) string {
	if !view.AccountCanonicalID.IsZero() {
		return view.AccountCanonicalID.String()
	}

	return view.Email
}

// populateStoredAccounts makes one view per catalog account, keyed by
// accountViewKey, with the account's names, drive type, sign-in requirement
// and its own saved login.
func populateStoredAccounts(stored *config.Catalog) map[string]*accountView {
	byKey := make(map[string]*accountView, len(stored.Accounts))
	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.Email == "" {
			continue
		}

		view := &accountView{
			Email:                 account.Email,
			DriveType:             account.DriveType,
			UserID:                account.UserID,
			DisplayName:           account.DisplayName,
			OrgName:               account.OrgName,
			AuthRequirementReason: account.AuthRequirementReason,
		}
		if accountCID, hasID := account.ID(); hasID {
			view.AccountCanonicalID = accountCID
			view.TokenDriveIDs = []driveid.CanonicalID{accountCID}
		}
		byKey[accountViewKey(view)] = view
	}

	return byKey
}

// populateConfiguredAccountViews files each configured drive under the
// account its catalog record names as owner. A drive without a record or an
// owner the catalog holds is in no account's view.
func populateConfiguredAccountViews(byKey map[string]*accountView, cfg *config.Config, stored *config.Catalog) {
	if cfg == nil {
		return
	}
	for cid := range cfg.Drives {
		driveRecord, found := stored.DriveByCanonicalID(cid)
		if !found {
			continue
		}

		accountCID, hasOwner := driveRecord.Owner()
		if !hasOwner {
			continue
		}

		view, ok := byKey[accountCID.String()]
		if !ok {
			continue
		}

		view.Configured = true
		view.ConfiguredDriveIDs = appendUniqueCanonicalID(view.ConfiguredDriveIDs, cid)
	}
}

// accountStateDBs lists the state databases of the account. Stores are named
// by drive ID, which carries the address, so when the address has one
// account every store under it is that account's. When it has two, a store
// goes to the account its drive belongs to: a personal store to the personal
// account, a work or SharePoint store to the work or school account, and
// any store whose catalog record names an owner to that owner. A shared
// folder's store with no owner on record belongs to neither, so neither
// counts it and neither account's purge deletes it.
func accountStateDBs(
	view *accountView,
	sharedAddress bool,
	stored *config.Catalog,
	logger *slog.Logger,
) ([]string, error) {
	paths, err := config.DiscoverStateDBsForEmail(view.Email, logger)
	if err != nil {
		return nil, fmt.Errorf("discover state databases for %s: %w", view.Email, err)
	}
	if !sharedAddress || view.AccountCanonicalID.IsZero() {
		return paths, nil
	}

	owned := make([]string, 0, len(paths))
	for _, path := range paths {
		if stateDBBelongsToAccount(path, view.AccountCanonicalID, stored) {
			owned = append(owned, path)
		}
	}

	return owned, nil
}

// stateDBBelongsToAccount reports whether the state database at path is a
// drive of account, for an address two accounts share (accountStateDBs).
func stateDBBelongsToAccount(path string, account driveid.CanonicalID, stored *config.Catalog) bool {
	if config.DriveStatePath(account) == path {
		return true
	}
	for key := range stored.Drives {
		drive := stored.Drives[key]
		cid, err := driveid.NewCanonicalID(drive.CanonicalID)
		if err != nil || config.DriveStatePath(cid) != path {
			continue
		}
		if owner, hasOwner := drive.Owner(); hasOwner {
			return owner.Equal(account)
		}

		break
	}

	name := filepath.Base(path)
	switch {
	case account.IsPersonal():
		return strings.HasPrefix(name, stateDBNamePrefix(driveid.DriveTypePersonal))
	case account.IsBusiness():
		return strings.HasPrefix(name, stateDBNamePrefix(driveid.DriveTypeBusiness)) ||
			strings.HasPrefix(name, stateDBNamePrefix(driveid.DriveTypeSharePoint))
	default:
		return false
	}
}

// stateDBNamePrefix is the start of the file name of every state database of
// a drive of driveType.
func stateDBNamePrefix(driveType string) string {
	return "state_" + driveType + "_"
}

// accountViewByCanonicalID finds the view of the account named by account.
func accountViewByCanonicalID(views []accountView, account driveid.CanonicalID) (accountView, bool) {
	for i := range views {
		if views[i].AccountCanonicalID.Equal(account) {
			return views[i], true
		}
	}

	return accountView{}, false
}

// accountViewAuthByDrive is each configured drive's account sign-in health,
// from the view of the account that owns the drive.
func accountViewAuthByDrive(views []accountView) map[driveid.CanonicalID]accountAuthHealth {
	result := make(map[driveid.CanonicalID]accountAuthHealth)
	for i := range views {
		for _, cid := range views[i].ConfiguredDriveIDs {
			result[cid] = views[i].AuthHealth
		}
	}

	return result
}

func accountViewAuthRequirements(views []accountView, include func(accountView) bool) []accountAuthRequirement {
	var result []accountAuthRequirement

	for i := range views {
		view := &views[i]
		if !include(*view) {
			continue
		}
		if view.AuthHealth.State != authStateAuthenticationNeeded {
			continue
		}

		result = append(result, authRequirement(
			view.Email,
			view.DisplayName,
			view.DriveType,
			view.StateDBCount,
			view.AuthHealth,
		))
	}

	sortAccountAuthRequirements(result)

	return result
}

// searchableBusinessTokenIDs lists the sign-ins of the work or school
// accounts SharePoint search can ask: those of the named account alone when
// account is not nil.
func searchableBusinessTokenIDs(views []accountView, account *accountTarget) []driveid.CanonicalID {
	var result []driveid.CanonicalID

	for i := range views {
		view := &views[i]
		if account != nil && !account.includesView(view) {
			continue
		}
		if view.DriveType != driveid.DriveTypeBusiness {
			continue
		}
		if view.AuthHealth.State == authStateAuthenticationNeeded {
			continue
		}
		if view.RepresentativeTokenID.IsZero() {
			continue
		}

		result = append(result, view.RepresentativeTokenID)
	}

	slices.SortFunc(result, compareCanonicalID)
	return result
}

func accountViewTokenIDs(views []accountView) []driveid.CanonicalID {
	var result []driveid.CanonicalID

	for i := range views {
		view := &views[i]
		if view.RepresentativeTokenID.IsZero() {
			continue
		}
		result = appendUniqueCanonicalID(result, view.RepresentativeTokenID)
	}

	slices.SortFunc(result, compareCanonicalID)
	return result
}

func appendUniqueCanonicalID(ids []driveid.CanonicalID, cid driveid.CanonicalID) []driveid.CanonicalID {
	for _, existing := range ids {
		if existing == cid {
			return ids
		}
	}

	return append(ids, cid)
}

func compareCanonicalID(a, b driveid.CanonicalID) int {
	return cmp.Compare(a.String(), b.String())
}

func representativeTokenID(ids []driveid.CanonicalID) driveid.CanonicalID {
	return canonicalIDForToken("", ids)
}

func accountDriveType(ids []driveid.CanonicalID) string {
	for _, cid := range ids {
		if !cid.IsSharePoint() {
			return cid.DriveType()
		}
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[0].DriveType()
}

func accountViewDriveType(view *accountView) string {
	switch {
	case !view.AccountCanonicalID.IsZero():
		return view.AccountCanonicalID.DriveType()
	case len(view.ConfiguredDriveIDs) > 0:
		return accountDriveType(view.ConfiguredDriveIDs)
	case len(view.TokenDriveIDs) > 0:
		return accountDriveType(view.TokenDriveIDs)
	default:
		return ""
	}
}

func accountViewNames(view *accountView, logger *slog.Logger) (string, string) {
	driveIDs := accountViewPreferredDriveIDs(view)
	if len(driveIDs) == 0 {
		return "", ""
	}

	displayName, orgName := readAccountMeta(view.Email, driveIDs, logger)
	return displayName, orgName
}

func accountViewPreferredDriveIDs(view *accountView) []driveid.CanonicalID {
	switch {
	case len(view.ConfiguredDriveIDs) > 0:
		return view.ConfiguredDriveIDs
	case len(view.TokenDriveIDs) > 0:
		return view.TokenDriveIDs
	default:
		return nil
	}
}

func accountViewAllDriveIDs(view *accountView) []driveid.CanonicalID {
	ids := make([]driveid.CanonicalID, 0, len(view.ConfiguredDriveIDs)+len(view.TokenDriveIDs)+1)
	if !view.AccountCanonicalID.IsZero() {
		ids = appendUniqueCanonicalID(ids, view.AccountCanonicalID)
	}
	for _, cid := range view.ConfiguredDriveIDs {
		ids = appendUniqueCanonicalID(ids, cid)
	}
	for _, cid := range view.TokenDriveIDs {
		ids = appendUniqueCanonicalID(ids, cid)
	}

	return ids
}

func evaluateAccountViewAuth(savedLoginReason, authRequirementReason authstate.Reason) accountAuthHealth {
	if savedLoginReason != "" {
		return authstate.RequiredHealth(savedLoginReason)
	}
	if authRequirementReason != "" {
		return authstate.RequiredHealth(authRequirementReason)
	}
	return authstate.ReadyHealth()
}

// accountTarget is the account an --account selector names.
type accountTarget struct {
	// Selector is what --account names it by, as the user gave it or as
	// accountViewSelector chose it; logout's purge rerun command repeats it.
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

// resolveAccountSelector reads an --account selector: an email, or the
// canonical ID of a personal or work or school account. An email that two
// catalog accounts share is refused, naming the canonical IDs that tell them
// apart, so every command that takes --account names one account: logout
// deletes a saved login and drive sections, and guessing which account was
// meant could delete the other one's; status would otherwise mix the two.
// action completes "name the account to ..." in that refusal.
func resolveAccountSelector(stored *config.Catalog, selector, action string) (accountTarget, error) {
	if strings.Contains(selector, ":") {
		cid, err := driveid.NewCanonicalID(selector)
		if err != nil || (!cid.IsPersonal() && !cid.IsBusiness()) {
			return accountTarget{}, fmt.Errorf(
				"--account %q is neither an email nor an account ID such as personal:<email> or business:<email>",
				selector)
		}

		return accountTarget{
			Selector:      selector,
			Email:         cid.Email(),
			Account:       cid,
			SharedAddress: len(catalogAccountsAtEmail(stored, cid.Email())) > 1,
		}, nil
	}

	accounts := catalogAccountsAtEmail(stored, selector)
	switch len(accounts) {
	case 0:
		return accountTarget{Selector: selector, Email: selector}, nil
	case 1:
		return accountTarget{Selector: selector, Email: selector, Account: accounts[0]}, nil
	default:
		names := make([]string, 0, len(accounts))
		for _, cid := range accounts {
			names = append(names, "--account "+cid.String())
		}

		return accountTarget{}, fmt.Errorf(
			"%s is the email of more than one account; name the account to %s:\n  %s",
			selector, action, strings.Join(names, "\n  "))
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

// drives is the configured drives of the named account (the ones a logout
// removes): every drive under the address, or, when a second account shares
// it, those of the named account alone. A drive goes with the owner its catalog record names; without one
// a personal drive is the personal account's and a work or SharePoint
// drive the work or school account's, and a shared folder is neither's, so
// its section stays.
func (t *accountTarget) drives(cfg *config.Config, stored *config.Catalog) []driveid.CanonicalID {
	all := drivesForAccount(cfg, t.Email)
	if !t.SharedAddress || t.Account.IsZero() {
		return all
	}

	owned := make([]driveid.CanonicalID, 0, len(all))
	for _, cid := range all {
		if t.includesDrive(cid, stored) {
			owned = append(owned, cid)
		}
	}

	return owned
}

// selectedAccount reads --account through resolveAccountSelector; selected
// is false when the flag is not given. action completes the refusal of a
// shared email.
func (cc *CLIContext) selectedAccount(stored *config.Catalog, action string) (target accountTarget, selected bool, err error) {
	if cc.Flags.Account == "" {
		return accountTarget{}, false, nil
	}

	target, err = resolveAccountSelector(stored, cc.Flags.Account, action)
	if err != nil {
		return accountTarget{}, false, err
	}

	return target, true, nil
}

// orNone is the target, or nil when nothing was selected.
func (t *accountTarget) orNone(selected bool) *accountTarget {
	if !selected {
		return nil
	}

	return t
}

// includesDrive reports whether the configured drive cid is one of the named
// account's drives (the rule of drives).
func (t *accountTarget) includesDrive(cid driveid.CanonicalID, stored *config.Catalog) bool {
	if cid.Email() != t.Email {
		return false
	}

	return !t.SharedAddress || t.Account.IsZero() || driveBelongsToAccount(cid, t.Account, stored)
}

// includesView reports whether the account view is the named account's: at
// its address, and, when a second account shares the address, the named
// one's.
func (t *accountTarget) includesView(view *accountView) bool {
	if view.Email != t.Email {
		return false
	}

	return !t.SharedAddress || t.Account.IsZero() || view.AccountCanonicalID.Equal(t.Account)
}

// driveBelongsToAccount reports whether the configured drive cid belongs to
// account: the owner its catalog record names, or else the account of its
// kind at the address (a personal drive the personal account's, a work or
// SharePoint drive the work or school account's); a shared folder without
// a record belongs to neither.
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
