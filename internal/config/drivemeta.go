package config

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DriveIdentity holds cached API data for a specific drive. Persisted in the
// managed catalog. Personal and business drives store drive_id. SharePoint
// adds site_id. Shared drives store the parent account canonical ID and owner
// info instead.
type DriveIdentity struct {
	DriveID            string `json:"drive_id,omitempty"`
	SiteID             string `json:"site_id,omitempty"`
	AccountCanonicalID string `json:"account_canonical_id,omitempty"`
	OwnerName          string `json:"owner_name,omitempty"`
	OwnerEmail         string `json:"owner_email,omitempty"`
	CachedAt           string `json:"cached_at,omitempty"`
}

// LookupDriveIdentity reads a drive's cached identity from the managed catalog.
func LookupDriveIdentity(cid driveid.CanonicalID) (*DriveIdentity, bool, error) {
	if cid.IsZero() {
		return nil, false, nil
	}

	catalog, err := LoadCatalog()
	if err != nil {
		return nil, false, fmt.Errorf("loading catalog: %w", err)
	}

	drive, found := catalog.DriveByCanonicalID(cid)
	if !found {
		return nil, false, nil
	}

	if drive.RemoteDriveID == "" &&
		drive.SiteID == "" &&
		drive.OwnerAccountCanonical == "" &&
		drive.SharedOwnerName == "" &&
		drive.SharedOwnerEmail == "" {
		return nil, false, nil
	}

	return &DriveIdentity{
		DriveID:            drive.RemoteDriveID,
		SiteID:             drive.SiteID,
		AccountCanonicalID: drive.OwnerAccountCanonical,
		OwnerName:          drive.SharedOwnerName,
		OwnerEmail:         drive.SharedOwnerEmail,
		CachedAt:           drive.CachedAt,
	}, true, nil
}

// SaveDriveIdentity writes a drive's cached identity into the managed catalog.
func SaveDriveIdentity(cid driveid.CanonicalID, identity *DriveIdentity) error {
	if cid.IsZero() {
		return fmt.Errorf("cannot determine drive catalog entry for %s", cid)
	}

	return UpdateCatalog(func(catalog *Catalog) error {
		drive := CatalogDrive{
			CanonicalID: cid.String(),
			DriveType:   cid.DriveType(),
		}
		if existing, found := catalog.DriveByCanonicalID(cid); found {
			drive = existing
		}

		if identity != nil {
			if identity.DriveID != "" {
				drive.RemoteDriveID = identity.DriveID
			}
			if identity.SiteID != "" {
				drive.SiteID = identity.SiteID
			}
			if identity.AccountCanonicalID != "" {
				drive.OwnerAccountCanonical = identity.AccountCanonicalID
			}
			if identity.OwnerName != "" {
				drive.SharedOwnerName = identity.OwnerName
			}
			if identity.OwnerEmail != "" {
				drive.SharedOwnerEmail = identity.OwnerEmail
			}
			if identity.CachedAt != "" {
				drive.CachedAt = identity.CachedAt
			}
		}
		if drive.OwnerAccountCanonical == "" {
			switch {
			case cid.IsPersonal(), cid.IsBusiness():
				drive.OwnerAccountCanonical = cid.String()
			case cid.IsSharePoint():
				if accountCID, err := driveid.Construct(driveid.DriveTypeBusiness, cid.Email()); err == nil {
					drive.OwnerAccountCanonical = accountCID.String()
				}
			}
		}

		catalog.UpsertDrive(&drive)
		return nil
	})
}
