package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DriveMetadata holds cached API data for a specific drive. Persisted in a
// per-drive JSON file under drives/. Personal and business drives store
// drive_id. SharePoint adds site_id. Shared drives store the parent account
// canonical ID and owner info instead.
type DriveMetadata struct {
	DriveID            string `json:"drive_id,omitempty"`
	SiteID             string `json:"site_id,omitempty"`
	AccountCanonicalID string `json:"account_canonical_id,omitempty"`
	OwnerName          string `json:"owner_name,omitempty"`
	OwnerEmail         string `json:"owner_email,omitempty"`
	CachedAt           string `json:"cached_at,omitempty"`
}

// drivesDirName is the subdirectory under DefaultDataDir() for drive metadata files.
const drivesDirName = "drives"

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

	return filepath.Join(dataDir, drivesDirName, sanitized+".json")
}

// LoadDriveMetadata reads a drive's cached metadata. Returns (nil, nil) if
// the file does not exist.
func LoadDriveMetadata(cid driveid.CanonicalID) (*DriveMetadata, error) {
	path := DriveMetadataPath(cid)
	if path == "" {
		return nil, nil //nolint:nilnil // sentinel for "not applicable"
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // sentinel for "not found"
	}

	if err != nil {
		return nil, fmt.Errorf("reading drive metadata: %w", err)
	}

	var meta DriveMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decoding drive metadata: %w", err)
	}

	return &meta, nil
}

// SaveDriveMetadata writes a drive's cached metadata. Creates the drives/
// directory and parent directories as needed. Atomic write (temp + rename).
// Idempotent: overwrites existing metadata.
func SaveDriveMetadata(cid driveid.CanonicalID, meta *DriveMetadata) error {
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
		return fmt.Errorf("creating drives directory: %w", mkdirErr)
	}

	tmp, err := os.CreateTemp(dir, ".drivemeta-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	tmpPath := tmp.Name()

	succeeded := false
	defer func() {
		if !succeeded {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()

		return fmt.Errorf("writing drive metadata: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()

		return fmt.Errorf("syncing drive metadata: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing drive metadata: %w", err)
	}

	if err := os.Chmod(tmpPath, configFilePermissions); err != nil {
		return fmt.Errorf("setting drive metadata permissions: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming drive metadata: %w", err)
	}

	succeeded = true

	return nil
}
