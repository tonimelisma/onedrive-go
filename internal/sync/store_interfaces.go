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
// These are declarations only in Phase 5.7.0. Nothing implements or uses
// them yet — they exist so Phase 5.7.1+ can wire them incrementally.
//
// See docs/design/remote-state-separation.md §12 for the full design.

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
// Records failure metadata on remote_state rows.
type FailureRecorder interface {
	RecordFailure(ctx context.Context, path string, errMsg string, httpStatus int) error
}

// ConflictEscalator is called by the reconciler goroutine (single caller).
// Writes a conflict record when a permanently-failing item exceeds the
// retry threshold (e.g., non-empty directory delete after 10 failures).
type ConflictEscalator interface {
	EscalateToConflict(ctx context.Context, driveID driveid.ID, itemID, path, reason string) error
}

// StateReader is called by reconciler, planner, status, CLI (read-only).
// All methods are pure reads. Multiple goroutines call concurrently.
// WAL mode guarantees readers never block.
type StateReader interface {
	ListUnreconciled(ctx context.Context) ([]RemoteStateRow, error)
	ListFailedForRetry(ctx context.Context, now time.Time) ([]RemoteStateRow, error)
	FailureCount(ctx context.Context) (int, error)
	BaselineEntryCount(ctx context.Context) (int, error)
	UnresolvedConflictCount(ctx context.Context) (int, error)
	ReadSyncMetadata(ctx context.Context) (map[string]string, error)
}

// StateAdmin is called by CLI commands and daemon maintenance.
// Write operations that don't fit the hot path.
type StateAdmin interface {
	ResetFailure(ctx context.Context, path string) error
	ResetAllFailures(ctx context.Context) error
	ResetInProgressStates(ctx context.Context) error
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
// the reconciler and status queries.
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
	FailureCount int
	NextRetryAt  int64
	LastError    string
	HTTPStatus   int
}
