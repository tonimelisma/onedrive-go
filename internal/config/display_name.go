package config

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// followedDisplayName is the name a drive goes by after its ID is renamed
// from from to to: a stored name that is exactly the default derived from the
// old ID becomes the default of the new one, and any other name, one Graph or
// the user supplied, stays. The comparison is exact, so a name that merely
// contains the old address is left alone. The account email change applies
// it to catalog records and to config sections alike.
func followedDisplayName(name string, from, to driveid.CanonicalID) string {
	if name != "" && name == DefaultDisplayName(from) {
		return DefaultDisplayName(to)
	}

	return name
}

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
		// Best effort when no display_name is stored in config. The real
		// display name should be set at drive-add time via the API response.
		return "Shared from " + cid.Email()

	default:
		return cid.Email()
	}
}
