package synctypes

import (
	"context"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ---------------------------------------------------------------------------
// Store interfaces — Sub-interfaces for SyncStore. Each interface exposes the
// narrowest set of methods needed by a specific caller.
// ---------------------------------------------------------------------------

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
// The delayFn parameter computes backoff from failure count: engine passes
// retry.ReconcilePolicy().Delay for transient failures, nil for actionable (no retry).
// The store calls delayFn atomically inside the transaction — it never imports
// the retry package.
type SyncFailureRecorder interface {
	RecordFailure(ctx context.Context, p *SyncFailureParams, delayFn func(int) time.Duration) error
	ListSyncFailures(ctx context.Context) ([]SyncFailureRow, error)
	ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]SyncFailureRow, error)
	ListActionableFailures(ctx context.Context) ([]SyncFailureRow, error)
	ListRemoteBlockedFailures(ctx context.Context) ([]SyncFailureRow, error)
	ClearSyncFailure(ctx context.Context, path string, driveID driveid.ID) error
	ClearActionableSyncFailures(ctx context.Context) error
	MarkSyncFailureActionable(ctx context.Context, path string, driveID driveid.ID) error
	UpsertActionableFailures(ctx context.Context, failures []ActionableFailure) error
	ClearResolvedActionableFailures(ctx context.Context, issueType string, currentPaths []string) error
	ResetRetryTimesForScope(ctx context.Context, scopeKey ScopeKey, now time.Time) error
}

// StateAdmin is called by CLI commands for administrative writes that do not
// fit the hot path.
type StateAdmin interface {
	ResetFailure(ctx context.Context, path string) error
	ResetAllFailures(ctx context.Context) error
}

// CrashRecoveryStore persists the state-only half of crash recovery. The
// caller decides local filesystem presence under the sync root; the store
// owns only remote_state transitions and sync_failures persistence.
type CrashRecoveryStore interface {
	ResetDownloadingStates(ctx context.Context, delayFn func(int) time.Duration) error
	ListDeletingCandidates(ctx context.Context) ([]RecoveryCandidate, error)
	FinalizeDeletingStates(
		ctx context.Context,
		deleted []RecoveryCandidate,
		pending []RecoveryCandidate,
		delayFn func(int) time.Duration,
	) error
}

// ScopeBlockStore persists scope blocks to durable storage. The engine loads
// these rows into its watch-mode working state at startup and mutates the
// database transactionally as scopes activate, release, or discard.
type ScopeBlockStore interface {
	UpsertScopeBlock(ctx context.Context, block *ScopeBlock) error
	DeleteScopeBlock(ctx context.Context, key ScopeKey) error
	ListScopeBlocks(ctx context.Context) ([]*ScopeBlock, error)
}

// ---------------------------------------------------------------------------
// Store DTOs — Data transfer objects used by store interfaces.
// ---------------------------------------------------------------------------

// SyncFailureParams bundles all inputs for RecordFailure into a single struct.
// Only Path, DriveID, Direction, and ErrMsg are always required. Other fields
// are optional — zero values are preserved via COALESCE on conflict.
type SyncFailureParams struct {
	Path       string          // required
	DriveID    driveid.ID      // required
	Direction  Direction       // DirectionUpload, DirectionDownload, DirectionDelete
	Role       FailureRole     // item, held, boundary
	IssueType  string          // e.g. IssueQuotaExceeded; empty = generic transient
	Category   FailureCategory // CategoryTransient or CategoryActionable — set by the engine, never by the store
	ErrMsg     string
	HTTPStatus int
	ActionType ActionType
	FileSize   int64    // optional, for upload validation context
	LocalHash  string   // optional, for upload validation context
	ItemID     string   // optional; auto-resolved from remote_state when empty
	ScopeKey   ScopeKey // typed scope key; zero value = unscoped
}

// SyncFailureRow represents a row from the sync_failures table.
type SyncFailureRow struct {
	Path                   string
	DriveID                driveid.ID
	Direction              Direction // DirectionDownload, DirectionUpload, DirectionDelete
	Role                   FailureRole
	Category               FailureCategory // CategoryTransient, CategoryActionable
	IssueType              string
	ItemID                 string
	ActionType             ActionType
	FailureCount           int
	NextRetryAt            int64
	LastError              string
	HTTPStatus             int
	FirstSeenAt            int64
	LastSeenAt             int64
	FileSize               int64
	LocalHash              string
	ScopeKey               ScopeKey // typed scope key; zero value = unscoped
	ManualTrialRequestedAt int64
}

// ActionableFailure represents a scanner-detected issue to batch-upsert into
// sync_failures. Used by UpsertActionableFailures.
type ActionableFailure struct {
	Path       string
	DriveID    driveid.ID
	Direction  Direction
	ActionType ActionType
	Role       FailureRole
	IssueType  string
	Error      string
	ScopeKey   ScopeKey // typed scope key; zero value = unscoped
	FileSize   int64
}

// ObservedItem represents a single item from a delta API response, ready
// for CommitObservation to process against existing remote_state.
type ObservedItem struct {
	DriveID   driveid.ID
	ItemID    string
	ParentID  string
	Path      string
	ItemType  ItemType
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
	ItemType     ItemType
	Hash         string
	Size         int64
	Mtime        int64
	ETag         string
	PreviousPath string
	SyncStatus   SyncStatus
	ObservedAt   int64
}

// RecoveryCandidate identifies one remote_state row that crash recovery must
// resolve after a previous in-progress execution was interrupted.
type RecoveryCandidate struct {
	DriveID string
	ItemID  string
	Path    string
}

// PendingRetryGroup aggregates transient failures by scope_key, with the
// earliest next_retry_at per group. Used by the issues command (R-2.10.22).
type PendingRetryGroup struct {
	ScopeKey     ScopeKey // typed scope key; zero value = unscoped
	Count        int
	EarliestNext time.Time
}
