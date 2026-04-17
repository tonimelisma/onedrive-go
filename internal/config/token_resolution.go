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
//	"shared:alice@outlook.com:drv123:item456"      -> "{dataDir}/token_personal_alice@outlook.com.json" (via catalog drive ownership)
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
// from the catalog drive record.
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

// resolveSharedTokenCID reads the catalog drive record for a shared drive to
// find its parent account's canonical ID. Returns a zero CID if the catalog
// entry is missing or lacks an owner account.
func resolveSharedTokenCID(cid driveid.CanonicalID) (driveid.CanonicalID, error) {
	catalog, err := LoadCatalog()
	if err != nil {
		return driveid.CanonicalID{}, fmt.Errorf("loading catalog for shared token resolution %s: %w", cid, err)
	}
	drive, found := catalog.DriveByCanonicalID(cid)
	if !found || drive.OwnerAccountCanonical == "" {
		return driveid.CanonicalID{}, nil
	}

	accountCID, err := driveid.NewCanonicalID(drive.OwnerAccountCanonical)
	if err != nil {
		return driveid.CanonicalID{}, fmt.Errorf("parse shared account canonical ID %q: %w", drive.OwnerAccountCanonical, err)
	}

	return accountCID, nil
}
