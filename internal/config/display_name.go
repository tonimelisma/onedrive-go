package config

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// DefaultDisplayName computes a human-readable display name for a drive when
// the user has not configured an explicit display_name. The format varies by
// drive type:
//
//   - personal/business: email address (e.g., "me@outlook.com")
//   - sharepoint with site+library: "site / library" (e.g., "marketing / Docs")
//   - sharepoint without site+library: email address (fallback)
//   - shared: placeholder with source drive ID (CLI overrides with API data)
func DefaultDisplayName(cid driveid.CanonicalID) string {
	switch {
	case cid.IsSharePoint() && cid.Site() != "" && cid.Library() != "":
		return cid.Site() + " / " + cid.Library()

	case cid.IsShared():
		return "Shared (" + cid.SourceDriveID() + ")"

	default:
		return cid.Email()
	}
}
