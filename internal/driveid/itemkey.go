package driveid

// ItemKey is a composite (DriveID, ItemID) pair used as a map key for
// baseline lookups and move detection. Replaces ad-hoc "driveID:itemID"
// string concatenation throughout the codebase.
//
// Comparable: Go structs with comparable fields support == and map keying.
// ID contains only an unexported string, so ItemKey is fully comparable.
type ItemKey struct {
	DriveID ID
	ItemID  string
}

// NewItemKey creates an ItemKey from a normalized drive ID and raw item ID.
func NewItemKey(driveID ID, itemID string) ItemKey {
	return ItemKey{DriveID: driveID, ItemID: itemID}
}

// String returns the "driveID:itemID" representation, matching the legacy
// concatenation format for logging and debugging.
func (k ItemKey) String() string {
	return k.DriveID.String() + ":" + k.ItemID
}

// IsZero reports whether both components are zero/empty.
func (k ItemKey) IsZero() bool {
	return k.DriveID.IsZero() && k.ItemID == ""
}
