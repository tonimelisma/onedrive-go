package sync

import (
	"context"
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Sentinel texts are user words without a package prefix: the sync console
// prints the innermost error of a chain as a failure's reason, and wrappers
// already add the "sync:" context.

// ---------------------------------------------------------------------------
// Executor errors
// ---------------------------------------------------------------------------

// ErrPathEscapesSyncRoot is returned when a relative path would resolve
// outside the sync root directory (path traversal attack prevention).
var ErrPathEscapesSyncRoot = errors.New("path leaves the sync folder")

// ErrActionPreconditionChanged is returned when executor-time revalidation
// proves the planner's item-level preconditions no longer hold.
var ErrActionPreconditionChanged = errors.New("file changed while it was being synced")

// ---------------------------------------------------------------------------
// Observer errors
// ---------------------------------------------------------------------------

// ErrSyncRootDeleted is returned when the sync root directory has been deleted
// or become inaccessible while a watch was running.
var ErrSyncRootDeleted = errors.New("it was deleted or can't be read")

// ErrLocalWatchUnavailable is matched (through localWatchUnavailableError)
// when the operating system cannot give watch mode the watches it needs: the
// inotify watch or instance limit, the open-file limit, kernel memory, or the
// kqueue descriptor budget. The engine falls back to periodic full scans.
var ErrLocalWatchUnavailable = errors.New("local change watching unavailable")

// ErrDeltaExpired indicates the saved delta token has expired and a full
// resync is required. Returned when the Graph API responds with HTTP 410.
var ErrDeltaExpired = errors.New("OneDrive change history expired")

// ResyncKind is the recovery OneDrive asks for with a 410 (R-2.15.3). It
// decides what an item the following full enumeration no longer returns
// means.
type ResyncKind int

const (
	// ResyncKindUnknown is a 410 without a recognized flavor. It is
	// treated like ResyncKindUploadDifferences: when unsure, never delete.
	ResyncKindUnknown ResyncKind = iota
	// ResyncKindApplyDifferences: the server's view is current; an item it
	// no longer returns was deleted, and the local copy may follow.
	ResyncKindApplyDifferences
	// ResyncKindUploadDifferences: the server lost changes; local items it
	// did not return are to be uploaded again, never deleted.
	ResyncKindUploadDifferences
)

// Graph's inner codes for the two resync flavors.
const (
	graphCodeResyncApplyDifferences  = "resyncChangesApplyDifferences"
	graphCodeResyncUploadDifferences = "resyncChangesUploadDifferences"
)

// OrphansAreDeletes reports whether the enumeration that follows this resync
// may turn a confirmed-missing synced item into a delete.
func (k ResyncKind) OrphansAreDeletes() bool {
	return k == ResyncKindApplyDifferences
}

// DeltaExpiredError is ErrDeltaExpired with the resync flavor the 410 named.
// It also carries this client's own "cursor unusable" decision (R-2.15.2): a
// change list that loops or a token OneDrive keeps refusing is recovered
// exactly like a 410, and reason then says why.
type DeltaExpiredError struct {
	kind   ResyncKind
	cause  error
	reason string
}

// NewCursorUnusableError is the recovery this client decides on for a delta
// cursor it cannot continue. It is a full refresh like any other the client
// starts, so confirmed orphans are deletes (subject to the delete-safety
// hold).
func NewCursorUnusableError(reason string, cause error) *DeltaExpiredError {
	return &DeltaExpiredError{kind: ResyncKindApplyDifferences, cause: cause, reason: reason}
}

func NewDeltaExpiredError(cause error) *DeltaExpiredError {
	kind := ResyncKindUnknown
	var graphErr *graph.GraphError
	if errors.As(cause, &graphErr) {
		switch {
		case graphErr.HasCode(graphCodeResyncApplyDifferences):
			kind = ResyncKindApplyDifferences
		case graphErr.HasCode(graphCodeResyncUploadDifferences):
			kind = ResyncKindUploadDifferences
		}
	}

	return &DeltaExpiredError{kind: kind, cause: cause}
}

func (e *DeltaExpiredError) Error() string {
	if e.reason != "" {
		return "OneDrive change list cannot be continued: " + e.reason
	}

	return ErrDeltaExpired.Error()
}

// Reason says why a cursor this client gave up on cannot be continued;
// empty for an expiry Graph reported.
func (e *DeltaExpiredError) Reason() string { return e.reason }

func (e *DeltaExpiredError) Is(target error) bool { return target == ErrDeltaExpired }

func (e *DeltaExpiredError) Unwrap() error { return e.cause }

// ResyncKindOf returns the flavor of an expired-delta error, or
// ResyncKindUnknown when err does not carry one.
func ResyncKindOf(err error) ResyncKind {
	var expired *DeltaExpiredError
	if errors.As(err, &expired) {
		return expired.kind
	}

	return ResyncKindUnknown
}

// ErrRemoteObservationIncomplete is returned when remote observation cannot
// materialize a provider item safely enough to advance the delta cursor.
var ErrRemoteObservationIncomplete = errors.New("OneDrive change list was incomplete")

// ErrMountRootUnavailable is returned when the sync root for a mounted content
// root is missing or inaccessible. Runtimes treat this as mount lifecycle, not
// a request to delete remote content below that root.
var ErrMountRootUnavailable = errors.New("sync folder unavailable")

// ---------------------------------------------------------------------------
// Scanner errors
// ---------------------------------------------------------------------------

// ErrSyncRootMissing is returned when the sync root directory does not exist
// or is not a directory. Callers can match with errors.Is.
var ErrSyncRootMissing = errors.New("sync folder does not exist")

// ErrFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var ErrFileChangedDuringHash = errors.New("file changed while it was being hashed")

// ---------------------------------------------------------------------------
// Planner errors
// ---------------------------------------------------------------------------

// ErrDependencyCycle indicates that the action plan contains a dependency
// cycle, making topological ordering impossible. This is a planner bug —
// well-formed sync actions should always form a DAG (B-313).
var ErrDependencyCycle = errors.New("dependency cycle in the action plan")

// ---------------------------------------------------------------------------
// Store errors
// ---------------------------------------------------------------------------

// ErrStoreAccessTimedOut is the cause a detached store access's bound expires
// with (RunDetachedStoreAccess), joined after the access's own error. The
// expired bound carries context.DeadlineExceeded as a request that timed out
// does, but it is the engine's own limit on its database: a store failure,
// never a request OneDrive left unanswered.
var ErrStoreAccessTimedOut = errors.New("the sync database did not answer in time")

// ---------------------------------------------------------------------------
// The end of a run
// ---------------------------------------------------------------------------

// RunEnded reports whether work that failed with err failed because its run
// is ending: exactly when ctx has ended. The caller passes the context the
// work was started under (for a worker's completion, the context the workers
// were started under), which ends only with the run or once nothing is left
// to classify, never a request's own: a request's deadline ends while the run
// goes on. The answer never depends on what err says. A request the transport
// timed out carries context.DeadlineExceeded while the run goes on, and that
// is a failure to retry, not a stop; err is there only so that a nil error,
// which is no failure, never reads as a stop.
func RunEnded(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() != nil
}

// IsCancellationEcho reports whether err is what a canceled or expired
// context hands back to the work it interrupted. A caller that already knows
// the run has ended (RunEnded) uses it to tell that echo of the stop from a
// separate failure that landed during it, which is still reported.
func IsCancellationEcho(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cancelCauseError is the cause each cancel the worker pool and the one-shot
// pass make carries, and it answers to context.Canceled as the cause of
// signal.NotifyContext does. A
// request interrupted by a cancel carries the cancel's cause in place of
// context.Canceled, so a cause that did not answer to context.Canceled would
// make the interrupted work read as a failure; the cause's text says which
// cancel it was.
type cancelCauseError string

func (e cancelCauseError) Error() string { return string(e) }

// Is answers true for context.Canceled and for nothing else.
func (cancelCauseError) Is(target error) bool { return target == context.Canceled }

// The causes of a one-shot pass context's cancel: a completion that failed
// the pass (which RunOnce then returns), or the pass's end.
//
//nolint:gosec // G101 reads "Pass" in the names as a password; these are cancel causes, not credentials
const (
	ErrOneShotPassFailed cancelCauseError = "sync: one-shot pass failed"
	ErrOneShotPassEnded  cancelCauseError = "sync: one-shot pass ended"
)

// The causes of the worker contexts' cancels. Neither reaches an action while
// its run goes on: ErrActionReturned lands after the action returned, and
// ErrWorkerPoolStopped ends the context the workers were started under, so an
// action it interrupts completes with RunEnded set, as a shutdown.
const (
	ErrActionReturned    cancelCauseError = "sync: action returned"
	ErrWorkerPoolStopped cancelCauseError = "sync: worker pool stopped"
)
