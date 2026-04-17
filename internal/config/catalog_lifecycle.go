package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// RecordLogin persists the durable account and primary-drive inventory facts
// discovered during login. CLI owns the Graph flow; config owns how those
// durable facts are represented in the catalog.
func RecordLogin(
	dataDir string,
	accountCID driveid.CanonicalID,
	userID string,
	displayName string,
	orgName string,
	primaryDriveID driveid.ID,
) error {
	if accountCID.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		account := CatalogAccount{
			CanonicalID:           accountCID.String(),
			Email:                 accountCID.Email(),
			DriveType:             accountCID.DriveType(),
			UserID:                userID,
			DisplayName:           displayName,
			OrgName:               orgName,
			PrimaryDriveID:        primaryDriveID.String(),
			PrimaryDriveCanonical: accountCID.String(),
		}
		if existing, found := catalog.AccountByCanonicalID(accountCID); found {
			account.AuthRequirementReason = existing.AuthRequirementReason
		}

		drive := CatalogDrive{
			CanonicalID:           accountCID.String(),
			OwnerAccountCanonical: accountCID.String(),
			DriveType:             accountCID.DriveType(),
			DisplayName:           DefaultDisplayName(accountCID),
			PrimaryForAccount:     true,
			RemoteDriveID:         primaryDriveID.String(),
			CachedAt:              time.Now().UTC().Format(time.RFC3339),
		}
		if existing, found := catalog.DriveByCanonicalID(accountCID); found {
			drive = existing
			drive.OwnerAccountCanonical = accountCID.String()
			drive.DriveType = accountCID.DriveType()
			drive.PrimaryForAccount = true
			drive.RemoteDriveID = primaryDriveID.String()
			drive.CachedAt = time.Now().UTC().Format(time.RFC3339)
			if drive.DisplayName == "" {
				drive.DisplayName = DefaultDisplayName(accountCID)
			}
		}

		catalog.UpsertAccount(&account)
		catalog.UpsertDrive(&drive)
		return nil
	})
}

// RegisterDrive persists a direct drive catalog record after config has
// accepted the drive section. Ownership and display defaults remain config
// concerns so CLI does not hand-edit CatalogDrive records.
func RegisterDrive(dataDir string, cid driveid.CanonicalID, displayName string) error {
	if cid.IsZero() {
		return nil
	}

	ownerCID := accountCIDForDrive(cid)
	if ownerCID.IsZero() {
		return fmt.Errorf("resolving drive owner for %s", cid)
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		drive := CatalogDrive{
			CanonicalID:           cid.String(),
			OwnerAccountCanonical: ownerCID.String(),
			DriveType:             cid.DriveType(),
			DisplayName:           defaultCatalogDriveDisplayName(cid, displayName),
		}
		if existing, found := catalog.DriveByCanonicalID(cid); found {
			drive = existing
			drive.OwnerAccountCanonical = ownerCID.String()
			drive.DriveType = cid.DriveType()
			if drive.DisplayName == "" {
				drive.DisplayName = defaultCatalogDriveDisplayName(cid, displayName)
			}
		}

		catalog.UpsertDrive(&drive)
		return nil
	})
}

// RegisterSharedDrive persists shared-drive ownership after CLI resolves the
// authoritative parent account through Graph/bootstrap flow.
func RegisterSharedDrive(dataDir string, cid, ownerCID driveid.CanonicalID, displayName string) error {
	if cid.IsZero() || ownerCID.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		drive := CatalogDrive{
			CanonicalID:           cid.String(),
			OwnerAccountCanonical: ownerCID.String(),
			DriveType:             cid.DriveType(),
			DisplayName:           defaultCatalogDriveDisplayName(cid, displayName),
		}
		if existing, found := catalog.DriveByCanonicalID(cid); found {
			drive = existing
			drive.OwnerAccountCanonical = ownerCID.String()
			drive.DriveType = cid.DriveType()
			if displayName != "" {
				drive.DisplayName = displayName
			} else if drive.DisplayName == "" {
				drive.DisplayName = DefaultDisplayName(cid)
			}
		}

		catalog.UpsertDrive(&drive)
		return nil
	})
}

// ApplyPlainLogout clears any persisted account-auth requirement while keeping
// retained inventory knowledge intact for future re-login.
func ApplyPlainLogout(dataDir, email string) error {
	if email == "" {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		account, found := catalog.AccountByEmail(email)
		if !found {
			return nil
		}
		account.AuthRequirementReason = authstate.Reason("")
		catalog.UpsertAccount(&account)
		return nil
	})
}

// ApplyPurgeLogout removes the catalog account and every owned drive record.
func ApplyPurgeLogout(dataDir, email string) error {
	if email == "" {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		accountCID := AccountCanonicalIDByEmail(catalog, email)
		if accountCID.IsZero() {
			return nil
		}

		drives := catalog.DrivesForAccount(accountCID)
		for i := range drives {
			driveCID, err := driveid.NewCanonicalID(drives[i].CanonicalID)
			if err != nil {
				continue
			}
			catalog.DeleteDrive(driveCID)
		}
		catalog.DeleteAccount(accountCID)
		return nil
	})
}

// PruneDriveAfterPurge removes non-primary retained drive records once local
// purge cleanup has completed, while preserving primary drives as durable
// inventory anchors.
func PruneDriveAfterPurge(dataDir string, cid driveid.CanonicalID) error {
	if cid.IsZero() {
		return nil
	}

	return UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		drive, found := catalog.DriveByCanonicalID(cid)
		if !found {
			return nil
		}

		drive.RetainedStatePresent = false
		accountCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
		if drive.PrimaryForAccount || (err == nil && accountOwnsPrimaryDrive(catalog, accountCID, cid)) {
			catalog.UpsertDrive(&drive)
			return nil
		}

		catalog.DeleteDrive(cid)
		return nil
	})
}

// AccountCanonicalIDByEmail resolves the durable account identity stored in
// the catalog for an email address.
func AccountCanonicalIDByEmail(catalog *Catalog, email string) driveid.CanonicalID {
	if catalog == nil || email == "" {
		return driveid.CanonicalID{}
	}

	account, found := catalog.AccountByEmail(email)
	if !found {
		return driveid.CanonicalID{}
	}

	cid, err := driveid.NewCanonicalID(account.CanonicalID)
	if err != nil {
		return driveid.CanonicalID{}
	}

	return cid
}

func LoadAccountCanonicalIDByEmail(dataDir, email string) (driveid.CanonicalID, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return driveid.CanonicalID{}, err
	}

	return AccountCanonicalIDByEmail(catalog, email), nil
}

// HasPersistedAccountAuthRequirement reports whether the catalog currently
// marks the account as requiring re-authentication.
func HasPersistedAccountAuthRequirement(catalog *Catalog, email string) bool {
	if catalog == nil || email == "" {
		return false
	}

	account, found := catalog.AccountByEmail(email)
	if !found {
		return false
	}

	return account.AuthRequirementReason != ""
}

func LoadHasPersistedAccountAuthRequirement(dataDir, email string) (bool, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return false, err
	}

	return HasPersistedAccountAuthRequirement(catalog, email), nil
}

// AuthenticatedAccountEmails returns known catalog accounts that still have a
// usable token file path on disk. It preserves a stable sort for CLI account
// selection prompts.
func AuthenticatedAccountEmails(dataDir string) ([]string, error) {
	catalog, err := LoadCatalogForDataDir(dataDir)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{})
	var emails []string
	for _, key := range catalog.SortedAccountKeys() {
		account := catalog.Accounts[key]
		if account.Email == "" {
			continue
		}
		if _, found := seen[account.Email]; found {
			continue
		}

		accountCID := AccountCanonicalIDByEmail(catalog, account.Email)
		if accountCID.IsZero() {
			continue
		}

		tokenPath := DriveTokenPath(accountCID)
		if tokenPath == "" || !catalogTokenPathExists(tokenPath) {
			continue
		}

		seen[account.Email] = struct{}{}
		emails = append(emails, account.Email)
	}

	slices.Sort(emails)
	return emails, nil
}

func accountOwnsPrimaryDrive(catalog *Catalog, accountCID, driveCID driveid.CanonicalID) bool {
	if catalog == nil || accountCID.IsZero() || driveCID.IsZero() {
		return false
	}

	account, found := catalog.AccountByCanonicalID(accountCID)
	if !found {
		return false
	}

	return account.PrimaryDriveCanonical == driveCID.String()
}

func defaultCatalogDriveDisplayName(cid driveid.CanonicalID, displayName string) string {
	if displayName != "" {
		return displayName
	}

	return DefaultDisplayName(cid)
}

func catalogTokenPathExists(path string) bool {
	if path == "" {
		return false
	}

	_, err := readManagedFile(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
