// Package driveid provides type-safe drive identity types for OneDrive API
// identifiers. It consolidates normalization logic (lowercase, zero-padding)
// and provides compile-time safety over raw string usage.
//
// Three types cover the codebase's identity needs:
//   - ID: normalized Graph API drive identifier (lowercase, zero-padded)
//   - CanonicalID: config-level "type:email" identifier
//   - ItemKey: composite (DriveID, ItemID) pair for map keys
//
// This is a leaf package with zero external dependencies beyond stdlib.
package driveid

import (
	"database/sql"
	"database/sql/driver"
	"encoding"
	"fmt"
	"strings"
)

// idMinLength is the minimum length for a normalized drive ID. Personal
// accounts sometimes return 15-character IDs (documented API bug); we
// zero-pad to this length for consistent map keying and database lookups.
const idMinLength = 16

// ID is a normalized OneDrive API drive identifier. Lowercase and
// zero-padded to at least 16 characters for short IDs (Personal accounts).
// The zero value (ID{}) represents an absent or unknown drive ID.
type ID struct {
	value string
}

// New creates a normalized ID from a raw API drive identifier. Applies
// lowercase and left-pads short IDs (< 16 chars) with zeros. Empty input
// returns the zero ID (ID{}), which is the single representation for
// "absent/unknown". Callers can check IsZero() when that matters.
func New(raw string) ID {
	if raw == "" {
		return ID{}
	}

	lower := strings.ToLower(raw)
	if len(lower) >= idMinLength {
		return ID{value: lower}
	}

	return ID{value: strings.Repeat("0", idMinLength-len(lower)) + lower}
}

// String returns the normalized drive ID string.
func (id ID) String() string {
	return id.value
}

// IsZero reports whether this is the zero-value ID (empty or all zeros).
func (id ID) IsZero() bool {
	return id.value == "" || id.value == strings.Repeat("0", idMinLength)
}

// Equal reports whether two IDs are identical. Both zero-value forms
// (empty string from ID{} and all-zeros from New("0")) are considered equal.
// This prevents a subtle bug where two "zero" IDs created via different paths
// compare as unequal (see New("") vs New("0") vs ID{}).
func (id ID) Equal(other ID) bool {
	if id.value == other.value {
		return true
	}

	return id.IsZero() && other.IsZero()
}

// MarshalText implements encoding.TextMarshaler.
func (id ID) MarshalText() ([]byte, error) {
	return []byte(id.value), nil
}

// UnmarshalText implements encoding.TextUnmarshaler. The input is
// normalized (lowercased + zero-padded) just like New().
func (id *ID) UnmarshalText(text []byte) error {
	*id = New(string(text))
	return nil
}

// Scan implements sql.Scanner for reading drive IDs from SQLite. SQL NULL
// produces the zero ID.
func (id *ID) Scan(src any) error {
	if src == nil {
		*id = ID{}
		return nil
	}

	switch v := src.(type) {
	case string:
		*id = New(v)
		return nil
	case []byte:
		*id = New(string(v))
		return nil
	default:
		return fmt.Errorf("driveid.ID.Scan: unsupported type %T", src)
	}
}

// Value implements driver.Valuer for writing drive IDs to SQLite. The zero
// ID writes SQL NULL to match the Scan behavior.
func (id ID) Value() (driver.Value, error) {
	if id.IsZero() {
		return nil, nil
	}

	return id.value, nil
}

// Compile-time interface assertions.
var (
	_ encoding.TextMarshaler   = ID{}
	_ encoding.TextUnmarshaler = (*ID)(nil)
	_ fmt.Stringer             = ID{}
	_ driver.Valuer            = ID{}
	_ sql.Scanner              = (*ID)(nil)
)
