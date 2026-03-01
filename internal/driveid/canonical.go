package driveid

import (
	"encoding"
	"fmt"
	"sort"
	"strings"
)

// Drive type constants used in canonical IDs.
const (
	DriveTypePersonal   = "personal"
	DriveTypeBusiness   = "business"
	DriveTypeSharePoint = "sharepoint"
	DriveTypeShared     = "shared"
)

// canonicalIDMaxParts is the maximum number of colon-separated segments in any
// canonical ID format. Both SharePoint (type:email:site:library) and shared
// (type:email:sourceDriveID:sourceItemID) use 4 parts.
const canonicalIDMaxParts = 4

// validDriveTypes enumerates accepted drive type prefixes in canonical IDs.
var validDriveTypes = map[string]bool{
	DriveTypePersonal:   true,
	DriveTypeBusiness:   true,
	DriveTypeSharePoint: true,
	DriveTypeShared:     true,
}

// IsValidDriveType reports whether the given string is a valid drive type prefix.
func IsValidDriveType(t string) bool {
	return validDriveTypes[t]
}

// validTypeList returns a sorted, comma-separated list of valid drive types
// for use in error messages. Derived from validDriveTypes so it never drifts.
func validTypeList() string {
	types := make([]string, 0, len(validDriveTypes))
	for t := range validDriveTypes {
		types = append(types, t)
	}

	sort.Strings(types)

	return strings.Join(types, ", ")
}

// CanonicalID is a config-level drive identifier with one of four formats:
//
//   - "personal:email"
//   - "business:email"
//   - "sharepoint:email:site:library"
//   - "shared:email:sourceDriveID:sourceItemID"
//
// The zero value (CanonicalID{}) represents an absent canonical ID.
//
// Fields are parsed once at construction time and routed to type-specific
// struct fields — accessors return stored values without re-splitting.
// SharePoint and shared types use different field sets for parts 3-4.
type CanonicalID struct {
	driveType     string // "personal", "business", "sharepoint", "shared"
	email         string // account email (all types)
	site          string // SharePoint only: site name
	library       string // SharePoint only: document library name
	sourceDriveID string // Shared only: source drive ID (e.g., "b!TG9yZW0")
	sourceItemID  string // Shared only: source item ID (e.g., "01ABCDEF")
}

// NewCanonicalID parses and validates a raw canonical ID string. Returns
// an error if the format is invalid (unknown type, empty email, or wrong
// number of parts for the given type).
//
// Part-count rules:
//   - personal, business: exactly 2 parts (type:email)
//   - sharepoint: 2-4 parts (type:email[:site[:library]])
//   - shared: exactly 4 parts (type:email:sourceDriveID:sourceItemID)
func NewCanonicalID(raw string) (CanonicalID, error) {
	parts := strings.SplitN(raw, ":", canonicalIDMaxParts)
	if len(parts) < 2 || parts[1] == "" {
		return CanonicalID{}, fmt.Errorf("driveid: canonical ID %q must be \"type:email\" format", raw)
	}

	driveType := parts[0]
	if !validDriveTypes[driveType] {
		return CanonicalID{}, fmt.Errorf(
			"driveid: canonical ID %q has unknown type %q (valid: %s)", raw, driveType, validTypeList())
	}

	cid := CanonicalID{
		driveType: driveType,
		email:     parts[1],
	}

	// Route remaining parts to type-specific fields.
	switch driveType {
	case DriveTypePersonal, DriveTypeBusiness:
		if len(parts) > 2 {
			return CanonicalID{}, fmt.Errorf(
				"driveid: %s canonical ID %q must have exactly 2 parts (type:email), got %d",
				driveType, raw, len(parts))
		}

	case DriveTypeSharePoint:
		if len(parts) >= 3 {
			cid.site = parts[2]
		}

		if len(parts) >= canonicalIDMaxParts {
			cid.library = parts[3]
		}

	case DriveTypeShared:
		if len(parts) != canonicalIDMaxParts {
			return CanonicalID{}, fmt.Errorf(
				"driveid: shared canonical ID %q must have exactly 4 parts "+
					"(shared:email:sourceDriveID:sourceItemID), got %d", raw, len(parts))
		}

		cid.sourceDriveID = parts[2]
		cid.sourceItemID = parts[3]

		if cid.sourceDriveID == "" || cid.sourceItemID == "" {
			return CanonicalID{}, fmt.Errorf(
				"driveid: shared canonical ID %q requires non-empty source drive ID and item ID", raw)
		}
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
//
// For SharePoint drives, use ConstructSharePoint() instead — it enforces
// required site and library fields. For shared drives, use ConstructShared().
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

// ConstructShared builds a shared drive canonical ID from separate parts.
// Returns an error if any required field is empty.
func ConstructShared(email, sourceDriveID, sourceItemID string) (CanonicalID, error) {
	if email == "" {
		return CanonicalID{}, fmt.Errorf("driveid: shared canonical ID requires non-empty email")
	}

	if sourceDriveID == "" || sourceItemID == "" {
		return CanonicalID{}, fmt.Errorf("driveid: shared canonical ID requires non-empty source drive ID and item ID")
	}

	return CanonicalID{
		driveType:     DriveTypeShared,
		email:         email,
		sourceDriveID: sourceDriveID,
		sourceItemID:  sourceItemID,
	}, nil
}

// String returns the canonical ID string. The format depends on drive type:
//
//   - personal/business: "type:email"
//   - sharepoint: "type:email[:site[:library]]"
//   - shared: "type:email:sourceDriveID:sourceItemID"
func (c CanonicalID) String() string {
	if c.driveType == "" {
		return ""
	}

	switch c.driveType {
	case DriveTypePersonal, DriveTypeBusiness:
		return c.driveType + ":" + c.email

	case DriveTypeSharePoint:
		s := c.driveType + ":" + c.email
		if c.site != "" {
			s += ":" + c.site
		}

		if c.library != "" {
			s += ":" + c.library
		}

		return s

	case DriveTypeShared:
		return c.driveType + ":" + c.email + ":" + c.sourceDriveID + ":" + c.sourceItemID

	default:
		return c.driveType + ":" + c.email
	}
}

// IsZero reports whether this is the zero-value CanonicalID.
func (c CanonicalID) IsZero() bool {
	return c.driveType == ""
}

// Equal reports whether two CanonicalIDs are identical.
func (c CanonicalID) Equal(other CanonicalID) bool {
	return c.driveType == other.driveType &&
		c.email == other.email &&
		c.site == other.site &&
		c.library == other.library &&
		c.sourceDriveID == other.sourceDriveID &&
		c.sourceItemID == other.sourceItemID
}

// DriveType returns the type prefix (e.g., "personal", "business", "sharepoint", "shared").
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) DriveType() string {
	return c.driveType
}

// Email returns the email portion of the canonical ID.
// Returns empty string for zero-value CanonicalID.
func (c CanonicalID) Email() string {
	return c.email
}

// IsPersonal reports whether this is a personal drive.
func (c CanonicalID) IsPersonal() bool {
	return c.driveType == DriveTypePersonal
}

// IsBusiness reports whether this is a business drive.
func (c CanonicalID) IsBusiness() bool {
	return c.driveType == DriveTypeBusiness
}

// IsSharePoint reports whether this is a SharePoint drive.
func (c CanonicalID) IsSharePoint() bool {
	return c.driveType == DriveTypeSharePoint
}

// IsShared reports whether this is a shared drive (folder/item from another user's drive).
func (c CanonicalID) IsShared() bool {
	return c.driveType == DriveTypeShared
}

// Site returns the SharePoint site name from the canonical ID.
// Returns empty string for non-SharePoint drives or zero-value CanonicalID.
func (c CanonicalID) Site() string {
	if !c.IsSharePoint() {
		return ""
	}

	return c.site
}

// Library returns the SharePoint document library name from the canonical ID.
// Returns empty string for non-SharePoint drives or zero-value CanonicalID.
func (c CanonicalID) Library() string {
	if !c.IsSharePoint() {
		return ""
	}

	return c.library
}

// SourceDriveID returns the source drive ID for shared drives.
// For "shared:me@outlook.com:b!TG9yZW0:01ABCDEF" returns "b!TG9yZW0".
// Returns empty string for non-shared drives or zero-value CanonicalID.
func (c CanonicalID) SourceDriveID() string {
	if !c.IsShared() {
		return ""
	}

	return c.sourceDriveID
}

// SourceItemID returns the source item ID for shared drives.
// For "shared:me@outlook.com:b!TG9yZW0:01ABCDEF" returns "01ABCDEF".
// Returns empty string for non-shared drives or zero-value CanonicalID.
func (c CanonicalID) SourceItemID() string {
	if !c.IsShared() {
		return ""
	}

	return c.sourceItemID
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

// Compile-time interface assertions.
var (
	_ encoding.TextMarshaler   = CanonicalID{}
	_ encoding.TextUnmarshaler = (*CanonicalID)(nil)
	_ fmt.Stringer             = CanonicalID{}
)
