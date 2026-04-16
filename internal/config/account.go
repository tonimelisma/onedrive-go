package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// AccountProfile holds cached API data about the account owner. Persisted in
// the managed catalog and updated on every login.
type AccountProfile struct {
	UserID         string `json:"user_id"`
	DisplayName    string `json:"display_name"`
	OrgName        string `json:"org_name,omitempty"`
	PrimaryDriveID string `json:"primary_drive_id"`
}

// AccountFilePath returns the legacy path shape for an account profile file.
// The managed catalog now owns account profiles in steady state; this helper
// remains for tests that validate the historical naming convention directly.
// Only valid for personal and business canonical IDs.
func AccountFilePath(cid driveid.CanonicalID) string {
	if cid.IsZero() {
		return ""
	}

	if !cid.IsPersonal() && !cid.IsBusiness() {
		return ""
	}

	dataDir := DefaultDataDir()
	if dataDir == "" {
		return ""
	}

	sanitized := cid.DriveType() + "_" + cid.Email()

	return filepath.Join(dataDir, "account_"+sanitized+".json")
}

// accountCIDForDrive returns the account canonical ID that owns this drive.
// Personal and business drives own themselves. SharePoint drives belong to
// the business account with the same email. Shared drives are resolved
// via drive metadata (not handled here — caller uses LoadDriveMetadata).
func accountCIDForDrive(cid driveid.CanonicalID) driveid.CanonicalID {
	switch {
	case cid.IsPersonal(), cid.IsBusiness():
		return cid
	case cid.IsSharePoint():
		biz, err := driveid.Construct(driveid.DriveTypeBusiness, cid.Email())
		if err != nil {
			return driveid.CanonicalID{}
		}

		return biz
	default:
		return driveid.CanonicalID{}
	}
}

// LookupAccountProfile reads the profile from the managed catalog. The returned
// found flag is false when the catalog has no account entry or the entry lacks
// cached profile fields.
func LookupAccountProfile(cid driveid.CanonicalID) (*AccountProfile, bool, error) {
	if cid.IsZero() || (!cid.IsPersonal() && !cid.IsBusiness()) {
		return nil, false, nil
	}

	catalog, err := LoadCatalog()
	if err != nil {
		return nil, false, fmt.Errorf("loading catalog: %w", err)
	}

	account, found := catalog.AccountByCanonicalID(cid)
	if !found {
		return nil, false, nil
	}

	if account.UserID == "" && account.DisplayName == "" && account.OrgName == "" && account.PrimaryDriveID == "" {
		return nil, false, nil
	}

	return &AccountProfile{
		UserID:         account.UserID,
		DisplayName:    account.DisplayName,
		OrgName:        account.OrgName,
		PrimaryDriveID: account.PrimaryDriveID,
	}, true, nil
}

// SaveAccountProfile writes the profile to the managed catalog.
func SaveAccountProfile(cid driveid.CanonicalID, profile *AccountProfile) error {
	if cid.IsZero() || (!cid.IsPersonal() && !cid.IsBusiness()) {
		return fmt.Errorf("cannot determine account catalog entry for %s", cid)
	}

	return UpdateCatalog(func(catalog *Catalog) error {
		account := CatalogAccount{
			CanonicalID: cid.String(),
			Email:       cid.Email(),
			DriveType:   cid.DriveType(),
		}
		if existing, found := catalog.AccountByCanonicalID(cid); found {
			account = existing
		}

		if profile != nil {
			account.UserID = profile.UserID
			account.DisplayName = profile.DisplayName
			account.OrgName = profile.OrgName
			account.PrimaryDriveID = profile.PrimaryDriveID
		}

		catalog.UpsertAccount(&account)
		return nil
	})
}

// DiscoverAccountProfiles enumerates catalog-backed account identities that
// still have cached account profile data.
func DiscoverAccountProfiles(logger *slog.Logger) []driveid.CanonicalID {
	catalog, err := LoadCatalog()
	if err != nil {
		logger.Debug("cannot load catalog for account discovery", "error", err)
		return nil
	}

	var ids []driveid.CanonicalID
	for _, key := range catalog.SortedAccountKeys() {
		account := catalog.Accounts[key]
		if account.UserID == "" && account.DisplayName == "" && account.OrgName == "" && account.PrimaryDriveID == "" {
			continue
		}

		cid, err := driveid.NewCanonicalID(account.CanonicalID)
		if err != nil {
			logger.Debug("skipping malformed catalog account", "canonical_id", account.CanonicalID, "error", err)
			continue
		}

		ids = append(ids, cid)
	}

	slices.SortFunc(ids, func(a, b driveid.CanonicalID) int {
		return strings.Compare(a.String(), b.String())
	})
	return ids
}

// discoverAccountProfilesIn remains as the filename-scanning helper used by
// unit tests that validate the old managed-file naming convention directly.
func discoverAccountProfilesIn(dir string, logger *slog.Logger) []driveid.CanonicalID {
	return discoverCIDFiles(dir, "account_", logger)
}
