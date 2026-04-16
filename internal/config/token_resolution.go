package config

import (
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DriveTokenPath returns the token file path for a canonical drive ID.
// Personal and business drives have their own token. SharePoint drives
// share the business account's token (same OAuth session). Shared drives
// piggyback on their parent account's token, resolved via catalog-backed
// drive ownership (no config dependency).
//
// Examples:
//
//	"personal:toni@outlook.com"                    -> "{dataDir}/token_personal_toni@outlook.com.json"
//	"sharepoint:alice@contoso.com:marketing:Docs"  -> "{dataDir}/token_business_alice@contoso.com.json"
//	"shared:alice@outlook.com:drv123:item456"      -> "{dataDir}/token_personal_alice@outlook.com.json" (via drive metadata)
func DriveTokenPath(canonicalID driveid.CanonicalID) string {
	dataDir := DefaultDataDir()
	if dataDir == "" || canonicalID.IsZero() {
		return ""
	}

	tokenCID, err := TokenAccountCanonicalID(canonicalID)
	if err != nil || tokenCID.IsZero() {
		return ""
	}

	sanitized := tokenCID.DriveType() + "_" + tokenCID.Email()

	return filepath.Join(dataDir, "token_"+sanitized+".json")
}

// TokenAccountCanonicalID resolves which account owns the OAuth token for a
// drive. Personal and business drives own their token. SharePoint drives use
// the business account's token. Shared drives use their parent account's token
// from catalog-backed drive metadata.
func TokenAccountCanonicalID(cid driveid.CanonicalID) (driveid.CanonicalID, error) {
	if cid.IsZero() {
		return driveid.CanonicalID{}, nil
	}

	if cid.IsShared() {
		return resolveSharedTokenCID(cid)
	}

	return accountCIDForDrive(cid), nil
}

// tokenAccountCID resolves which account owns the OAuth token for a drive.
// Personal and business drives own their token. SharePoint drives use the
// business account's token. Shared drives use their parent account's
// token, determined from the managed catalog.
func tokenAccountCID(cid driveid.CanonicalID) driveid.CanonicalID {
	tokenCID, err := TokenAccountCanonicalID(cid)
	if err != nil {
		return driveid.CanonicalID{}
	}

	return tokenCID
}

// resolveSharedTokenCID reads the catalog-backed drive metadata for a shared
// drive to find its parent account's canonical ID. Returns a zero CID if the
// catalog entry is missing or lacks an account_canonical_id.
func resolveSharedTokenCID(cid driveid.CanonicalID) (driveid.CanonicalID, error) {
	meta, found, err := LookupDriveMetadata(cid)
	if err != nil {
		return driveid.CanonicalID{}, fmt.Errorf("lookup shared drive metadata for %s: %w", cid, err)
	}
	if !found || meta.AccountCanonicalID == "" {
		return driveid.CanonicalID{}, nil
	}

	accountCID, err := driveid.NewCanonicalID(meta.AccountCanonicalID)
	if err != nil {
		return driveid.CanonicalID{}, fmt.Errorf("parse shared account canonical ID %q: %w", meta.AccountCanonicalID, err)
	}

	return accountCID, nil
}
