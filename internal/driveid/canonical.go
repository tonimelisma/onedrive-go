package driveid

import (
	"encoding"
	"fmt"
	"strings"
)

// Drive type constants used in canonical IDs.
const (
	DriveTypePersonal   = "personal"
	DriveTypeBusiness   = "business"
	DriveTypeSharePoint = "sharepoint"
)

// canonicalIDMaxParts is the maximum number of colon-separated parts in a
// canonical ID (type:email:site:library).
const canonicalIDMaxParts = 4

// validDriveTypes enumerates accepted drive type prefixes in canonical IDs.
var validDriveTypes = map[string]bool{
	DriveTypePersonal:   true,
	DriveTypeBusiness:   true,
	DriveTypeSharePoint: true,
}

// IsValidDriveType reports whether the given string is a valid drive type prefix.
func IsValidDriveType(t string) bool {
	return validDriveTypes[t]
}

// CanonicalID is a config-level drive identifier with the format
// "type:email" (e.g., "personal:user@example.com") or
// "type:email:site:library" for SharePoint.
// The zero value (CanonicalID{}) represents an absent canonical ID.
//
// Fields are parsed once at construction time — accessors return stored
// values without re-splitting.
type CanonicalID struct {
	driveType string // "personal", "business", "sharepoint"
	email     string // "user@example.com"
	site      string // SharePoint only
	library   string // SharePoint only
}

// NewCanonicalID parses and validates a raw canonical ID string. Returns
// an error if the format is invalid (no colon separator, unknown type prefix,
// or empty email).
func NewCanonicalID(raw string) (CanonicalID, error) {
	parts := strings.SplitN(raw, ":", canonicalIDMaxParts)
	if len(parts) < 2 || parts[1] == "" {
		return CanonicalID{}, fmt.Errorf("driveid: canonical ID %q must be \"type:email\" format", raw)
	}

	driveType := parts[0]
	if !validDriveTypes[driveType] {
		return CanonicalID{}, fmt.Errorf(
			"driveid: canonical ID %q has unknown type %q (must be personal, business, or sharepoint)", raw, driveType)
	}

	cid := CanonicalID{
		driveType: driveType,
		email:     parts[1],
	}

	if len(parts) >= 3 {
		cid.site = parts[2]
	}

	if len(parts) >= canonicalIDMaxParts {
		cid.library = parts[3]
	}

	return cid, nil
}

// MustCanonicalID is like NewCanonicalID but panics on invalid input.
// It panics if the format is invalid (no colon, unknown type, or empty email).
// Use only in tests and initialization code where the value is known-good.
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

// ConstructSharePoint builds a SharePoint canonical ID from separate parts.
// Returns an error if any required field is empty or the type is invalid.
func ConstructSharePoint(email, site, library string) (CanonicalID, error) {
	if email == "" {
		return CanonicalID{}, fmt.Errorf("driveid: SharePoint canonical ID requires non-empty email")
	}

	if site == "" || library == "" {
		return CanonicalID{}, fmt.Errorf("driveid: SharePoint canonical ID requires non-empty site and library")
	}

	return CanonicalID{
		driveType: DriveTypeSharePoint,
		email:     email,
		site:      site,
		library:   library,
	}, nil
}

// String returns the canonical ID string.
func (c CanonicalID) String() string {
	if c.driveType == "" {
		return ""
	}

	s := c.driveType + ":" + c.email

	if c.site != "" {
		s += ":" + c.site
	}

	if c.library != "" {
		s += ":" + c.library
	}

	return s
}

// IsZero reports whether this is the zero-value CanonicalID.
func (c CanonicalID) IsZero() bool {
	return c.driveType == ""
}

// DriveType returns the type prefix (e.g., "personal", "business", "sharepoint").
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) DriveType() string {
	return c.driveType
}

// Email returns the email portion of the canonical ID.
// For "personal:user@example.com" returns "user@example.com".
// For "sharepoint:alice@contoso.com:marketing:Docs" returns "alice@contoso.com".
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) Email() string {
	return c.email
}

// IsSharePoint reports whether this is a SharePoint drive.
func (c CanonicalID) IsSharePoint() bool {
	return c.driveType == DriveTypeSharePoint
}

// Site returns the SharePoint site name from the canonical ID.
// For "sharepoint:alice@contoso.com:marketing:Docs" returns "marketing".
// Returns empty string for non-SharePoint drives or zero-value CanonicalID.
func (c CanonicalID) Site() string {
	if !c.IsSharePoint() {
		return ""
	}

	return c.site
}

// Library returns the SharePoint document library name from the canonical ID.
// For "sharepoint:alice@contoso.com:marketing:Docs" returns "Docs".
// Returns empty string for non-SharePoint drives or zero-value CanonicalID.
func (c CanonicalID) Library() string {
	if !c.IsSharePoint() {
		return ""
	}

	return c.library
}

// MarshalText implements encoding.TextMarshaler.
func (c CanonicalID) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. The input is validated
// just like NewCanonicalID().
func (c *CanonicalID) UnmarshalText(text []byte) error {
	cid, err := NewCanonicalID(string(text))
	if err != nil {
		return err
	}

	*c = cid

	return nil
}

// TokenCanonicalID returns the canonical ID to use for token path derivation.
// SharePoint drives share the business OAuth token, so "sharepoint:email:..."
// returns "business:email". All other types return self.
func (c CanonicalID) TokenCanonicalID() CanonicalID {
	if !c.IsSharePoint() {
		return c
	}

	// SharePoint drives share the business account's token.
	return CanonicalID{driveType: DriveTypeBusiness, email: c.email}
}

// Compile-time interface assertions.
var (
	_ encoding.TextMarshaler   = CanonicalID{}
	_ encoding.TextUnmarshaler = (*CanonicalID)(nil)
	_ fmt.Stringer             = CanonicalID{}
)
