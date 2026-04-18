package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

type StateDBResetReason string

const (
	StateDBResetReasonOpenFailed                StateDBResetReason = "open_failed"
	StateDBResetReasonIncompatibleSchema        StateDBResetReason = "incompatible_schema"
	StateDBResetReasonLegacyThrottleAccount     StateDBResetReason = "legacy_throttle_account"
	StateDBResetReasonLegacyPermRemoteWrite     StateDBResetReason = "legacy_perm_remote_write"
	StateDBResetReasonRemotePermissionAuthority StateDBResetReason = "remote_permission_authority"
	StateDBResetReasonIllegalRetryTiming        StateDBResetReason = "illegal_retry_timing"
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

func (r StateDBResetReason) Description() string {
	switch r {
	case StateDBResetReasonOpenFailed:
		return "existing sync state DB could not be opened"
	case StateDBResetReasonIncompatibleSchema:
		return "existing sync state DB uses an incompatible schema"
	case StateDBResetReasonLegacyThrottleAccount:
		return "existing sync state DB contains unsupported legacy throttle state"
	case StateDBResetReasonLegacyPermRemoteWrite:
		return "existing sync state DB contains unsupported legacy remote-permission alias state"
	case StateDBResetReasonRemotePermissionAuthority:
		return "existing sync state DB contains unsupported persisted remote-permission authorities"
	case StateDBResetReasonIllegalRetryTiming:
		return "existing sync state DB contains unsupported persisted retry timing"
	default:
		return "existing sync state DB is unsupported"
	}
}

func openEngineSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
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

	reason, probeErr := detectResetRequiredPersistedState(ctx, store)
	if probeErr != nil {
		closeErr := store.Close(context.WithoutCancel(ctx))
		if closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("detect unsupported persisted state: %w", probeErr),
				fmt.Errorf("close sync store after unsupported-state probe: %w", closeErr),
			)
		}
		return nil, fmt.Errorf("detect unsupported persisted state: %w", probeErr)
	}
	if reason == "" {
		return store, nil
	}

	if closeErr := store.Close(context.WithoutCancel(ctx)); closeErr != nil {
		return nil, fmt.Errorf("close sync store before reset-required exit: %w", closeErr)
	}

	return nil, &StateDBResetRequiredError{Reason: reason}
}

func detectResetRequiredPersistedState(ctx context.Context, store *SyncStore) (StateDBResetReason, error) {
	probes := []struct {
		reason StateDBResetReason
		query  string
		args   []any
	}{
		{
			reason: StateDBResetReasonLegacyThrottleAccount,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key = ? LIMIT 1`,
			args:   []any{wireLegacyThrottleAccount},
		},
		{
			reason: StateDBResetReasonLegacyThrottleAccount,
			query:  `SELECT 1 FROM retry_state WHERE scope_key = ? LIMIT 1`,
			args:   []any{wireLegacyThrottleAccount},
		},
		{
			reason: StateDBResetReasonLegacyThrottleAccount,
			query:  `SELECT 1 FROM sync_failures WHERE scope_key = ? LIMIT 1`,
			args:   []any{wireLegacyThrottleAccount},
		},
		{
			reason: StateDBResetReasonLegacyPermRemoteWrite,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{legacyPermRemoteWriteLikePattern()},
		},
		{
			reason: StateDBResetReasonLegacyPermRemoteWrite,
			query:  `SELECT 1 FROM retry_state WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{legacyPermRemoteWriteLikePattern()},
		},
		{
			reason: StateDBResetReasonLegacyPermRemoteWrite,
			query:  `SELECT 1 FROM sync_failures WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{legacyPermRemoteWriteLikePattern()},
		},
		{
			reason: StateDBResetReasonRemotePermissionAuthority,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{permRemoteScopeKeyLikePattern()},
		},
		{
			reason: StateDBResetReasonRemotePermissionAuthority,
			query: `SELECT 1 FROM sync_failures
				WHERE failure_role = ? AND scope_key LIKE ?
				LIMIT 1`,
			args: []any{FailureRoleBoundary, permRemoteScopeKeyLikePattern()},
		},
		{
			reason: StateDBResetReasonIllegalRetryTiming,
			query: `SELECT 1 FROM sync_failures
				WHERE next_retry_at IS NOT NULL
				  AND (category <> ? OR failure_role <> ?)
				LIMIT 1`,
			args: []any{CategoryTransient, FailureRoleItem},
		},
	}

	for i := range probes {
		found, err := rowExists(ctx, store.db, probes[i].query, probes[i].args...)
		if err != nil {
			return "", err
		}
		if found {
			return probes[i].reason, nil
		}
	}

	return "", nil
}

func rowExists(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var exists int
	err := db.QueryRowContext(ctx, query, args...).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	return false, fmt.Errorf("query persisted state probe: %w", err)
}

func stateDBFamilyExists(dbPath string) bool {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := localpath.Stat(candidate); err == nil {
			return true
		}
	}

	return false
}
