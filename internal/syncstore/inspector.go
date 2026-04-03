package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	_ "modernc.org/sqlite"
)

// Inspector is a read-only sync-state boundary for CLI status and other
// administrative readers that must not own raw SQLite access themselves.
type Inspector struct {
	db     *sql.DB
	logger *slog.Logger
}

// StatusSnapshot is the read-only projection consumed by the CLI status
// command. It intentionally exposes counts and metadata only, not raw tables.
type StatusSnapshot struct {
	SyncMetadata        map[string]string
	BaselineEntryCount  int
	UnresolvedConflicts int
	ActionableFailures  int
	RemoteBlockedScopes int
	AuthScopeBlocks     int
	PendingSyncItems    int
	RetryingFailures    int
}

// OpenInspector opens a read-only connection to a sync state database.
func OpenInspector(dbPath string, logger *slog.Logger) (*Inspector, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(1000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only sync store %s: %w", dbPath, err)
	}

	return &Inspector{
		db:     db,
		logger: logger,
	}, nil
}

func (i *Inspector) Close() error {
	if err := i.db.Close(); err != nil {
		return fmt.Errorf("close read-only sync store: %w", err)
	}

	return nil
}

// ReadStatusSnapshot returns the CLI status projection for a sync state DB.
// Missing tables are tolerated so older or partially initialized DBs still
// yield best-effort status information.
func (i *Inspector) ReadStatusSnapshot(ctx context.Context) StatusSnapshot {
	snapshot := StatusSnapshot{
		SyncMetadata: make(map[string]string),
	}

	rows, err := i.db.QueryContext(ctx, "SELECT key, value FROM sync_metadata")
	if err == nil {
		defer rows.Close()

		for rows.Next() {
			var key, value string
			if scanErr := rows.Scan(&key, &value); scanErr == nil {
				snapshot.SyncMetadata[key] = value
			}
		}
		if rowErr := rows.Err(); rowErr != nil {
			i.logger.Debug("read sync metadata snapshot", slog.String("error", rowErr.Error()))
		}
	}

	snapshot.BaselineEntryCount = i.countOrZero(ctx, "baseline entries", "SELECT COUNT(*) FROM baseline")
	snapshot.UnresolvedConflicts = i.countOrZero(
		ctx,
		"unresolved conflicts",
		"SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'",
	)
	snapshot.ActionableFailures = i.countOrZero(
		ctx,
		"actionable sync failures",
		"SELECT COUNT(*) FROM sync_failures WHERE category = 'actionable'",
	)
	snapshot.RemoteBlockedScopes = i.countOrZero(
		ctx,
		"remote blocked scopes",
		`SELECT COUNT(DISTINCT scope_key) FROM sync_failures
		WHERE failure_role = 'held' AND scope_key LIKE 'perm:remote:%'`,
	)
	snapshot.AuthScopeBlocks = i.countOrZero(
		ctx,
		"auth scope blocks",
		"SELECT COUNT(*) FROM scope_blocks WHERE scope_key = 'auth:account'",
	)
	snapshot.PendingSyncItems = i.countOrZero(
		ctx,
		"pending sync items",
		"SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced','deleted','filtered')",
	)
	snapshot.RetryingFailures = i.countOrZero(
		ctx,
		"retrying sync failures",
		"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
	)

	return snapshot
}

func (i *Inspector) countOrZero(ctx context.Context, label, query string) int {
	var count int
	if err := i.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		i.logger.Debug("read sync status count", slog.String("label", label), slog.String("error", err.Error()))
		return 0
	}

	return count
}
