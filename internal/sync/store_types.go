package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SyncFailureParams bundles all inputs for RecordFailure into a single struct.
// Only Path, DriveID, Direction, and ErrMsg are always required. Other fields
// are optional — zero values are preserved via COALESCE on conflict.
type SyncFailureParams struct {
	Path       string
	DriveID    driveid.ID
	Direction  Direction
	Role       FailureRole
	IssueType  string
	Category   FailureCategory
	ErrMsg     string
	HTTPStatus int
	ActionType ActionType
	FileSize   int64
	LocalHash  string
	ItemID     string
	ScopeKey   ScopeKey
}

// SyncFailureRow represents a row from the sync_failures table.
type SyncFailureRow struct {
	Path         string
	DriveID      driveid.ID
	Direction    Direction
	Role         FailureRole
	Category     FailureCategory
	IssueType    string
	ItemID       string
	ActionType   ActionType
	FailureCount int
	NextRetryAt  int64
	LastError    string
	HTTPStatus   int
	FirstSeenAt  int64
	LastSeenAt   int64
	FileSize     int64
	LocalHash    string
	ScopeKey     ScopeKey
}

// ActionableFailure represents a scanner-detected issue to batch-upsert into
// sync_failures.
type ActionableFailure struct {
	Path       string
	DriveID    driveid.ID
	Direction  Direction
	ActionType ActionType
	Role       FailureRole
	IssueType  string
	Error      string
	ScopeKey   ScopeKey
	FileSize   int64
}

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID          driveid.ID
	ItemID           string
	ParentID         string
	Path             string
	ItemType         ItemType
	Hash             string
	Size             int64
	Mtime            int64
	ETag             string
	IsDeleted        bool
	Filtered         bool
	FilterGeneration int64
	FilterReason     RemoteFilterReason
}

// RemoteStateRow represents a row from the remote_state table.
type RemoteStateRow struct {
	DriveID          driveid.ID
	ItemID           string
	Path             string
	ParentID         string
	ItemType         ItemType
	Hash             string
	Size             int64
	Mtime            int64
	ETag             string
	PreviousPath     string
	IsFiltered       bool
	ObservedAt       int64
	FilterGeneration int64
	FilterReason     RemoteFilterReason
}

// PendingRetryGroup aggregates transient failures by scope_key, with the
// earliest next_retry_at per group.
type PendingRetryGroup struct {
	ScopeKey     ScopeKey
	Count        int
	EarliestNext time.Time
}
