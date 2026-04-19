package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type StateStoreIncompatibleReason string

const (
	StateStoreIncompatibleReasonOpenFailed         StateStoreIncompatibleReason = "open_failed"
	StateStoreIncompatibleReasonIncompatibleSchema StateStoreIncompatibleReason = "incompatible_schema"
)

var ErrStateStoreIncompatible = errors.New("sync: incompatible sync state DB")

type StateStoreIncompatibleError struct {
	Reason StateStoreIncompatibleReason
	Cause  error
}

func (e *StateStoreIncompatibleError) Error() string {
	if e == nil {
		return ""
	}

	if e.Cause != nil {
		return fmt.Sprintf("sync state DB is incompatible: %s: %v", e.Reason.Description(), e.Cause)
	}

	return fmt.Sprintf("sync state DB is incompatible: %s", e.Reason.Description())
}

func (e *StateStoreIncompatibleError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Cause
}

func (e *StateStoreIncompatibleError) Is(target error) bool {
	return target == ErrStateStoreIncompatible
}

func IsStateStoreIncompatible(err error) bool {
	return errors.Is(err, ErrStateStoreIncompatible)
}

func (r StateStoreIncompatibleReason) Description() string {
	switch r {
	case StateStoreIncompatibleReasonOpenFailed:
		return "existing sync state DB could not be opened"
	case StateStoreIncompatibleReasonIncompatibleSchema:
		return "existing sync state DB uses an unsupported store generation or schema"
	default:
		return "existing sync state DB is unsupported"
	}
}

// openEngineSyncStore is the engine-owned compatibility boundary for existing
// state DBs. It never mutates or recreates durable state during startup:
// missing DBs bootstrap normally via NewSyncStore, while unreadable or
// incompatible existing DBs surface a typed store-incompatible error.
func openEngineSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	store, err := NewSyncStore(ctx, dbPath, logger)
	if err == nil {
		return store, nil
	}
	if !stateDBFamilyExists(dbPath) {
		return nil, err
	}

	reason := StateStoreIncompatibleReasonOpenFailed
	if errors.Is(err, ErrIncompatibleSchema) {
		reason = StateStoreIncompatibleReasonIncompatibleSchema
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
