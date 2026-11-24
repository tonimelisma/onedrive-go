package syncstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// quickCheckOK is the single row PRAGMA quick_check returns for a sound DB.
const quickCheckOK = "ok"

// quickCheckMaxFindings bounds how many quick_check findings are kept in the
// error cause; a badly damaged file can report thousands.
const quickCheckMaxFindings = 5

type stateStoreIncompatibleReason string

const (
	StateStoreIncompatibleReasonOpenFailed         stateStoreIncompatibleReason = "open_failed"
	StateStoreIncompatibleReasonIncompatibleSchema stateStoreIncompatibleReason = "incompatible_schema"
	StateStoreIncompatibleReasonNewerVersion       stateStoreIncompatibleReason = "newer_version"
	StateStoreIncompatibleReasonCorrupt            stateStoreIncompatibleReason = "corrupt"
)

var errStateStoreIncompatible = errors.New("syncstore: incompatible sync state DB")

type StateStoreIncompatibleError struct {
	Reason stateStoreIncompatibleReason
	Cause  error
}

// Error is one plain sentence shown at default verbosity. The underlying
// cause (SQLite or schema detail) is reachable through Unwrap for --verbose
// and the log file, not embedded here.
func (e *StateStoreIncompatibleError) Error() string {
	if e == nil {
		return ""
	}

	return "the saved sync state for this drive can't be used: " + e.Reason.Description()
}

func (e *StateStoreIncompatibleError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Cause
}

func (e *StateStoreIncompatibleError) Is(target error) bool {
	return target == errStateStoreIncompatible
}

func IsStateStoreIncompatible(err error) bool {
	return errors.Is(err, errStateStoreIncompatible)
}

func (r stateStoreIncompatibleReason) Description() string {
	switch r {
	case StateStoreIncompatibleReasonOpenFailed:
		return "its database could not be opened"
	case StateStoreIncompatibleReasonIncompatibleSchema:
		return "it was written by an incompatible version of onedrive-go"
	case StateStoreIncompatibleReasonNewerVersion:
		return "it was written by a newer version of onedrive-go"
	case StateStoreIncompatibleReasonCorrupt:
		return "its database file is damaged"
	default:
		return "its format is not supported"
	}
}

// OpenEngineSyncStore is the engine-owned compatibility boundary for existing
// state DBs. A missing DB bootstraps the canonical schema. A DB an older
// onedrive-go wrote (its generation marker names an older generation) is
// rebuilt: its file family is deleted and a fresh canonical store created in
// its place, as `drive reset-sync-state` does (R-2.5.9). State is a cache of
// local and remote truth, and a store without a baseline never plans a
// delete. Every other unusable DB is left as it is and surfaces a typed
// store-incompatible error: one a newer version wrote (rebuilding it would
// throw that version's state away each time an older binary started), one
// without a readable marker or with another shape at the current generation
// (nothing says which version wrote it), and a damaged or unreadable one.
//
// The caller holds the engine's store lock (acquireEngineLocks in package
// sync), so no other engine or offline change has the store open while it is
// rebuilt.
//
// now is the engine's run clock, which the store dates its delete-ledger
// rows, retry timing and pending-create intents by, so a retry the store
// schedules is judged due by the same clock; nil is the wall clock. hooks are
// the store's test seams; production passes the zero value.
func OpenEngineSyncStore(
	ctx context.Context, dbPath string, logger *slog.Logger, now func() time.Time, hooks Hooks,
) (*SyncStore, error) {
	store, err := openSyncStoreWithHooks(ctx, dbPath, logger, true, now, hooks)
	var generationErr *storeGenerationError
	if errors.As(err, &generationErr) && generationErr.IsOlder() {
		// A delete that fails is a filesystem failure, reported as such: it is
		// not a store a reset would fix, and the next start tries again.
		if rebuildErr := rebuildOlderGenerationStore(dbPath, logger, generationErr, hooks); rebuildErr != nil {
			return nil, rebuildErr
		}
		store, err = openSyncStoreWithHooks(ctx, dbPath, logger, true, now, hooks)
	}
	if err == nil {
		checkErr := checkStoreIntegrity(ctx, store)
		if checkErr == nil {
			return store, nil
		}
		if closeErr := store.Close(ctx); closeErr != nil {
			logger.Warn("close sync store after a failed integrity check",
				slog.String("db_path", dbPath), slog.Any("error", closeErr))
		}
		failure := integrityCheckFailure(checkErr)
		if IsStateStoreIncompatible(failure) {
			logger.Error("sync state DB failed its integrity check",
				slog.String("db_path", dbPath), slog.Any("error", checkErr))
		}

		return nil, failure
	}
	// Only an existing store is incompatible. One that cannot be checked is
	// reported as the failure it is, never steered towards a reset.
	exists, existsErr := StateDBFamilyExists(dbPath)
	if existsErr != nil {
		return nil, errors.Join(err, existsErr)
	}
	if !exists {
		return nil, err
	}

	reason := StateStoreIncompatibleReasonOpenFailed
	switch {
	case errors.As(err, &generationErr) && !generationErr.IsOlder():
		reason = StateStoreIncompatibleReasonNewerVersion
	case errors.Is(err, errIncompatibleSchema):
		reason = StateStoreIncompatibleReasonIncompatibleSchema
	case isSQLiteCorruption(err):
		reason = StateStoreIncompatibleReasonCorrupt
	}

	return nil, &StateStoreIncompatibleError{
		Reason: reason,
		Cause:  err,
	}
}

// rebuildOlderGenerationStore deletes the file family of a store an older
// onedrive-go wrote, so the next open bootstraps a fresh canonical store at
// its path. The delete takes the database last (RemoveStateDBFiles): one cut
// short leaves the older store in place, and the next start rebuilds it again.
func rebuildOlderGenerationStore(
	dbPath string, logger *slog.Logger, generationErr *storeGenerationError, hooks Hooks,
) error {
	logger.Warn("rebuilding sync state written by an older onedrive-go version; "+
		"the next sync compares local and remote files afresh and deletes nothing",
		slog.String("db_path", dbPath),
		slog.Int("stored_generation", generationErr.stored),
		slog.Int("current_generation", generationErr.current),
	)
	if err := hooks.removeStoreFiles(dbPath); err != nil {
		return fmt.Errorf("rebuild sync store from an older version: %w", err)
	}

	return nil
}

// StateDBFamilyExists reports whether any file of the store at dbPath is on
// disk (config.StateDBFamily). A file that cannot be checked (permission denied, an
// I/O error) is an error, not absent: read as absent, an email change would
// rename a drive's config away from a store it could not see.
func StateDBFamilyExists(dbPath string) (bool, error) {
	for _, candidate := range config.StateDBFamily(dbPath) {
		_, err := localpath.Stat(candidate)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, os.ErrNotExist):
			return false, fmt.Errorf("check state DB file: %w", err)
		}
	}

	return false, nil
}

// checkStoreIntegrity runs PRAGMA quick_check on an open store (R-2.5.8). A
// damaged b-tree can hide rows, and a missing remote_state row reads as a
// remote delete, so the engine must refuse the DB before planning from it.
// quick_check skips the index-to-table cross-check of integrity_check, which
// keeps it linear in the DB size and cheap enough for every startup.
func checkStoreIntegrity(ctx context.Context, store *SyncStore) error {
	rows, err := store.db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("run sync store quick_check: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var findings []string
	for rows.Next() {
		var finding string
		if scanErr := rows.Scan(&finding); scanErr != nil {
			return fmt.Errorf("read sync store quick_check result: %w", scanErr)
		}
		if finding == quickCheckOK {
			continue
		}
		if len(findings) < quickCheckMaxFindings {
			findings = append(findings, finding)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read sync store quick_check result: %w", err)
	}
	if len(findings) > 0 {
		return &quickCheckFindingsError{findings: findings}
	}

	return nil
}

// quickCheckFindingsError is quick_check reporting damage: the only check
// result, besides SQLITE_CORRUPT, that means the file is damaged.
type quickCheckFindingsError struct {
	findings []string
}

func (e *quickCheckFindingsError) Error() string {
	return "sync store quick_check: " + strings.Join(e.findings, "; ")
}

// integrityCheckFailure maps a failed integrity check to what startup
// reports. Damage (quick_check findings, SQLITE_CORRUPT) is the corrupt
// store-incompatible error with its reset guidance. Anything else -- a
// canceled context, SQLITE_BUSY, an I/O or scan error -- says nothing about
// the file and is returned as an ordinary failure, so a transient problem
// never steers the user to delete their sync state.
func integrityCheckFailure(err error) error {
	var findings *quickCheckFindingsError
	if errors.As(err, &findings) || isSQLiteCorruption(err) {
		return &StateStoreIncompatibleError{Reason: StateStoreIncompatibleReasonCorrupt, Cause: err}
	}

	return fmt.Errorf("check the sync state DB: %w", err)
}

// isSQLiteCorruption reports whether err carries SQLITE_CORRUPT, which SQLite
// can raise while the schema is read at open, before quick_check runs.
func isSQLiteCorruption(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&sqlitePrimaryResultMask == sqlite3.SQLITE_CORRUPT
}
