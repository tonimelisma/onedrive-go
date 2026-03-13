package sync

import "errors"

// ---------------------------------------------------------------------------
// Executor errors
// ---------------------------------------------------------------------------

// ErrPathEscapesSyncRoot is returned when a relative path would resolve
// outside the sync root directory (path traversal attack prevention).
var ErrPathEscapesSyncRoot = errors.New("sync: path escapes sync root")

// ErrDiskFull is returned when available disk space is below the configured
// min_free_space threshold. Triggers a disk:local scope block (R-2.10.43).
var ErrDiskFull = errors.New("sync: local disk full")

// ErrFileTooLargeForSpace is returned when available disk space is above
// min_free_space but below file size + min_free_space. Per-file failure,
// no scope escalation — smaller files may still fit (R-2.10.44).
var ErrFileTooLargeForSpace = errors.New("sync: insufficient space for file")

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

// errFileChangedDuringHash is returned when a file's metadata changes between
// pre-hash stat and post-hash stat, indicating active writing (B-119).
var errFileChangedDuringHash = errors.New("sync: file changed during hashing")

// ---------------------------------------------------------------------------
// Planner errors
// ---------------------------------------------------------------------------

// ErrBigDeleteTriggered indicates that the planned number of deletions
// exceeds safety thresholds. The sync pass should halt and require
// user confirmation before proceeding.
var ErrBigDeleteTriggered = errors.New("sync: big-delete protection triggered")

// ErrDependencyCycle indicates that the action plan contains a dependency
// cycle, making topological ordering impossible. This is a planner bug —
// well-formed sync actions should always form a DAG (B-313).
var ErrDependencyCycle = errors.New("sync: dependency cycle detected in action plan")
