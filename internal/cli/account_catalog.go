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
	Email                 string
	DriveType             string
	DisplayName           string
	OrgName               string
	Configured            bool
	ConfiguredDriveIDs    []driveid.CanonicalID
	ProfileDriveIDs       []driveid.CanonicalID
	TokenDriveIDs         []driveid.CanonicalID
	RepresentativeTokenID driveid.CanonicalID
	StateDBCount          int
	SavedLoginState       string
	HasPersistedAuthScope bool
	AuthHealth            accountAuthHealth
}

func buildAccountCatalog(ctx context.Context, cfg *config.Config, logger *slog.Logger) []accountCatalogEntry {
	grouped, order := groupDrivesByAccount(cfg)
	byEmail := make(map[string]*accountCatalogEntry, len(order))

	ensureEntry := func(email string) *accountCatalogEntry {
		entry, ok := byEmail[email]
		if ok {
			return entry
		}

		entry = &accountCatalogEntry{Email: email}
		byEmail[email] = entry
		return entry
	}

	for _, email := range order {
		entry := ensureEntry(email)
		entry.Configured = true
		for _, cid := range grouped[email] {
			entry.ConfiguredDriveIDs = appendUniqueCanonicalID(entry.ConfiguredDriveIDs, cid)
		}
	}

	for _, cid := range config.DiscoverAccountProfiles(logger) {
		entry := ensureEntry(cid.Email())
		entry.ProfileDriveIDs = appendUniqueCanonicalID(entry.ProfileDriveIDs, cid)
	}

	for _, cid := range config.DiscoverTokens(logger) {
		entry := ensureEntry(cid.Email())
		entry.TokenDriveIDs = appendUniqueCanonicalID(entry.TokenDriveIDs, cid)
	}

	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	slices.Sort(emails)

	result := make([]accountCatalogEntry, 0, len(emails))
	for _, email := range emails {
		entry := byEmail[email]
		slices.SortFunc(entry.ConfiguredDriveIDs, compareCanonicalID)
		slices.SortFunc(entry.ProfileDriveIDs, compareCanonicalID)
		slices.SortFunc(entry.TokenDriveIDs, compareCanonicalID)

		entry.RepresentativeTokenID = representativeTokenID(entry.TokenDriveIDs)
		entry.DriveType = accountCatalogDriveType(entry)
		entry.DisplayName, entry.OrgName = accountCatalogNames(entry, logger)
		entry.StateDBCount = len(config.DiscoverStateDBsForEmail(entry.Email, logger))
		entry.SavedLoginState = inspectSavedLoginState(ctx, entry, logger)
		entry.HasPersistedAuthScope = hasPersistedAuthScope(ctx, entry.Email, logger)
		entry.AuthHealth = deriveAccountAuthHealth(entry.SavedLoginState, entry.HasPersistedAuthScope)

		result = append(result, *entry)
	}

	return result
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
	case len(entry.ConfiguredDriveIDs) > 0:
		return accountDriveType(entry.ConfiguredDriveIDs)
	case len(entry.TokenDriveIDs) > 0:
		return accountDriveType(entry.TokenDriveIDs)
	case len(entry.ProfileDriveIDs) > 0:
		return accountDriveType(entry.ProfileDriveIDs)
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
		return entry.ProfileDriveIDs
	}
}

func accountCatalogAllDriveIDs(entry *accountCatalogEntry) []driveid.CanonicalID {
	ids := make([]driveid.CanonicalID, 0, len(entry.ConfiguredDriveIDs)+len(entry.ProfileDriveIDs)+len(entry.TokenDriveIDs))
	for _, cid := range entry.ConfiguredDriveIDs {
		ids = appendUniqueCanonicalID(ids, cid)
	}
	for _, cid := range entry.ProfileDriveIDs {
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
	default:
		return savedLoginStateUsable
	}
}

func deriveAccountAuthHealth(savedLoginState string, hasPersistedAuthScope bool) accountAuthHealth {
	switch savedLoginState {
	case savedLoginStateMissing:
		return authstate.RequiredHealth(authReasonMissingLogin)
	case savedLoginStateInvalid:
		return authstate.RequiredHealth(authReasonInvalidSavedLogin)
	default:
		if hasPersistedAuthScope {
			return authstate.RequiredHealth(authReasonSyncAuthRejected)
		}
		return authstate.ReadyHealth()
	}
}
