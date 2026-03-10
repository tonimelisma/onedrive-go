package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// AccountProfile holds cached API data about the account owner. Persisted
// alongside the OAuth token in the account file. Updated on every login.
type AccountProfile struct {
	UserID         string `json:"user_id"`
	DisplayName    string `json:"display_name"`
	OrgName        string `json:"org_name,omitempty"`
	PrimaryDriveID string `json:"primary_drive_id"`
}

// accountFile is the on-disk format for account files. Contains just the
// profile — the OAuth token continues to be managed by tokenfile.Save/Load
// in the same file (account files ARE the token files, renamed and relocated).
type accountFile struct {
	Profile *AccountProfile `json:"profile"`
}

// AccountFilePath returns the path for an account's file (token + profile).
// Only valid for personal and business canonical IDs — SharePoint and shared
// drives use their parent account's file.
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

// LoadAccountProfile reads the profile from an account file. Returns
// (nil, nil) if the file does not exist.
func LoadAccountProfile(cid driveid.CanonicalID) (*AccountProfile, error) {
	path := AccountFilePath(cid)
	if path == "" {
		return nil, nil //nolint:nilnil // sentinel for "not applicable"
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // sentinel for "not found"
	}

	if err != nil {
		return nil, fmt.Errorf("reading account file: %w", err)
	}

	var af accountFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("decoding account file: %w", err)
	}

	return af.Profile, nil
}

// SaveAccountProfile writes the profile to an account file. Creates
// parent directories as needed. Atomic write (temp file + rename).
func SaveAccountProfile(cid driveid.CanonicalID, profile *AccountProfile) error {
	path := AccountFilePath(cid)
	if path == "" {
		return fmt.Errorf("cannot determine account file path for %s", cid)
	}

	af := accountFile{Profile: profile}

	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding account profile: %w", err)
	}

	dir := filepath.Dir(path)

	if mkdirErr := os.MkdirAll(dir, configDirPermissions); mkdirErr != nil {
		return fmt.Errorf("creating data directory: %w", mkdirErr)
	}

	// Atomic write: temp file in same dir, then rename.
	tmp, err := os.CreateTemp(dir, ".account-*.tmp")
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

		return fmt.Errorf("writing account file: %w", err)
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()

		return fmt.Errorf("syncing account file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing account file: %w", err)
	}

	if err := os.Chmod(tmpPath, configFilePermissions); err != nil {
		return fmt.Errorf("setting account file permissions: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming account file: %w", err)
	}

	succeeded = true

	return nil
}

// DiscoverAccountProfiles scans the data directory for account profile files
// and returns the canonical IDs they represent. Account files follow the naming
// convention: account_{type}_{email}.json. This is the profile-file counterpart
// of DiscoverTokens — it finds accounts that may no longer have tokens (logged
// out but not purged).
func DiscoverAccountProfiles(logger *slog.Logger) []driveid.CanonicalID {
	return discoverAccountProfilesIn(DefaultDataDir(), logger)
}

// discoverAccountProfilesIn scans dir for account profile files and extracts
// canonical IDs. Files that don't match the naming convention are silently skipped.
func discoverAccountProfilesIn(dir string, logger *slog.Logger) []driveid.CanonicalID {
	return discoverCIDFiles(dir, "account_", logger)
}
