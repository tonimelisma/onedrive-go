package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type StateDBResetReason string

const (
	StateDBResetReasonOpenFailed         StateDBResetReason = "open_failed"
	StateDBResetReasonIncompatibleSchema StateDBResetReason = "incompatible_schema"
)

var ErrStateDBResetRequired = errors.New("sync: sync state DB reset required")

type StateDBResetRequiredError struct {
	Reason StateDBResetReason
	Cause  error
}

func (e *StateDBResetRequiredError) Error() string {
	if e == nil {
		return ""
	}

	if e.Cause != nil {
		return fmt.Sprintf("sync state DB requires reset: %s: %v", e.Reason.Description(), e.Cause)
	}

	return fmt.Sprintf("sync state DB requires reset: %s", e.Reason.Description())
}

func (e *StateDBResetRequiredError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Cause
}

func (e *StateDBResetRequiredError) Is(target error) bool {
	return target == ErrStateDBResetRequired
}

func IsStateDBResetRequired(err error) bool {
	return errors.Is(err, ErrStateDBResetRequired)
}

func (r StateDBResetReason) Description() string {
	switch r {
	case StateDBResetReasonOpenFailed:
		return "existing sync state DB could not be opened"
	case StateDBResetReasonIncompatibleSchema:
		return "existing sync state DB uses an unsupported store generation or schema"
	default:
		return "existing sync state DB is unsupported"
	}
}

// openEngineSyncStore is the engine-owned compatibility boundary for existing
// state DBs. It never mutates or recreates durable state during startup:
// missing DBs bootstrap normally via NewSyncStore, while unreadable or
// incompatible existing DBs surface a typed reset-required error.
func openEngineSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		return store, nil
	}
	if !stateDBFamilyExists(dbPath) {
		return nil, err
	}

	reason := StateDBResetReasonOpenFailed
	if errors.Is(err, ErrIncompatibleSchema) {
		reason = StateDBResetReasonIncompatibleSchema
	}

	return nil, &StateDBResetRequiredError{
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
