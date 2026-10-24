package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
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
	Success   bool
	// Planned is true for a dry run: the action would happen but did not.
	Planned bool
	// WillRetry is true for failures the engine keeps as retry work; the user
	// does not need to act on them yet.
	WillRetry bool
	Err       error
}

// SetActivityHook registers a callback for finished file actions. The hook
// runs on the engine's completion path, so it must not block.
func (e *Engine) SetActivityHook(hook func(Activity)) {
	e.activityHook = hook
}

func activityKindFor(t actionType) (ActivityKind, bool) {
	switch t {
	case ActionDownload:
		return ActivityDownload, true
	case ActionUpload:
		return ActivityUpload, true
	case ActionLocalDelete:
		return ActivityLocalDelete, true
	case ActionRemoteDelete:
		return ActivityRemoteDelete, true
	case ActionLocalMove:
		return ActivityLocalMove, true
	case ActionRemoteMove:
		return ActivityRemoteMove, true
	case ActionFolderCreate:
		return ActivityFolderCreate, true
	case ActionConflictCopy:
		return ActivityConflictCopy, true
	case ActionBaselineUpdate, ActionCleanup:
		return 0, false
	}

	return 0, false
}

// emitActivity publishes a classified completion. Shutdown and superseded
// completions are not outcomes the user should see: the work either stopped
// on request or was replaced by newer truth.
func (e *Engine) emitActivity(r *actionCompletion, class errclass.Class) {
	if e.activityHook == nil || r == nil {
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
	}

	switch class {
	case errclass.ClassSuccess:
		activity.Success = true
	case errclass.ClassRetryableTransient, errclass.ClassScopeBlockingTransient:
		activity.WillRetry = true
		activity.Err = r.Err
	case errclass.ClassActionable, errclass.ClassFatal, errclass.ClassInvalid:
		activity.Err = r.Err
	case errclass.ClassShutdown, errclass.ClassSuperseded:
		return
	}

	e.activityHook(activity)
}

// emitPlannedActivity lists what a dry run would do, one activity per
// user-visible planned action.
func (e *Engine) emitPlannedActivity(plan *actionPlan) {
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
			Planned: true,
		})
	}
}
