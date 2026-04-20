package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ActionCompletion reports the terminal outcome of one planned action. The
// engine reads these from the completions channel, classifies them, and calls
// depGraph.Complete. Failed items become retry_work, block_scopes, or other
// engine-owned durable control state through the persistence flow.
type ActionCompletion struct {
	Path              string
	OldPath           string
	ItemID            string
	DriveID           driveid.ID
	ActionType        ActionType
	Success           bool
	ErrMsg            string
	HTTPStatus        int // from graph.GraphError, 0 if not a Graph API error
	FailurePath       string
	FailureCapability PermissionCapability

	// Err is the full error for classification (context.Canceled, os.ErrPermission, etc.).
	// The engine uses errors.Is to distinguish shutdown from genuine failures.
	Err error

	// RetryAfter is the server-mandated wait duration from the Retry-After
	// header on 429/503 responses. Zero when absent. Used by block scopes
	// for initial trial timing (R-2.10.7, R-2.10.8).
	RetryAfter time.Duration

	// TargetDriveID is the actual drive ID targeted by this action. For
	// normal drives, equals DriveID. For shared-folder drives rooted below the
	// remote drive root, it still names the backing drive so classification and
	// cleanup use one consistent authority boundary.
	TargetDriveID driveid.ID

	// IsTrial is true if this was a scope trial action (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies the scope being tested by this trial.
	TrialScopeKey ScopeKey

	// ActionID is the TrackedAction.ID for the engine to call Complete on
	// the DepGraph.
	ActionID int64
}

// ThrottleTargetKey returns the narrowest remote boundary that can be blocked
// after a 429 for this action completion.
func (r *ActionCompletion) ThrottleTargetKey() string {
	if r == nil {
		return ""
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

func completionFromTrackedAction(
	ta *TrackedAction,
	outcome *ActionOutcome,
	actionErr error,
) ActionCompletion {
	driveID := ta.Action.DriveID
	if outcome != nil && !outcome.DriveID.IsZero() {
		driveID = outcome.DriveID
	} else if driveID.IsZero() && !ta.Action.TargetDriveID.IsZero() {
		// Shortcut-targeted actions may defer drive resolution to execution time.
		// When no concrete action drive was planned, retain the intended target
		// drive so failure persistence and success cleanup address the same row.
		driveID = ta.Action.TargetDriveID
	}

	r := ActionCompletion{
		Path:          ta.Action.Path,
		OldPath:       ta.Action.OldPath,
		ItemID:        ta.Action.ItemID,
		DriveID:       driveID,
		ActionType:    ta.Action.Type,
		Err:           actionErr,
		ErrMsg:        "",
		HTTPStatus:    ExtractHTTPStatus(actionErr),
		RetryAfter:    ExtractRetryAfter(actionErr),
		TargetDriveID: ta.Action.TargetDriveID,
		IsTrial:       ta.IsTrial,
		TrialScopeKey: ta.TrialScopeKey,
		ActionID:      ta.ID,
	}

	if outcome != nil {
		r.Success = outcome.Success
		if outcome.Error != nil {
			r.ErrMsg = outcome.Error.Error()
			r.Err = outcome.Error
			r.HTTPStatus = ExtractHTTPStatus(outcome.Error)
			r.RetryAfter = ExtractRetryAfter(outcome.Error)
		}
	} else if actionErr != nil {
		r.ErrMsg = actionErr.Error()
	}

	return r
}
