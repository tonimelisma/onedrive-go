package config

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// TokenCanonicalID resolves which OAuth token a drive uses. Personal and
// business drives use their own token. SharePoint drives share the business
// account's token (same OAuth session). Shared drives piggyback on their
// primary account's token (personal or business), determined by scanning
// configured drives.
//
// The cfg parameter is only required for shared drives. Pass nil for
// personal, business, and SharePoint drives.
func TokenCanonicalID(cid driveid.CanonicalID, cfg *Config) (driveid.CanonicalID, error) {
	switch cid.DriveType() {
	case driveid.DriveTypePersonal, driveid.DriveTypeBusiness:
		return cid, nil

	case driveid.DriveTypeSharePoint:
		return driveid.Construct(driveid.DriveTypeBusiness, cid.Email())

	case driveid.DriveTypeShared:
		return resolveSharedToken(cid, cfg)

	default:
		return driveid.CanonicalID{}, fmt.Errorf("config: unknown drive type %q", cid.DriveType())
	}
}

// resolveSharedToken finds the primary drive (personal or business) for the
// shared drive's email and returns that drive's canonical ID for token lookup.
func resolveSharedToken(cid driveid.CanonicalID, cfg *Config) (driveid.CanonicalID, error) {
	if cfg == nil {
		return driveid.CanonicalID{}, fmt.Errorf(
			"config: config required to resolve token for shared drive %s", cid.Email())
	}

	for id := range cfg.Drives {
		if id.Email() != cid.Email() {
			continue
		}

		if id.IsPersonal() || id.IsBusiness() {
			return driveid.Construct(id.DriveType(), cid.Email())
		}
	}

	return driveid.CanonicalID{}, fmt.Errorf(
		"config: no personal or business account found for %s to resolve shared drive token", cid.Email())
}
