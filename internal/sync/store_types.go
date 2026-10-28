package sync

import (
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ObservationIssue is a durable current-truth problem discovered by
// observation and shown to the user until the underlying path becomes
// syncable again.
type ObservationIssue struct {
	Path      string
	DriveID   driveid.ID
	IssueType string
	ScopeKey  ScopeKey
}

// ObservationFindingsBatch is one coherent observation-owned durable batch.
// The batch replaces the current observation-owned issue set for its managed
// issue families. Full observation passes manage whole issue families;
// an exact-path batch may instead manage specific paths without touching
// unrelated durable findings.
type ObservationFindingsBatch struct {
	Issues            []ObservationIssue
	ManagedIssueTypes []string
	ManagedPaths      []string
}

// ObservationIssueRow represents a row from the observation_issues table.
type ObservationIssueRow struct {
	Path      string
	DriveID   driveid.ID
	IssueType string
	ScopeKey  ScopeKey
}

// RetryWorkFailure records one exact work item that still needs a retry. The
// retry_work table owns attempt counting and backoff state for delayed exact
// retry rows. Blocked scope rows use the exact RetryWorkKey plus ScopeKey
// directly; they are not modeled as the same failure shape.
type RetryWorkFailure struct {
	Work     RetryWorkKey
	ScopeKey ScopeKey
}

// observedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type observedItem struct {
	DriveID  driveid.ID
	ItemID   string
	ParentID string // places the children of a shadowed remote twin (store_remote_twins.go)
	Path     string
	ItemType ItemType
	Hash     string
	Size     int64
	// SizeUnknown stores the size as NULL (R-2.4.16).
	SizeUnknown bool
	Mtime       int64
	ETag        string
	IsDeleted   bool
	// ReuploadLocal also forgets the deleted item's baseline rows, subtree
	// included, so its local copy plans as an upload (R-2.15.3).
	ReuploadLocal bool
}

// remoteStateRow represents a row from the remote_state table.
type remoteStateRow struct {
	DriveID  driveid.ID
	ItemID   string
	Path     string
	ItemType ItemType
	Hash     string
	Size     int64
	// SizeKnown is false only when the stored size is NULL: a file item
	// from delta always carries its size, zero included.
	SizeKnown bool
	Mtime     int64
	ETag      string
}

// localStateRow represents a row from the local_state table.
type localStateRow struct {
	Path             string
	ItemType         ItemType
	Hash             string
	Size             int64
	Mtime            int64
	Ctime            int64 // inode change time (Unix nanoseconds); zero means unknown
	LocalDevice      uint64
	LocalInode       uint64
	LocalHasIdentity bool
}

// RetryWorkRow represents a row from the retry_work table.
type RetryWorkRow struct {
	Path         string
	OldPath      string
	ActionType   actionType
	ScopeKey     ScopeKey
	Blocked      bool
	AttemptCount int
	NextRetryAt  int64
}

// A file with a new identity whose content is provably the synced content is
// the synced item too: identity is a pairing hint, never a change signal
// (D-057). A folder rebuilt with the same files gives each a new inode, and
// requiring the old one superseded every move of them forever.
func localRowIsBaselineItem(row *localStateRow, baseline *BaselineEntry) bool {
	if row == nil || baseline == nil {
		return row == nil && baseline == nil
	}
	if row.ItemType != baseline.ItemType {
		return false
	}

	return localRowIdentityMatchesBaseline(row, baseline) || localRowHoldsBaselineContent(row, baseline)
}

// localRowHoldsBaselineContent reports whether a local file provably holds
// the synced content: both hashes known and equal, and the same size.
func localRowHoldsBaselineContent(row *localStateRow, baseline *BaselineEntry) bool {
	return row.ItemType == ItemTypeFile && row.Hash != "" && row.Hash == baseline.LocalHash &&
		(!baseline.LocalSizeKnown || row.Size == baseline.LocalSize)
}

func localRowIdentityMatchesBaseline(row *localStateRow, baseline *BaselineEntry) bool {
	if !baseline.LocalHasIdentity || !row.LocalHasIdentity {
		return true
	}

	return row.LocalDevice == baseline.LocalDevice && row.LocalInode == baseline.LocalInode
}
