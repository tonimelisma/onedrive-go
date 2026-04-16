package config

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DriveMetadata holds cached API data for a specific drive. Persisted in the
// managed catalog. Personal and business drives store drive_id. SharePoint
// adds site_id. Shared drives store the parent account canonical ID and owner
// info instead.
type DriveMetadata struct {
	DriveID            string `json:"drive_id,omitempty"`
	SiteID             string `json:"site_id,omitempty"`
	AccountCanonicalID string `json:"account_canonical_id,omitempty"`
	OwnerName          string `json:"owner_name,omitempty"`
	OwnerEmail         string `json:"owner_email,omitempty"`
	CachedAt           string `json:"cached_at,omitempty"`
}

// DriveMetadataPath returns the legacy path shape for a drive metadata file.
// The managed catalog now owns drive metadata in steady state; this helper
// remains for tests that validate the historical naming convention directly.
func DriveMetadataPath(cid driveid.CanonicalID) string {
	if cid.IsZero() {
		return ""
	}

	dataDir := DefaultDataDir()
	if dataDir == "" {
		return ""
	}

	sanitized := strings.ReplaceAll(cid.String(), ":", "_")

	return filepath.Join(dataDir, "drive_"+sanitized+".json")
}

// LookupDriveMetadata reads a drive's cached metadata from the managed catalog.
func LookupDriveMetadata(cid driveid.CanonicalID) (*DriveMetadata, bool, error) {
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

	return &DriveMetadata{
		DriveID:            drive.RemoteDriveID,
		SiteID:             drive.SiteID,
		AccountCanonicalID: drive.OwnerAccountCanonical,
		OwnerName:          drive.SharedOwnerName,
		OwnerEmail:         drive.SharedOwnerEmail,
		CachedAt:           drive.CachedAt,
	}, true, nil
}

// SaveDriveMetadata writes a drive's cached metadata into the managed catalog.
func SaveDriveMetadata(cid driveid.CanonicalID, meta *DriveMetadata) error {
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

		if meta != nil {
			if meta.DriveID != "" {
				drive.RemoteDriveID = meta.DriveID
			}
			if meta.SiteID != "" {
				drive.SiteID = meta.SiteID
			}
			if meta.AccountCanonicalID != "" {
				drive.OwnerAccountCanonical = meta.AccountCanonicalID
			}
			if meta.OwnerName != "" {
				drive.SharedOwnerName = meta.OwnerName
			}
			if meta.OwnerEmail != "" {
				drive.SharedOwnerEmail = meta.OwnerEmail
			}
			if meta.CachedAt != "" {
				drive.CachedAt = meta.CachedAt
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

// DiscoverDriveMetadataForEmail enumerates catalog-backed drives owned by the
// given email and returns their legacy path shape. These paths are no longer
// authoritative steady-state storage, but the helper remains for tests and
// cleanup paths that still reason in terms of managed drive artifacts.
func DiscoverDriveMetadataForEmail(email string, logger *slog.Logger) []string {
	catalog, err := LoadCatalog()
	if err != nil {
		logger.Debug("cannot load catalog for drive metadata discovery", "error", err)
		return nil
	}

	var paths []string
	for _, key := range catalog.SortedDriveKeys() {
		drive := catalog.Drives[key]
		if drive.OwnerAccountCanonical == "" {
			continue
		}

		accountCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
		if err != nil {
			logger.Debug("skipping malformed catalog drive owner", "canonical_id", drive.CanonicalID, "error", err)
			continue
		}
		if accountCID.Email() != email {
			continue
		}

		driveCID, err := driveid.NewCanonicalID(drive.CanonicalID)
		if err != nil {
			logger.Debug("skipping malformed catalog drive", "canonical_id", drive.CanonicalID, "error", err)
			continue
		}
		paths = append(paths, DriveMetadataPath(driveCID))
	}

	slices.Sort(paths)
	return paths
}

// discoverDriveMetadataForEmailIn remains as the filename-scanning helper used
// by unit tests that validate the old managed-file naming convention directly.
func discoverDriveMetadataForEmailIn(dir, email string, logger *slog.Logger) []string {
	return discoverFilesForEmail(dir, "drive_", ".json", email, logger)
}
