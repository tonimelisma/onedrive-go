package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ObservationIssue is a durable current-truth problem discovered by
// observation and shown to the user until the underlying path becomes
// syncable again.
type ObservationIssue struct {
	Path       string
	DriveID    driveid.ID
	ActionType ActionType
	IssueType  string
	Error      string
	ItemID     string
	ScopeKey   ScopeKey
	FileSize   int64
	LocalHash  string
}

// ObservationFindingsBatch is one coherent observation-owned durable batch.
// The batch replaces the current observation-owned issue set for its managed
// issue families and read scopes.
type ObservationFindingsBatch struct {
	Issues     []ObservationIssue
	ReadScopes []ScopeKey
}

// ObservationIssueRow represents a row from the observation_issues table.
type ObservationIssueRow struct {
	Path        string
	DriveID     driveid.ID
	ActionType  ActionType
	IssueType   string
	ItemID      string
	LastError   string
	FirstSeenAt int64
	LastSeenAt  int64
	FileSize    int64
	LocalHash   string
	ScopeKey    ScopeKey
}

// RetryWorkFailure records one exact work item that still needs a retry. The
// retry_work table owns attempt counting and backoff state.
type RetryWorkFailure struct {
	Path       string
	OldPath    string
	ActionType ActionType
	IssueType  string
	ScopeKey   ScopeKey
	LastError  string
	HTTPStatus int
	Blocked    bool
}

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID         driveid.ID
	ItemID          string
	ParentID        string
	Path            string
	ItemType        ItemType
	Hash            string
	Size            int64
	Mtime           int64
	ETag            string
	ContentIdentity string
	IsDeleted       bool
}

// RemoteStateRow represents a row from the remote_state table.
type RemoteStateRow struct {
	DriveID         driveid.ID
	ItemID          string
	Path            string
	ParentID        string
	ItemType        ItemType
	Hash            string
	Size            int64
	Mtime           int64
	ETag            string
	ContentIdentity string
	PreviousPath    string
}

// LocalStateRow represents a row from the local_state table.
type LocalStateRow struct {
	Path            string
	ItemType        ItemType
	Hash            string
	Size            int64
	Mtime           int64
	ContentIdentity string
	ObservedAt      int64
}

// RetryWorkRow represents a row from the retry_work table.
type RetryWorkRow struct {
	WorkKey      string
	Path         string
	OldPath      string
	ActionType   ActionType
	IssueType    string
	ScopeKey     ScopeKey
	Blocked      bool
	AttemptCount int
	NextRetryAt  int64
	LastError    string
	HTTPStatus   int
	FirstSeenAt  int64
	LastSeenAt   int64
}

// SyncRunStatus is the typed one-shot status row persisted for product-facing
// status output.
type SyncRunStatus struct {
	LastCompletedAt    int64
	LastDurationMs     int64
	LastSucceededCount int
	LastFailedCount    int
	LastError          string
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
