package sync

import "github.com/tonimelisma/onedrive-go/internal/driveid"

// BaselineMutation is the store-owned persistence input produced from one
// executed action result.
type BaselineMutation struct {
	Action ActionType
	// Success is carried for safety and for tests that seed mixed result sets.
	// CommitMutation no-ops failed mutations so store persistence stays aligned
	// with the engine's success-only commit contract.
	Success         bool
	Path            string
	OldPath         string
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	ItemType        ItemType
	LocalHash       string
	RemoteHash      string
	LocalSize       int64
	LocalSizeKnown  bool
	RemoteSize      int64
	RemoteSizeKnown bool
	LocalMtime      int64
	RemoteMtime     int64
	ETag            string
	ConflictType    string
	ResolvedBy      string
}
