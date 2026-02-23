package driveid

import (
	"fmt"
	"strings"
)

// canonicalSplitLimit is the maximum split limit for parsing canonical IDs.
// Using SplitN with limit 3 splits "type:email:rest" into at most 3 parts,
// ensuring the email (parts[1]) is extracted cleanly even for SharePoint IDs
// like "sharepoint:alice@contoso.com:marketing:Docs".
const canonicalSplitLimit = 3

// ValidDriveTypes enumerates accepted drive type prefixes in canonical IDs.
var ValidDriveTypes = map[string]bool{
	"personal":   true,
	"business":   true,
	"sharepoint": true,
}

// CanonicalID is a config-level drive identifier with the format
// "type:email" (e.g., "personal:user@example.com") or
// "type:email:site:library" for SharePoint.
// The zero value (CanonicalID{}) represents an absent canonical ID.
type CanonicalID struct {
	value string
}

// NewCanonicalID parses and validates a raw canonical ID string. Returns
// an error if the format is invalid (no colon separator, unknown type prefix,
// or empty email).
func NewCanonicalID(raw string) (CanonicalID, error) {
	parts := strings.SplitN(raw, ":", canonicalSplitLimit)
	if len(parts) < 2 || parts[1] == "" {
		return CanonicalID{}, fmt.Errorf("driveid: canonical ID %q must be \"type:email\" format", raw)
	}

	driveType := parts[0]
	if !ValidDriveTypes[driveType] {
		return CanonicalID{}, fmt.Errorf(
			"driveid: canonical ID %q has unknown type %q (must be personal, business, or sharepoint)", raw, driveType)
	}

	return CanonicalID{value: raw}, nil
}

// MustCanonicalID is like NewCanonicalID but panics on invalid input.
// Use in tests and initialization code where the value is known-good.
func MustCanonicalID(raw string) CanonicalID {
	cid, err := NewCanonicalID(raw)
	if err != nil {
		panic(err)
	}

	return cid
}

// Construct builds a canonical ID from separate drive type and email parts.
// Returns an error if the resulting ID is invalid.
func Construct(driveType, email string) (CanonicalID, error) {
	return NewCanonicalID(driveType + ":" + email)
}

// String returns the canonical ID string.
func (c CanonicalID) String() string {
	return c.value
}

// IsZero reports whether this is the zero-value CanonicalID.
func (c CanonicalID) IsZero() bool {
	return c.value == ""
}

// DriveType returns the type prefix (e.g., "personal", "business", "sharepoint").
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) DriveType() string {
	if c.value == "" {
		return ""
	}

	parts := strings.SplitN(c.value, ":", canonicalSplitLimit)

	return parts[0]
}

// Email returns the email portion of the canonical ID.
// For "personal:user@example.com" returns "user@example.com".
// For "sharepoint:alice@contoso.com:marketing:Docs" returns "alice@contoso.com".
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) Email() string {
	if c.value == "" {
		return ""
	}

	parts := strings.SplitN(c.value, ":", canonicalSplitLimit)
	if len(parts) < 2 {
		return ""
	}

	return parts[1]
}

// IsSharePoint reports whether this is a SharePoint drive.
func (c CanonicalID) IsSharePoint() bool {
	return c.DriveType() == "sharepoint"
}

// TokenCanonicalID returns the canonical ID to use for token path derivation.
// SharePoint drives share the business OAuth token, so "sharepoint:email:..."
// returns "business:email". All other types return self.
func (c CanonicalID) TokenCanonicalID() CanonicalID {
	if !c.IsSharePoint() {
		return c
	}

	// SharePoint drives share the business account's token.
	return CanonicalID{value: "business:" + c.Email()}
}
