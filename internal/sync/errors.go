package sync

import "errors"

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

// errWatchLimitExhausted is returned when the inotify watch limit is
// exhausted (Linux ENOSPC). The engine falls back to periodic full scans.
var errWatchLimitExhausted = errors.New("inotify watch limit reached")

// errDeltaExpired indicates the saved delta token has expired and a full
// resync is required. Returned when the Graph API responds with HTTP 410.
var errDeltaExpired = errors.New("OneDrive change history expired")

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
