package cli

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	savedLoginStateMissing = "missing"
	savedLoginStateInvalid = "invalid"
	savedLoginStateUsable  = "usable"
)

type accountCatalogEntry struct {
	AccountCanonicalID    driveid.CanonicalID
	Email                 string
	DriveType             string
	DisplayName           string
	OrgName               string
	Configured            bool
	ConfiguredDriveIDs    []driveid.CanonicalID
	TokenDriveIDs         []driveid.CanonicalID
	RepresentativeTokenID driveid.CanonicalID
	StateDBCount          int
	SavedLoginState       string
	AuthRequirementReason authstate.Reason
	AuthHealth            accountAuthHealth
}

func buildAccountCatalog(ctx context.Context, cfg *config.Config, logger *slog.Logger) []accountCatalogEntry {
	stored, err := config.LoadCatalog()
	if err != nil {
		logger.Debug("loading catalog for account projection", "error", err)
		stored = config.DefaultCatalog()
	}
	return buildAccountCatalogWithStored(ctx, cfg, stored, logger)
}

func buildAccountCatalogWithStored(
	ctx context.Context,
	cfg *config.Config,
	stored *config.Catalog,
	logger *slog.Logger,
) []accountCatalogEntry {
	grouped, order := groupDrivesByAccount(cfg)
	byEmail := make(map[string]*accountCatalogEntry)

	if stored == nil {
		stored = config.DefaultCatalog()
	}
	populateCatalogAccounts(byEmail, stored)
	populateConfiguredAccounts(byEmail, grouped, order, stored)

	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	slices.Sort(emails)

	result := make([]accountCatalogEntry, 0, len(emails))
	for _, email := range emails {
		entry := byEmail[email]
		slices.SortFunc(entry.ConfiguredDriveIDs, compareCanonicalID)
		slices.SortFunc(entry.TokenDriveIDs, compareCanonicalID)

		entry.RepresentativeTokenID = representativeTokenID(entry.TokenDriveIDs)
		if entry.DriveType == "" {
			entry.DriveType = accountCatalogDriveType(entry)
		}
		if entry.DisplayName == "" && entry.OrgName == "" {
			entry.DisplayName, entry.OrgName = accountCatalogNames(entry, logger)
		}
		entry.StateDBCount = len(config.DiscoverStateDBsForEmail(entry.Email, logger))
		entry.SavedLoginState = inspectSavedLoginState(ctx, entry, logger)
		entry.AuthHealth = deriveAccountAuthHealth(entry.SavedLoginState, entry.AuthRequirementReason)

		result = append(result, *entry)
	}

	return result
}

func ensureAccountCatalogEntry(byEmail map[string]*accountCatalogEntry, email string) *accountCatalogEntry {
	if entry, ok := byEmail[email]; ok {
		return entry
	}

	entry := &accountCatalogEntry{Email: email}
	byEmail[email] = entry
	return entry
}

func populateCatalogAccounts(byEmail map[string]*accountCatalogEntry, stored *config.Catalog) {
	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.Email == "" {
			continue
		}

		entry := ensureAccountCatalogEntry(byEmail, account.Email)
		accountCID, err := driveid.NewCanonicalID(account.CanonicalID)
		if err == nil {
			entry.AccountCanonicalID = accountCID
			entry.TokenDriveIDs = appendUniqueCanonicalID(entry.TokenDriveIDs, accountCID)
		}
		entry.DriveType = account.DriveType
		entry.DisplayName = account.DisplayName
		entry.OrgName = account.OrgName
		entry.AuthRequirementReason = account.AuthRequirementReason
	}
}

func populateConfiguredAccounts(
	byEmail map[string]*accountCatalogEntry,
	grouped map[string][]driveid.CanonicalID,
	order []string,
	stored *config.Catalog,
) {
	for _, email := range order {
		for _, cid := range grouped[email] {
			driveRecord, found := stored.DriveByCanonicalID(cid)
			if !found || driveRecord.OwnerAccountCanonical == "" {
				continue
			}

			accountCID, err := driveid.NewCanonicalID(driveRecord.OwnerAccountCanonical)
			if err != nil {
				continue
			}

			entry, ok := byEmail[accountCID.Email()]
			if !ok {
				continue
			}

			entry.Configured = true
			entry.ConfiguredDriveIDs = appendUniqueCanonicalID(entry.ConfiguredDriveIDs, cid)
		}
	}
}

func catalogEntryByEmail(catalog []accountCatalogEntry, email string) (accountCatalogEntry, bool) {
	for i := range catalog {
		if catalog[i].Email == email {
			return catalog[i], true
		}
	}

	return accountCatalogEntry{}, false
}

func catalogAuthByEmail(catalog []accountCatalogEntry) map[string]accountAuthHealth {
	result := make(map[string]accountAuthHealth, len(catalog))
	for i := range catalog {
		result[catalog[i].Email] = catalog[i].AuthHealth
	}

	return result
}

func catalogAuthRequirements(catalog []accountCatalogEntry, include func(accountCatalogEntry) bool) []accountAuthRequirement {
	var result []accountAuthRequirement

	for i := range catalog {
		entry := &catalog[i]
		if !include(*entry) {
			continue
		}
		if entry.AuthHealth.State != authStateAuthenticationNeeded {
			continue
		}

		result = append(result, authRequirement(
			entry.Email,
			entry.DisplayName,
			entry.DriveType,
			entry.StateDBCount,
			entry.AuthHealth,
		))
	}

	sortAccountAuthRequirements(result)

	return result
}

func searchableBusinessTokenIDs(catalog []accountCatalogEntry, accountFilter string) []driveid.CanonicalID {
	var result []driveid.CanonicalID

	for i := range catalog {
		entry := &catalog[i]
		if accountFilter != "" && entry.Email != accountFilter {
			continue
		}
		if entry.DriveType != driveid.DriveTypeBusiness {
			continue
		}
		if entry.AuthHealth.State == authStateAuthenticationNeeded {
			continue
		}
		if entry.RepresentativeTokenID.IsZero() {
			continue
		}

		result = append(result, entry.RepresentativeTokenID)
	}

	slices.SortFunc(result, compareCanonicalID)
	return result
}

func catalogTokenIDs(catalog []accountCatalogEntry) []driveid.CanonicalID {
	var result []driveid.CanonicalID

	for i := range catalog {
		entry := &catalog[i]
		if entry.RepresentativeTokenID.IsZero() {
			continue
		}
		result = appendUniqueCanonicalID(result, entry.RepresentativeTokenID)
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

func accountCatalogDriveType(entry *accountCatalogEntry) string {
	switch {
	case !entry.AccountCanonicalID.IsZero():
		return entry.AccountCanonicalID.DriveType()
	case len(entry.ConfiguredDriveIDs) > 0:
		return accountDriveType(entry.ConfiguredDriveIDs)
	case len(entry.TokenDriveIDs) > 0:
		return accountDriveType(entry.TokenDriveIDs)
	default:
		return ""
	}
}

func accountCatalogNames(entry *accountCatalogEntry, logger *slog.Logger) (string, string) {
	driveIDs := accountCatalogPreferredDriveIDs(entry)
	if len(driveIDs) == 0 {
		return "", ""
	}

	displayName, orgName := readAccountMeta(entry.Email, driveIDs, logger)
	return displayName, orgName
}

func accountCatalogPreferredDriveIDs(entry *accountCatalogEntry) []driveid.CanonicalID {
	switch {
	case len(entry.ConfiguredDriveIDs) > 0:
		return entry.ConfiguredDriveIDs
	case len(entry.TokenDriveIDs) > 0:
		return entry.TokenDriveIDs
	default:
		return nil
	}
}

func accountCatalogAllDriveIDs(entry *accountCatalogEntry) []driveid.CanonicalID {
	ids := make([]driveid.CanonicalID, 0, len(entry.ConfiguredDriveIDs)+len(entry.TokenDriveIDs)+1)
	if !entry.AccountCanonicalID.IsZero() {
		ids = appendUniqueCanonicalID(ids, entry.AccountCanonicalID)
	}
	for _, cid := range entry.ConfiguredDriveIDs {
		ids = appendUniqueCanonicalID(ids, cid)
	}
	for _, cid := range entry.TokenDriveIDs {
		ids = appendUniqueCanonicalID(ids, cid)
	}

	return ids
}

func inspectSavedLoginState(ctx context.Context, entry *accountCatalogEntry, logger *slog.Logger) string {
	reason := inspectSavedLogin(ctx, entry.Email, accountCatalogAllDriveIDs(entry), logger)
	switch reason {
	case authReasonMissingLogin:
		return savedLoginStateMissing
	case authReasonInvalidSavedLogin:
		return savedLoginStateInvalid
	case authReasonSyncAuthRejected:
		return savedLoginStateUsable
	default:
		return savedLoginStateUsable
	}
}

func deriveAccountAuthHealth(savedLoginState string, authRequirementReason authstate.Reason) accountAuthHealth {
	switch savedLoginState {
	case savedLoginStateMissing:
		return authstate.RequiredHealth(authReasonMissingLogin)
	case savedLoginStateInvalid:
		return authstate.RequiredHealth(authReasonInvalidSavedLogin)
	default:
		if authRequirementReason != "" {
			return authstate.RequiredHealth(authRequirementReason)
		}
		return authstate.ReadyHealth()
	}
}

func configAccountCIDForDrive(cid driveid.CanonicalID) driveid.CanonicalID {
	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		return cid
	case cid.IsSharePoint():
		accountCID, err := driveid.Construct(driveid.DriveTypeBusiness, cid.Email())
		if err != nil {
			return driveid.CanonicalID{}
		}
		return accountCID
	default:
		return driveid.CanonicalID{}
	}
}
