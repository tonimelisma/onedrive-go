package syncdomain

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

// ErrActionPreconditionChanged is returned when executor-time revalidation
// proves the planner's item-level preconditions no longer hold.
var ErrActionPreconditionChanged = errors.New("file changed while it was being synced")

// ---------------------------------------------------------------------------
// Observer errors
// ---------------------------------------------------------------------------

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

// CancelCauseError is the cause each cancel the worker pool and the one-shot
// pass make carries (syncexec and the engine each declare their own), and it
// answers to context.Canceled as the cause of signal.NotifyContext does. A
// request interrupted by a cancel carries the cancel's cause in place of
// context.Canceled, so a cause that did not answer to context.Canceled would
// make the interrupted work read as a failure; the cause's text says which
// cancel it was.
type CancelCauseError string

func (e CancelCauseError) Error() string { return string(e) }

// Is answers true for context.Canceled and for nothing else.
func (CancelCauseError) Is(target error) bool { return target == context.Canceled }
