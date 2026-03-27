package config

import (
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DriveTokenPath returns the token file path for a canonical drive ID.
// Personal and business drives have their own token. SharePoint drives
// share the business account's token (same OAuth session). Shared drives
// piggyback on their parent account's token, resolved via drive metadata
// files (no config dependency).
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

	tokenCID := tokenAccountCID(canonicalID)
	if tokenCID.IsZero() {
		return ""
	}

	sanitized := tokenCID.DriveType() + "_" + tokenCID.Email()

	return filepath.Join(dataDir, "token_"+sanitized+".json")
}

// tokenAccountCID resolves which account owns the OAuth token for a drive.
// Personal and business drives own their token. SharePoint drives use the
// business account's token. Shared drives use their parent account's
// token, determined from the drive metadata file.
func tokenAccountCID(cid driveid.CanonicalID) driveid.CanonicalID {
	if cid.IsShared() {
		return resolveSharedTokenCID(cid)
	}

	return accountCIDForDrive(cid)
}

// resolveSharedTokenCID reads the drive metadata file for a shared drive
// to find its parent account's canonical ID. Returns a zero CID if the
// drive metadata is missing or lacks an account_canonical_id.
func resolveSharedTokenCID(cid driveid.CanonicalID) driveid.CanonicalID {
	meta, found, err := LookupDriveMetadata(cid)
	if err != nil || !found || meta.AccountCanonicalID == "" {
		return driveid.CanonicalID{}
	}

	accountCID, err := driveid.NewCanonicalID(meta.AccountCanonicalID)
	if err != nil {
		return driveid.CanonicalID{}
	}

	return accountCID
}
