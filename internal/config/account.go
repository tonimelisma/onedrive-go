package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/fsroot"
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

// LookupAccountProfile reads the profile from an account file. The returned
// found flag is false when the account file is not applicable, missing, or
// lacks a profile block.
func LookupAccountProfile(cid driveid.CanonicalID) (*AccountProfile, bool, error) {
	path := AccountFilePath(cid)
	if path == "" {
		return nil, false, nil
	}

	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return nil, false, fmt.Errorf("opening account root: %w", err)
	}

	data, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("reading account file: %w", err)
	}

	var af accountFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, false, fmt.Errorf("decoding account file: %w", err)
	}

	if af.Profile == nil {
		return nil, false, nil
	}

	return af.Profile, true, nil
}

// SaveAccountProfile writes the profile to an account file. Creates
// parent directories as needed. Atomic write (temp file + rename).
func SaveAccountProfile(cid driveid.CanonicalID, profile *AccountProfile) (err error) {
	path := AccountFilePath(cid)
	if path == "" {
		return fmt.Errorf("cannot determine account file path for %s", cid)
	}

	af := accountFile{Profile: profile}

	data, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding account profile: %w", err)
	}

	root, name, err := fsroot.OpenPath(path)
	if err != nil {
		return fmt.Errorf("opening account root: %w", err)
	}

	if err := root.AtomicWrite(name, data, configFilePermissions, configDirPermissions, ".account-*.tmp"); err != nil {
		return fmt.Errorf("writing account file: %w", err)
	}

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
