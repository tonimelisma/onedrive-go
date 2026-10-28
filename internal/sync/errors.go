package sync

import (
	"errors"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Sentinel texts are user words without a package prefix: the sync console
// prints the innermost error of a chain as a failure's reason, and wrappers
// already add the "sync:" context.

// ---------------------------------------------------------------------------
// Executor errors
// ---------------------------------------------------------------------------

// errPathEscapesSyncRoot is returned when a relative path would resolve
// outside the sync root directory (path traversal attack prevention).
var errPathEscapesSyncRoot = errors.New("path leaves the sync folder")

// errActionPreconditionChanged is returned when executor-time revalidation
// proves the planner's item-level preconditions no longer hold.
var errActionPreconditionChanged = errors.New("file changed while it was being synced")

// ---------------------------------------------------------------------------
// Observer errors
// ---------------------------------------------------------------------------

// errSyncRootDeleted is returned when the sync root directory has been deleted
// or become inaccessible while a watch was running.
var errSyncRootDeleted = errors.New("it was deleted or can't be read")

// errLocalWatchUnavailable is matched (through localWatchUnavailableError)
// when the operating system cannot give watch mode the watches it needs: the
// inotify watch or instance limit, the open-file limit, kernel memory, or the
// kqueue descriptor budget. The engine falls back to periodic full scans.
var errLocalWatchUnavailable = errors.New("local change watching unavailable")

// errDeltaExpired indicates the saved delta token has expired and a full
// resync is required. Returned when the Graph API responds with HTTP 410.
var errDeltaExpired = errors.New("OneDrive change history expired")

// resyncKind is the recovery OneDrive asks for with a 410 (R-2.15.3). It
// decides what an item the following full enumeration no longer returns
// means.
type resyncKind int

const (
	// resyncKindUnknown is a 410 without a recognized flavor. It is
	// treated like resyncKindUploadDifferences: when unsure, never delete.
	resyncKindUnknown resyncKind = iota
	// resyncKindApplyDifferences: the server's view is current; an item it
	// no longer returns was deleted, and the local copy may follow.
	resyncKindApplyDifferences
	// resyncKindUploadDifferences: the server lost changes; local items it
	// did not return are to be uploaded again, never deleted.
	resyncKindUploadDifferences
)

// Graph's inner codes for the two resync flavors.
const (
	graphCodeResyncApplyDifferences  = "resyncChangesApplyDifferences"
	graphCodeResyncUploadDifferences = "resyncChangesUploadDifferences"
)

// orphansAreDeletes reports whether the enumeration that follows this resync
// may turn a confirmed-missing synced item into a delete.
func (k resyncKind) orphansAreDeletes() bool {
	return k == resyncKindApplyDifferences
}

// deltaExpiredError is errDeltaExpired with the resync flavor the 410 named.
// It also carries this client's own "cursor unusable" decision (R-2.15.2): a
// change list that loops or a token OneDrive keeps refusing is recovered
// exactly like a 410, and reason then says why.
type deltaExpiredError struct {
	kind   resyncKind
	cause  error
	reason string
}

// newCursorUnusableError is the recovery this client decides on for a delta
// cursor it cannot continue. It is a full refresh like any other the client
// starts, so confirmed orphans are deletes (subject to the delete-safety
// hold).
func newCursorUnusableError(reason string, cause error) *deltaExpiredError {
	return &deltaExpiredError{kind: resyncKindApplyDifferences, cause: cause, reason: reason}
}

func newDeltaExpiredError(cause error) *deltaExpiredError {
	kind := resyncKindUnknown
	var graphErr *graph.GraphError
	if errors.As(cause, &graphErr) {
		switch {
		case graphErr.HasCode(graphCodeResyncApplyDifferences):
			kind = resyncKindApplyDifferences
		case graphErr.HasCode(graphCodeResyncUploadDifferences):
			kind = resyncKindUploadDifferences
		}
	}

	return &deltaExpiredError{kind: kind, cause: cause}
}

func (e *deltaExpiredError) Error() string {
	if e.reason != "" {
		return "OneDrive change list cannot be continued: " + e.reason
	}

	return errDeltaExpired.Error()
}

func (e *deltaExpiredError) Is(target error) bool { return target == errDeltaExpired }

func (e *deltaExpiredError) Unwrap() error { return e.cause }

// resyncKindOf returns the flavor of an expired-delta error, or
// resyncKindUnknown when err does not carry one.
func resyncKindOf(err error) resyncKind {
	var expired *deltaExpiredError
	if errors.As(err, &expired) {
		return expired.kind
	}

	return resyncKindUnknown
}

// errRemoteObservationIncomplete is returned when remote observation cannot
// materialize a provider item safely enough to advance the delta cursor.
var errRemoteObservationIncomplete = errors.New("OneDrive change list was incomplete")

// ErrMountRootUnavailable is returned when the sync root for a mounted content
// root is missing or inaccessible. Runtimes treat this as mount lifecycle, not
// a request to delete remote content below that root.
var ErrMountRootUnavailable = errors.New("sync folder unavailable")

// ---------------------------------------------------------------------------
// Scanner errors
// ---------------------------------------------------------------------------

// errSyncRootMissing is returned when the sync root directory does not exist
// or is not a directory. Callers can match with errors.Is.
var errSyncRootMissing = errors.New("sync folder does not exist")

// errFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var errFileChangedDuringHash = errors.New("sync: file changed during hashing")

// ---------------------------------------------------------------------------
// Planner errors
// ---------------------------------------------------------------------------

// errDependencyCycle indicates that the action plan contains a dependency
// cycle, making topological ordering impossible. This is a planner bug —
// well-formed sync actions should always form a DAG (B-313).
var errDependencyCycle = errors.New("sync: dependency cycle detected in action plan")
