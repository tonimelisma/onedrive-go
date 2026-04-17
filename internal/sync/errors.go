package sync

import "errors"

// ---------------------------------------------------------------------------
// Executor errors
// ---------------------------------------------------------------------------

// ErrPathEscapesSyncRoot is returned when a relative path would resolve
// outside the sync root directory (path traversal attack prevention).
var ErrPathEscapesSyncRoot = errors.New("sync: path escapes sync root")

// ErrActionPreconditionChanged is returned when executor-time revalidation
// proves the planner's item-level preconditions no longer hold.
var ErrActionPreconditionChanged = errors.New("sync: action precondition changed")

// ---------------------------------------------------------------------------
// Observer errors
// ---------------------------------------------------------------------------

// ErrSyncRootDeleted is returned when the sync root directory has been deleted
// or become inaccessible while a watch was running.
var ErrSyncRootDeleted = errors.New("sync: sync root directory deleted or inaccessible")

// ErrWatchLimitExhausted is returned when the inotify watch limit is
// exhausted (Linux ENOSPC). The engine falls back to periodic full scans.
var ErrWatchLimitExhausted = errors.New("sync: inotify watch limit exhausted")

// ErrDeltaExpired indicates the saved delta token has expired and a full
// resync is required. Returned when the Graph API responds with HTTP 410.
var ErrDeltaExpired = errors.New("sync: delta token expired (resync required)")

// ---------------------------------------------------------------------------
// Scanner errors
// ---------------------------------------------------------------------------

// ErrSyncRootMissing is returned when the sync root directory does not exist
// or is not a directory. Callers can match with errors.Is.
var ErrSyncRootMissing = errors.New("sync: sync root directory does not exist")

// ErrNosyncGuard is returned when a .nosync guard file is present in the
// sync root, indicating the sync directory may be unmounted or guarded.
var ErrNosyncGuard = errors.New("sync: .nosync guard file present (sync dir may be unmounted)")

// ErrFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var ErrFileChangedDuringHash = errors.New("sync: file changed during hashing")

// ---------------------------------------------------------------------------
// Planner errors
// ---------------------------------------------------------------------------

// ErrDependencyCycle indicates that the action plan contains a dependency
// cycle, making topological ordering impossible. This is a planner bug —
// well-formed sync actions should always form a DAG (B-313).
var ErrDependencyCycle = errors.New("sync: dependency cycle detected in action plan")
