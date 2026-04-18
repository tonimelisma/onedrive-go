package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	unsupportedLegacyThrottleAccountReason     = "legacy throttle:account state"
	unsupportedRemotePermissionAuthorityReason = "persisted remote permission authorities"
	unsupportedRemotePermissionAliasReason     = "legacy perm:remote-write state"
	unsupportedRetryTimingReason               = "illegal persisted retry timing"
)

func openEngineSyncStore(ctx context.Context, dbPath string, logger *slog.Logger) (*SyncStore, error) {
	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		if !stateDBFamilyExists(dbPath) {
			return nil, err
		}
		if recreateErr := recreateStateDB(ctx, dbPath, logger, err.Error()); recreateErr != nil {
			return nil, errors.Join(
				fmt.Errorf("open sync store: %w", err),
				recreateErr,
			)
		}

		reopened, reopenErr := NewSyncStore(ctx, dbPath, logger)
		if reopenErr != nil {
			return nil, errors.Join(
				fmt.Errorf("open sync store: %w", err),
				fmt.Errorf("open recreated sync store: %w", reopenErr),
			)
		}

		return reopened, nil
	}

	reason, probeErr := detectUnsupportedPersistedState(ctx, store)
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
		return nil, fmt.Errorf("close sync store before recreate: %w", closeErr)
	}
	if recreateErr := recreateStateDB(ctx, dbPath, logger, reason); recreateErr != nil {
		return nil, recreateErr
	}

	reopened, reopenErr := NewSyncStore(ctx, dbPath, logger)
	if reopenErr != nil {
		return nil, fmt.Errorf("open recreated sync store: %w", reopenErr)
	}

	return reopened, nil
}

func recreateStateDB(ctx context.Context, dbPath string, logger *slog.Logger, reason string) error {
	if logger != nil {
		logger.Warn("recreating sync state DB",
			slog.String("db_path", dbPath),
			slog.String("reason", reason),
		)
	}
	if err := removeStateDBFiles(dbPath); err != nil {
		return err
	}

	store, err := NewSyncStore(ctx, dbPath, logger)
	if err != nil {
		return fmt.Errorf("create fresh sync store: %w", err)
	}
	if err := store.Close(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("close fresh sync store: %w", err)
	}

	return nil
}

func detectUnsupportedPersistedState(ctx context.Context, store *SyncStore) (string, error) {
	probes := []struct {
		reason string
		query  string
		args   []any
	}{
		{
			reason: unsupportedLegacyThrottleAccountReason,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key = ? LIMIT 1`,
			args:   []any{"throttle:account"},
		},
		{
			reason: unsupportedLegacyThrottleAccountReason,
			query:  `SELECT 1 FROM retry_state WHERE scope_key = ? LIMIT 1`,
			args:   []any{"throttle:account"},
		},
		{
			reason: unsupportedLegacyThrottleAccountReason,
			query:  `SELECT 1 FROM sync_failures WHERE scope_key = ? LIMIT 1`,
			args:   []any{"throttle:account"},
		},
		{
			reason: unsupportedRemotePermissionAliasReason,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{"perm:remote-write:%"},
		},
		{
			reason: unsupportedRemotePermissionAliasReason,
			query:  `SELECT 1 FROM retry_state WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{"perm:remote-write:%"},
		},
		{
			reason: unsupportedRemotePermissionAliasReason,
			query:  `SELECT 1 FROM sync_failures WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{"perm:remote-write:%"},
		},
		{
			reason: unsupportedRemotePermissionAuthorityReason,
			query:  `SELECT 1 FROM scope_blocks WHERE scope_key LIKE ? LIMIT 1`,
			args:   []any{"perm:remote:%"},
		},
		{
			reason: unsupportedRemotePermissionAuthorityReason,
			query: `SELECT 1 FROM sync_failures
				WHERE failure_role = ? AND scope_key LIKE ?
				LIMIT 1`,
			args: []any{FailureRoleBoundary, "perm:remote:%"},
		},
		{
			reason: unsupportedRetryTimingReason,
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

func removeStateDBFiles(dbPath string) error {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := localpath.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state DB file %s: %w", candidate, err)
		}
	}

	return nil
}

func stateDBFamilyExists(dbPath string) bool {
	for _, candidate := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := localpath.Stat(candidate); err == nil {
			return true
		}
	}

	return false
}
