package syncdomain

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ActionCompletion reports the terminal outcome of one planned action. The
// engine reads these from the completions channel, classifies them, and calls
// DepGraph.Complete. Failed items become retry_work, block_scopes, or other
// engine-owned durable control state through the persistence flow.
//
// Production builds every completion in one place
// (syncexec.CompletionFromTrackedAction), which reads the Graph facts --
// HTTPStatus, RetryAfter, ServerAnswer -- from Err once; no one changes Err
// after that, so the recorded answer cannot disagree with the error it was
// read from. Tests build theirs through syncexec.CompletionForTest, which
// reads them the same way.
type ActionCompletion struct {
	Path       string
	OldPath    string
	ItemID     string
	DriveID    driveid.ID
	ActionType ActionType
	// ItemType is the kind of item the action touched, as its planned view
	// says (ResolveItemType): what the activity feed tells a summary.
	ItemType          ItemType
	Success           bool
	ErrMsg            string
	HTTPStatus        int // the Graph answer's status, 0 if Err carries none
	FailurePath       string
	FailureCapability PermissionCapability

	// Size is the transferred file size for display, when the outcome knows it.
	Size      int64
	SizeKnown bool

	// Err is the full error for classification (os.ErrPermission, a Graph
	// answer, a timed-out request and so on).
	Err error

	// ServerAnswer is what Err's Graph failure means (ServerAnswerNone when
	// it carries none), read once when the completion is built.
	ServerAnswer ServerAnswer

	// RunEnded is whether the run the action ran under had ended when it
	// returned (RunEnded): the one fact that makes a failure a shutdown. Set
	// by whoever ran the action, from the run's context; the classifier reads
	// it and no context.
	RunEnded bool

	// RetryAfter is the server-mandated wait duration from the Retry-After
	// header on 429/503 responses. Zero when absent. Used by block scopes
	// for initial trial timing (R-2.10.7, R-2.10.8).
	RetryAfter time.Duration

	// IsTrial is true if this was a scope trial action (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies the scope being tested by this trial.
	TrialScopeKey ScopeKey

	// TrialScopeGeneration is the scope generation the trial was released
	// under (TrackedAction.TrialScopeGeneration).
	TrialScopeGeneration uint64

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
	if r.DriveID.IsZero() {
		return ""
	}
	return throttleDriveParam(r.DriveID)
}
