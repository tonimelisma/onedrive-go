package syncdomain

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ActionCompletion reports the terminal outcome of one planned action. The
// engine reads these from the completions channel, classifies them, and calls
// DepGraph.Complete. Failed items become retry_work, block_scopes, or other
// engine-owned durable control state through the persistence flow.
type ActionCompletion struct {
	Path       string
	OldPath    string
	ItemID     string
	DriveID    driveid.ID
	ActionType ActionType
	// ItemType is the kind of item the action touched, as its planned view
	// says (ActionItemType): what the activity feed tells a summary.
	ItemType          ItemType
	Success           bool
	ErrMsg            string
	HTTPStatus        int // from graph.GraphError, 0 if not a Graph API error
	FailurePath       string
	FailureCapability PermissionCapability

	// Size is the transferred file size for display, when the outcome knows it.
	Size      int64
	SizeKnown bool

	// Err is the full error for classification (os.ErrPermission, a Graph
	// answer, a timed-out request and so on).
	Err error

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

// ServerAnswer is what this completion's Graph failure means (see
// classifyServerAnswer). It is derived, not stored, so it can never disagree
// with HTTPStatus and Err.
func (r *ActionCompletion) ServerAnswer() ServerAnswer {
	if r == nil {
		return ServerAnswerNone
	}

	return classifyServerAnswer(r.HTTPStatus, r.Err)
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

// ActionCompletionFromTrackedAction builds the shared runtime result shape for
// one exact tracked action. Workers and the engine-owned publication-drain stage both
// use this helper so classification/persistence sees one consistent exact
// action identity.
func ActionCompletionFromTrackedAction(
	ta *TrackedAction,
	outcome *ActionOutcome,
	actionErr error,
) ActionCompletion {
	driveID := ta.Action.DriveID
	if outcome != nil && !outcome.DriveID.IsZero() {
		driveID = outcome.DriveID
	}

	r := ActionCompletion{
		Path:                 ta.Action.Path,
		OldPath:              ta.Action.OldPath,
		ItemID:               ta.Action.ItemID,
		DriveID:              driveID,
		ActionType:           ta.Action.Type,
		ItemType:             ActionItemType(&ta.Action),
		Err:                  actionErr,
		ErrMsg:               "",
		HTTPStatus:           ExtractHTTPStatus(actionErr),
		RetryAfter:           extractRetryAfter(actionErr),
		IsTrial:              ta.IsTrial,
		TrialScopeKey:        ta.TrialScopeKey,
		TrialScopeGeneration: ta.TrialScopeGeneration,
		ActionID:             ta.ID,
	}

	if outcome != nil {
		r.Success = outcome.Success
		r.FailurePath = outcome.FailurePath
		r.FailureCapability = outcome.FailureCapability
		r.Size, r.SizeKnown = outcomeDisplaySize(outcome)
		if outcome.Error != nil {
			r.ErrMsg = outcome.Error.Error()
			r.Err = outcome.Error
			r.HTTPStatus = ExtractHTTPStatus(outcome.Error)
			r.RetryAfter = extractRetryAfter(outcome.Error)
		}
	} else if actionErr != nil {
		r.ErrMsg = actionErr.Error()
	}

	return r
}

// outcomeDisplaySize picks the size a user would associate with the action:
// what arrived for a download, what was sent for an upload.
func outcomeDisplaySize(outcome *ActionOutcome) (int64, bool) {
	switch outcome.Action {
	case ActionDownload:
		return outcome.RemoteSize, outcome.RemoteSizeKnown
	case ActionUpload:
		return outcome.LocalSize, outcome.LocalSizeKnown
	case ActionLocalDelete, ActionRemoteDelete, ActionLocalMove, ActionRemoteMove,
		ActionFolderCreate, ActionConflictCopy, ActionBaselineUpdate, ActionCleanup:
		return 0, false
	default:
		panic(OutsideTheEnum(outcome.Action))
	}
}
