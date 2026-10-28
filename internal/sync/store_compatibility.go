package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// quickCheckOK is the single row PRAGMA quick_check returns for a sound DB.
const quickCheckOK = "ok"

// quickCheckMaxFindings bounds how many quick_check findings are kept in the
// error cause; a badly damaged file can report thousands.
const quickCheckMaxFindings = 5

type stateStoreIncompatibleReason string

const (
	stateStoreIncompatibleReasonOpenFailed         stateStoreIncompatibleReason = "open_failed"
	StateStoreIncompatibleReasonIncompatibleSchema stateStoreIncompatibleReason = "incompatible_schema"
	StateStoreIncompatibleReasonCorrupt            stateStoreIncompatibleReason = "corrupt"
)

var errStateStoreIncompatible = errors.New("sync: incompatible sync state DB")

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
	case stateStoreIncompatibleReasonOpenFailed:
		return "its database could not be opened"
	case StateStoreIncompatibleReasonIncompatibleSchema:
		return "it was written by an incompatible version of onedrive-go"
	case StateStoreIncompatibleReasonCorrupt:
		return "its database file is damaged"
	default:
		return "its format is not supported"
	}
}

// openEngineSyncStore is the engine-owned compatibility boundary for existing
// state DBs. It never mutates or recreates durable state during startup:
// missing DBs bootstrap normally via NewSyncStore, while unreadable or
// incompatible existing DBs surface a typed store-incompatible error.
func openEngineSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		checkErr := checkStoreIntegrity(ctx, store)
		if checkErr == nil {
			return store, nil
		}
		if closeErr := store.Close(ctx); closeErr != nil {
			logger.Warn("close sync store after a failed integrity check",
				slog.String("db_path", dbPath), slog.String("error", closeErr.Error()))
		}
		failure := integrityCheckFailure(checkErr)
		if IsStateStoreIncompatible(failure) {
			logger.Error("sync state DB failed its integrity check",
				slog.String("db_path", dbPath), slog.String("error", checkErr.Error()))
		}

		return nil, failure
	}
	if !stateDBFamilyExists(dbPath) {
		return nil, err
	}

	reason := stateStoreIncompatibleReasonOpenFailed
	switch {
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

func stateDBFamilyExists(dbPath string) bool {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := localpath.Stat(candidate); err == nil {
			return true
		}
	}

	return false
}

// checkStoreIntegrity runs PRAGMA quick_check on an open store (R-2.5.8). A
// damaged b-tree can hide rows, and a missing remote_state row reads as a
// remote delete, so the engine must refuse the DB before planning from it.
// quick_check skips the index-to-table cross-check of integrity_check, which
// keeps it linear in the DB size and cheap enough for every startup.
func checkStoreIntegrity(ctx context.Context, store *SyncStore) error {
	rows, err := store.rawDB().QueryContext(ctx, `PRAGMA quick_check`)
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
