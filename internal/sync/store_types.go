package sync

import (
	"time"

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
// single-path observation may instead manage exact paths without touching
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

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID   driveid.ID
	ItemID    string
	Path      string
	ItemType  ItemType
	Hash      string
	Size      int64
	Mtime     int64
	ETag      string
	IsDeleted bool
}

// RemoteStateRow represents a row from the remote_state table.
type RemoteStateRow struct {
	DriveID  driveid.ID
	ItemID   string
	Path     string
	ItemType ItemType
	Hash     string
	Size     int64
	Mtime    int64
	ETag     string
}

// LocalStateRow represents a row from the local_state table.
type LocalStateRow struct {
	Path     string
	ItemType ItemType
	Hash     string
	Size     int64
	Mtime    int64
}

// RetryWorkRow represents a row from the retry_work table.
type RetryWorkRow struct {
	Path         string
	OldPath      string
	ActionType   ActionType
	ScopeKey     ScopeKey
	Blocked      bool
	AttemptCount int
	NextRetryAt  int64
}

// PendingRetryGroup aggregates transient failures by scope_key, with the
// earliest next_retry_at per group.
type PendingRetryGroup struct {
	ScopeKey     ScopeKey
	Count        int
	EarliestNext time.Time
}

// RetryWorkKey identifies semantic work that may be retried across replans.
// It intentionally stays smaller than a runtime action because retry_work
// persists only delayed obligations, not the executable action set.
type RetryWorkKey struct {
	Path       string
	OldPath    string
	ActionType ActionType
}
