package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// RecoveryCandidate identifies one remote_state row that DB repair must
// reconcile while preserving durable truth.
type RecoveryCandidate struct {
	DriveID string
	ItemID  string
	Path    string
}

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

// SyncRunReport is the durable one-shot status projection persisted after a
// completed run.
type SyncRunReport struct {
	CompletedAt time.Time
	Duration    time.Duration
	Succeeded   int
	Failed      int
	Errors      []error
}
