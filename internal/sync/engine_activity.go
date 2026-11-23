package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ActivityKind names a user-visible file action. It deliberately omits the
// bookkeeping action types (baseline updates, cleanups) that change nothing a
// user would recognize.
type ActivityKind int

const (
	ActivityDownload ActivityKind = iota + 1
	ActivityUpload
	ActivityLocalDelete
	ActivityRemoteDelete
	ActivityLocalMove
	ActivityRemoteMove
	ActivityFolderCreate
	ActivityConflictCopy
)

// Activity reports that one user-visible file action finished. Paths are
// relative to the mount's sync root.
type Activity struct {
	At        time.Time
	Kind      ActivityKind
	Path      string
	OldPath   string
	Size      int64
	SizeKnown bool
	// Folder is whether the item the action touched is a folder, as the
	// planned action's view says (syncdomain.ResolveItemType), so a summary
	// counts a deleted folder as a folder rather than as a file.
	Folder  bool
	Success bool
	// Planned is true for a dry run: the action would happen but did not.
	Planned bool
	// WillRetry is true for failures the engine keeps as retry work; the user
	// does not need to act on them yet.
	WillRetry bool
	// ConditionStarts is set with ConditionKey when the condition starts with
	// this failure: the action's retry work did not hold that condition yet.
	// A retry the same condition refuses again leaves it unset, and so does
	// every activity that names no condition.
	ConditionStarts bool
	// ConditionKey and ConditionType name the condition only the user can
	// clear that refused the action, as `status` lists it
	// (StoredConditionGroup): the family whose words status uses, and the
	// issue type `status --json` prints. They are set only for a failure
	// recorded with that condition (failureAwaitsUser), which the run counts
	// with what is still not synced rather than as failed.
	ConditionKey  syncdomain.ConditionKey
	ConditionType syncdomain.IssueType
	Err           error
}

// SetActivityHook registers a callback for finished file actions. The hook
// runs on the engine's completion path, so it must not block.
func (e *Engine) SetActivityHook(hook func(Activity)) {
	e.activityHook = hook
}

func activityKindFor(t syncdomain.ActionType) (ActivityKind, bool) {
	switch t {
	case syncdomain.ActionDownload:
		return ActivityDownload, true
	case syncdomain.ActionUpload:
		return ActivityUpload, true
	case syncdomain.ActionLocalDelete:
		return ActivityLocalDelete, true
	case syncdomain.ActionRemoteDelete:
		return ActivityRemoteDelete, true
	case syncdomain.ActionLocalMove:
		return ActivityLocalMove, true
	case syncdomain.ActionRemoteMove:
		return ActivityRemoteMove, true
	case syncdomain.ActionFolderCreate:
		return ActivityFolderCreate, true
	case syncdomain.ActionConflictCopy:
		return ActivityConflictCopy, true
	case syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return 0, false
	default:
		panic(syncdomain.OutsideTheEnum(t))
	}
}

// emitActivity publishes a classified completion. Shutdown and superseded
// completions are not outcomes the user should see: the work either stopped
// on request or was replaced by newer truth. conditionStarts is whether the
// condition the failure is refused for starts with it (conditionStartsWith);
// the activity carries it with that condition.
func (e *Engine) emitActivity(r *syncdomain.ActionCompletion, decision *syncdomain.ResultDecision, conditionStarts bool) {
	if e.activityHook == nil || r == nil || decision == nil {
		return
	}

	kind, ok := activityKindFor(r.ActionType)
	if !ok {
		return
	}

	activity := Activity{
		At:        e.nowFunc(),
		Kind:      kind,
		Path:      r.Path,
		OldPath:   r.OldPath,
		Size:      r.Size,
		SizeKnown: r.SizeKnown,
		Folder:    r.ItemType == syncdomain.ItemTypeFolder,
	}

	switch decision.Class {
	case errclass.ClassSuccess:
		activity.Success = true
	case errclass.ClassRetryableTransient, errclass.ClassScopeBlockingTransient,
		errclass.ClassActionable, errclass.ClassFatal, errclass.ClassInvalid:
		activity.WillRetry = failureKeptForRetry(decision.Class)
		activity.Err = r.Err
		if failureAwaitsUser(decision, r) {
			activity.ConditionKey = decision.ConditionKey
			activity.ConditionType = decision.ConditionType
			activity.ConditionStarts = conditionStarts
		}
	case errclass.ClassShutdown, errclass.ClassSuperseded:
		return
	}

	e.activityHook(activity)
}

// failureAwaitsUser reports whether a failure is a condition only the user can
// clear, recorded with its retry row under the condition the decision names,
// which `status` lists under that condition (recordRetryWork, R-2.10.71): an
// action refused per item for a named condition. A failure the permission
// probes own is not, because the condition and scope they record are known
// only after the probe ran and can differ from the decision's. Nor is a
// transfer OneDrive answered with other bytes than its own facts describe
// (an upload not stored as sent, a download that does not match): nothing the
// user does clears it, and its retry may succeed. The activity feed's
// condition and a pass's count of such failures (Report.LastPassAwaitingUser)
// both read it, so the two cannot disagree.
func failureAwaitsUser(decision *syncdomain.ResultDecision, r *syncdomain.ActionCompletion) bool {
	return decision.Class == errclass.ClassActionable && decision.Persistence == syncdomain.PersistRetryWork &&
		decision.ConditionType != "" && decision.ConditionKey != "" &&
		decision.ConditionKey != syncdomain.ConditionUploadVerification && decision.ConditionKey != syncdomain.ConditionDownloadIntegrity &&
		!shouldHandleRemote403Permission(r) && !shouldHandleLocalPermission(r)
}

// activitySaysConditionStarts reports whether the activity feed tells its
// consumer that the condition that refused this failure starts with it
// (failureAwaitsUser and conditionStarts): sync's output then says it to the
// user in the words `status` uses.
func (e *Engine) activitySaysConditionStarts(
	decision *syncdomain.ResultDecision, r *syncdomain.ActionCompletion, conditionStarts bool,
) bool {
	return conditionStarts && e.activityHook != nil && decision != nil && failureAwaitsUser(decision, r)
}

// conditionStartsWith reports whether the condition decision names starts
// with the failure r: the action's retry work does not hold that condition
// yet. A path whose retry work the plan dropped, with no activity of the path
// in between, starts it again. Read before the failure is recorded, the
// activity feed and the log line of the failure read one answer.
func (flow *engineFlow) conditionStartsWith(decision *syncdomain.ResultDecision, r *syncdomain.ActionCompletion) bool {
	row, held := flow.retryWork.row(syncdomain.RetryWorkKeyForCompletion(r))
	return !held || row.Blocked || row.ConditionType != decision.ConditionType
}

// failureKeptForRetry reports whether the engine keeps a failure of class as
// retry work it runs again by itself: a transient failure, or one a scope
// holds until its trial. The activity feed's WillRetry and a pass's count of
// failures kept for retry (Report.LastPassKeptForRetry) both read it, so the
// two cannot disagree.
func failureKeptForRetry(class errclass.Class) bool {
	switch class {
	case errclass.ClassRetryableTransient, errclass.ClassScopeBlockingTransient:
		return true
	case errclass.ClassInvalid, errclass.ClassSuccess, errclass.ClassShutdown, errclass.ClassSuperseded,
		errclass.ClassActionable, errclass.ClassFatal:
		return false
	}

	return false
}

// emitPlannedActivity lists what a dry run would do, one activity per
// user-visible planned action.
func (e *Engine) emitPlannedActivity(plan *syncdomain.ActionPlan) {
	if e.activityHook == nil || plan == nil {
		return
	}

	at := e.nowFunc()
	for i := range plan.Actions {
		action := &plan.Actions[i]
		kind, ok := activityKindFor(action.Type)
		if !ok {
			continue
		}
		e.activityHook(Activity{
			At:      at,
			Kind:    kind,
			Path:    action.Path,
			OldPath: action.OldPath,
			Folder:  syncdomain.ResolveItemType(action.View) == syncdomain.ItemTypeFolder,
			Planned: true,
		})
	}
}
