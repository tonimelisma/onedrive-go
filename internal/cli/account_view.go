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

func buildAccountViews(
	ctx context.Context,
	cfg *config.Config,
	stored *config.Catalog,
	logger *slog.Logger,
) []accountView {
	grouped, order := groupDrivesByAccount(cfg)
	byEmail := make(map[string]*accountView)

	if stored == nil {
		stored = config.DefaultCatalog()
	}
	populateStoredAccounts(byEmail, stored)
	populateConfiguredAccountViews(byEmail, grouped, order, stored)

	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	slices.Sort(emails)

	result := make([]accountView, 0, len(emails))
	for _, email := range emails {
		view := byEmail[email]
		slices.SortFunc(view.ConfiguredDriveIDs, compareCanonicalID)
		slices.SortFunc(view.TokenDriveIDs, compareCanonicalID)

		view.RepresentativeTokenID = representativeTokenID(view.TokenDriveIDs)
		if view.DriveType == "" {
			view.DriveType = accountViewDriveType(view)
		}
		if view.DisplayName == "" && view.OrgName == "" {
			view.DisplayName, view.OrgName = accountViewNames(view, logger)
		}
		view.StateDBCount = len(config.DiscoverStateDBsForEmail(view.Email, logger))
		view.SavedLoginReason = inspectSavedLogin(ctx, view.Email, accountViewAllDriveIDs(view), logger)
		view.AuthHealth = evaluateAccountViewAuth(view.SavedLoginReason, view.AuthRequirementReason)

		result = append(result, *view)
	}

	return result
}

func ensureAccountView(byEmail map[string]*accountView, email string) *accountView {
	if view, ok := byEmail[email]; ok {
		return view
	}

	view := &accountView{Email: email}
	byEmail[email] = view
	return view
}

func populateStoredAccounts(byEmail map[string]*accountView, stored *config.Catalog) {
	for _, key := range stored.SortedAccountKeys() {
		account := stored.Accounts[key]
		if account.Email == "" {
			continue
		}

		view := ensureAccountView(byEmail, account.Email)
		accountCID, err := driveid.NewCanonicalID(account.CanonicalID)
		if err == nil {
			view.AccountCanonicalID = accountCID
			view.TokenDriveIDs = appendUniqueCanonicalID(view.TokenDriveIDs, accountCID)
		}
		view.DriveType = account.DriveType
		view.UserID = account.UserID
		view.DisplayName = account.DisplayName
		view.OrgName = account.OrgName
		view.AuthRequirementReason = account.AuthRequirementReason
	}
}

func populateConfiguredAccountViews(
	byEmail map[string]*accountView,
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

			view, ok := byEmail[accountCID.Email()]
			if !ok {
				continue
			}

			view.Configured = true
			view.ConfiguredDriveIDs = appendUniqueCanonicalID(view.ConfiguredDriveIDs, cid)
		}
	}
}

func accountViewByEmail(catalog []accountView, email string) (accountView, bool) {
	for i := range catalog {
		if catalog[i].Email == email {
			return catalog[i], true
		}
	}

	return accountView{}, false
}

func accountViewAuthByEmail(views []accountView) map[string]accountAuthHealth {
	result := make(map[string]accountAuthHealth, len(views))
	for i := range views {
		result[views[i].Email] = views[i].AuthHealth
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

func searchableBusinessTokenIDs(views []accountView, accountFilter string) []driveid.CanonicalID {
	var result []driveid.CanonicalID

	for i := range views {
		view := &views[i]
		if accountFilter != "" && view.Email != accountFilter {
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
