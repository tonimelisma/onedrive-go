package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
)

// DriveMetadata holds cached API data for a specific drive. Persisted as a
// per-drive JSON file (drive_*.json) in the data directory. Personal and
// business drives store drive_id. SharePoint adds site_id. Shared drives
// store the parent account canonical ID and owner info instead.
type DriveMetadata struct {
	DriveID            string `json:"drive_id,omitempty"`
	SiteID             string `json:"site_id,omitempty"`
	AccountCanonicalID string `json:"account_canonical_id,omitempty"`
	OwnerName          string `json:"owner_name,omitempty"`
	OwnerEmail         string `json:"owner_email,omitempty"`
	CachedAt           string `json:"cached_at,omitempty"`
}

// DriveMetadataPath returns the path for a drive's metadata file.
// Uses the canonical ID with ":" replaced by "_" for filesystem safety.
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

// LookupDriveMetadata reads a drive's cached metadata. The returned found flag
// is false when the metadata file is not applicable or missing.
func LookupDriveMetadata(cid driveid.CanonicalID) (*DriveMetadata, bool, error) {
	path := DriveMetadataPath(cid)
	if path == "" {
		return nil, false, nil
	}

	data, err := trustedpath.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("reading drive metadata: %w", err)
	}

	var meta DriveMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, false, fmt.Errorf("decoding drive metadata: %w", err)
	}

	return &meta, true, nil
}

// SaveDriveMetadata writes a drive's cached metadata. Creates parent
// directories as needed. Atomic write (temp + rename).
// Idempotent: overwrites existing metadata.
func SaveDriveMetadata(cid driveid.CanonicalID, meta *DriveMetadata) (err error) {
	path := DriveMetadataPath(cid)
	if path == "" {
		return fmt.Errorf("cannot determine drive metadata path for %s", cid)
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding drive metadata: %w", err)
	}

	dir := filepath.Dir(path)

	if mkdirErr := os.MkdirAll(dir, configDirPermissions); mkdirErr != nil {
		return fmt.Errorf("creating data directory: %w", mkdirErr)
	}

	tmp, err := os.CreateTemp(dir, ".drivemeta-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	succeeded := false
	defer func() {
		if !succeeded {
			err = removeTempPath(tmpPath, "drive metadata temp file", err)
		}
	}()

	if _, writeErr := tmp.Write(data); writeErr != nil {
		return closeTempFile(tmp, "drive metadata temp file", fmt.Errorf("writing drive metadata: %w", writeErr))
	}

	if syncErr := tmp.Sync(); syncErr != nil {
		return closeTempFile(tmp, "drive metadata temp file", fmt.Errorf("syncing drive metadata: %w", syncErr))
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing drive metadata: %w", err)
	}

	if err := chmodTrustedTempPath(tmpPath, configFilePermissions, "drive metadata"); err != nil {
		return err
	}

	if err := renameTrustedTempPath(tmpPath, path, "drive metadata"); err != nil {
		return err
	}

	succeeded = true

	return nil
}

// DiscoverDriveMetadataForEmail scans the data directory for drive metadata
// files belonging to the given email address. Returns full file paths.
// Uses the same underscore-boundary matching as DiscoverStateDBsForEmail.
func DiscoverDriveMetadataForEmail(email string, logger *slog.Logger) []string {
	return discoverDriveMetadataForEmailIn(DefaultDataDir(), email, logger)
}

// discoverDriveMetadataForEmailIn scans dir for drive metadata files belonging to email.
func discoverDriveMetadataForEmailIn(dir, email string, logger *slog.Logger) []string {
	return discoverFilesForEmail(dir, "drive_", ".json", email, logger)
}
