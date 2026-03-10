package sync

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Sub-interfaces for SyncStore. Each interface exposes the narrowest set of
// methods needed by a specific caller. SyncStore implements all of them.
// Components receive the interface they need, not the full SyncStore.
//
// See spec/design/sync-store.md for the full design.

// ObservationWriter is called by the RemoteObserver goroutine (single caller).
// Writes observed remote state and advances the delta token atomically.
type ObservationWriter interface {
	CommitObservation(ctx context.Context, events []ObservedItem, newToken string, driveID driveid.ID) error
}

// OutcomeWriter is called by worker goroutines (N concurrent callers).
// Commits action results to baseline and updates remote_state on success.
type OutcomeWriter interface {
	CommitOutcome(ctx context.Context, outcome *Outcome) error
	Load(ctx context.Context) (*Baseline, error)
}

// FailureRecorder is called by the drainWorkerResults goroutine (single caller).
// Records failure metadata in sync_failures and transitions remote_state status.
type FailureRecorder interface {
	RecordFailure(ctx context.Context, path string, driveID driveid.ID, direction, errMsg string, httpStatus int) error
}

// StateReader is called by failure retrier, planner, status, CLI (read-only).
// All methods are pure reads. Multiple goroutines call concurrently.
// WAL mode guarantees readers never block.
type StateReader interface {
	// ListUnreconciled returns remote_state rows not yet reconciled with
	// baseline. Used by the status command to show pending items.
	ListUnreconciled(ctx context.Context) ([]RemoteStateRow, error)
	ListSyncFailuresForRetry(ctx context.Context, now time.Time) ([]SyncFailureRow, error)
	EarliestSyncFailureRetryAt(ctx context.Context, now time.Time) (time.Time, error)
	SyncFailureCount(ctx context.Context) (int, error)
	BaselineEntryCount(ctx context.Context) (int, error)
	UnresolvedConflictCount(ctx context.Context) (int, error)
	ReadSyncMetadata(ctx context.Context) (map[string]string, error)
}

// SyncFailureRecorder is called by the engine to persist all failure types
// (upload, download, delete) in the unified sync_failures table.
type SyncFailureRecorder interface {
	RecordSyncFailure(ctx context.Context, path string, driveID driveid.ID, direction, issueType, errMsg string,
		httpStatus int, fileSize int64, localHash string, itemID string, scopeKey string) error
	ListSyncFailures(ctx context.Context) ([]SyncFailureRow, error)
	ListActionableFailures(ctx context.Context) ([]SyncFailureRow, error)
	ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error
	ClearActionableSyncFailures(ctx context.Context) error
	MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error
	UpsertActionableFailures(ctx context.Context, failures []ActionableFailure) error
	ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error
}

// StateAdmin is called by CLI commands and daemon maintenance.
// Write operations that don't fit the hot path.
type StateAdmin interface {
	ResetFailure(ctx context.Context, path string) error
	ResetAllFailures(ctx context.Context) error
	ResetInProgressStates(ctx context.Context, syncRoot string) error
}

// SyncFailureRow represents a row from the sync_failures table.
type SyncFailureRow struct {
	Path         string
	DriveID      driveid.ID
	Direction    string // "download", "upload", "delete"
	Category     string // "transient", "actionable"
	IssueType    string
	ItemID       string
	FailureCount int
	NextRetryAt  int64
	LastError    string
	HTTPStatus   int
	FirstSeenAt  int64
	LastSeenAt   int64
	FileSize     int64
	LocalHash    string
	ScopeKey     string // e.g. "quota:own", "throttle:account", "perm:remote:/path"
}

// ActionableFailure represents a scanner-detected issue to batch-upsert into
// sync_failures. Used by UpsertActionableFailures.
type ActionableFailure struct {
	Path      string
	DriveID   driveid.ID
	Direction string
	IssueType string
	Error     string
	ScopeKey  string
}

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID   driveid.ID
	ItemID    string
	ParentID  string
	Path      string
	ItemType  string // "file", "folder", "root"
	Hash      string
	Size      int64
	Mtime     int64
	ETag      string
	IsDeleted bool
}

// RemoteStateRow represents a row from the remote_state table, used by
// the failure retrier and status queries.
type RemoteStateRow struct {
	DriveID      driveid.ID
	ItemID       string
	Path         string
	ParentID     string
	ItemType     string
	Hash         string
	Size         int64
	Mtime        int64
	ETag         string
	PreviousPath string
	SyncStatus   string
	ObservedAt   int64
}
