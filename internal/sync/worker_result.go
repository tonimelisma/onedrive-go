package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// WorkerResult reports the outcome of a single action execution. The engine
// reads these from the Results channel, classifies them, and calls
// depGraph.Complete. Failed items are recorded in sync_failures for retry
// by the engine retry sweep.
type WorkerResult struct {
	Path       string
	ItemID     string
	DriveID    driveid.ID
	ActionType ActionType
	Success    bool
	ErrMsg     string
	HTTPStatus int // from graph.GraphError, 0 if not a Graph API error

	// Err is the full error for classification (context.Canceled, os.ErrPermission, etc.).
	// The engine uses errors.Is to distinguish shutdown from genuine failures.
	Err error

	// RetryAfter is the server-mandated wait duration from the Retry-After
	// header on 429/503 responses. Zero when absent. Used by scope blocks
	// for initial trial timing (R-2.10.7, R-2.10.8).
	RetryAfter time.Duration

	// TargetDriveID is the actual drive ID targeted by this action. For
	// own-drive actions, equals DriveID. For shortcut actions, equals the
	// sharer's drive. Flows through the pipeline without lookup (R-6.8.12).
	TargetDriveID driveid.ID

	// ShortcutKey identifies the shortcut scope. Format: "remoteDrive:remoteItem".
	// Empty for own-drive actions. Used by updateScope for 507 scope keys (R-2.10.16).
	ShortcutKey string

	// IsTrial is true if this was a scope trial action (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies the scope being tested by this trial.
	TrialScopeKey ScopeKey

	// ActionID is the TrackedAction.ID for the engine to call Complete on
	// the DepGraph.
	ActionID int64
}

// ThrottleTargetKey returns the narrowest remote boundary that can be blocked
// after a 429 for this worker result.
func (r *WorkerResult) ThrottleTargetKey() string {
	if r == nil {
		return ""
	}

	if r.ShortcutKey != "" {
		return throttleSharedPrefix + r.ShortcutKey
	}
	targetDriveID := r.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = r.DriveID
	}
	if targetDriveID.IsZero() {
		return ""
	}
	return throttleDriveParam(targetDriveID)
}
