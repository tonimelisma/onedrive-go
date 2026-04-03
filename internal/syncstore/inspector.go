package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"

	_ "modernc.org/sqlite"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	SyncMetadata       map[string]string
	BaselineEntryCount int
	Issues             IssueSummary
	PendingSyncItems   int
}

// IssueGroupCount is one derived visible issue family with its aggregated
// count in the read-only status projection.
type IssueGroupCount struct {
	Key   synctypes.SummaryKey
	Count int
}

// IssueSummary is the store-owned aggregate view of visible issues for the
// status command. It centralizes how conflicts, actionable rows, and special
// derived scopes count toward status.
type IssueSummary struct {
	Groups   []IssueGroupCount
	Retrying int
}

func (s IssueSummary) VisibleTotal() int {
	total := 0
	for _, group := range s.Groups {
		total += group.Count
	}

	return total
}

func (s IssueSummary) ConflictCount() int {
	return s.countForKey(synctypes.SummaryConflictUnresolved)
}

func (s IssueSummary) ActionableCount() int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == synctypes.SummaryConflictUnresolved ||
			group.Key == synctypes.SummarySharedFolderWritesBlocked ||
			group.Key == synctypes.SummaryAuthenticationRequired {
			continue
		}
		total += group.Count
	}

	return total
}

func (s IssueSummary) RemoteBlockedCount() int {
	return s.countForKey(synctypes.SummarySharedFolderWritesBlocked)
}

func (s IssueSummary) AuthRequiredCount() int {
	return s.countForKey(synctypes.SummaryAuthenticationRequired)
}

func (s IssueSummary) RetryingCount() int {
	return s.Retrying
}

func (s IssueSummary) countForKey(key synctypes.SummaryKey) int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == key {
			total += group.Count
		}
	}

	return total
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
	snapshot.Issues = i.readIssueSummary(ctx)
	snapshot.PendingSyncItems = i.countOrZero(
		ctx,
		"pending sync items",
		"SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced','deleted','filtered')",
	)

	return snapshot
}

func (i *Inspector) readIssueSummary(ctx context.Context) IssueSummary {
	groupCounts := make(map[synctypes.SummaryKey]int)

	addCount := func(key synctypes.SummaryKey, count int) {
		if key == "" || count <= 0 {
			return
		}
		groupCounts[key] += count
	}

	addCount(
		synctypes.SummaryConflictUnresolved,
		i.countOrZero(ctx, "unresolved conflicts", "SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'"),
	)

	i.readGroupedCounts(
		ctx,
		"actionable sync failure groups",
		`SELECT COALESCE(issue_type, ''), COUNT(*) FROM sync_failures
		WHERE category = 'actionable'
		GROUP BY issue_type`,
		func(issueType string, count int) {
			addCount(
				synctypes.SummaryKeyForPersistedFailure(
					issueType,
					synctypes.CategoryActionable,
					synctypes.FailureRoleItem,
				),
				count,
			)
		},
	)

	i.readGroupedKeys(
		ctx,
		"remote blocked scope groups",
		`SELECT COALESCE(issue_type, ''), scope_key FROM sync_failures
		WHERE failure_role = 'held' AND scope_key LIKE 'perm:remote:%'
		GROUP BY issue_type, scope_key`,
		func(issueType, scopeKey string) {
			addCount(
				synctypes.SummaryKeyForPersistedFailure(
					issueType,
					synctypes.CategoryTransient,
					synctypes.FailureRoleHeld,
				),
				1,
			)
		},
	)

	i.readGroupedKeys(
		ctx,
		"auth scope block groups",
		`SELECT COALESCE(issue_type, ''), scope_key FROM scope_blocks
		WHERE scope_key = 'auth:account'
		GROUP BY issue_type, scope_key`,
		func(issueType, scopeKey string) {
			addCount(
				synctypes.SummaryKeyForScopeBlock(issueType, synctypes.ParseScopeKey(scopeKey)),
				1,
			)
		},
	)

	summary := IssueSummary{
		Groups: make([]IssueGroupCount, 0, len(groupCounts)),
		Retrying: i.countOrZero(
			ctx,
			"retrying sync failures",
			"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
		),
	}
	for key, count := range groupCounts {
		summary.Groups = append(summary.Groups, IssueGroupCount{Key: key, Count: count})
	}
	sort.Slice(summary.Groups, func(i, j int) bool {
		return string(summary.Groups[i].Key) < string(summary.Groups[j].Key)
	})

	return summary
}

func (i *Inspector) countOrZero(ctx context.Context, label, query string) int {
	var count int
	if err := i.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		i.logger.Debug("read sync status count", slog.String("label", label), slog.String("error", err.Error()))
		return 0
	}

	return count
}

func (i *Inspector) readGroupedCounts(
	ctx context.Context,
	label string,
	query string,
	add func(issueType string, count int),
) {
	i.iterateGroupedRows(ctx, label, query, func(rows *sql.Rows) error {
		var issueType string
		var count int
		if err := rows.Scan(&issueType, &count); err != nil {
			return fmt.Errorf("scan grouped count row: %w", err)
		}
		add(issueType, count)
		return nil
	})
}

func (i *Inspector) readGroupedKeys(
	ctx context.Context,
	label string,
	query string,
	add func(issueType, scopeKey string),
) {
	i.iterateGroupedRows(ctx, label, query, func(rows *sql.Rows) error {
		var issueType string
		var scopeKey string
		if err := rows.Scan(&issueType, &scopeKey); err != nil {
			return fmt.Errorf("scan grouped scope row: %w", err)
		}
		add(issueType, scopeKey)
		return nil
	})
}

func (i *Inspector) iterateGroupedRows(
	ctx context.Context,
	label string,
	query string,
	scan func(rows *sql.Rows) error,
) {
	rows, err := i.db.QueryContext(ctx, query)
	if err != nil {
		i.logger.Debug("read sync status groups", slog.String("label", label), slog.String("error", err.Error()))
		return
	}
	defer rows.Close()

	for rows.Next() {
		if scanErr := scan(rows); scanErr != nil {
			i.logger.Debug("scan sync status groups", slog.String("label", label), slog.String("error", scanErr.Error()))
			return
		}
	}
	if rowErr := rows.Err(); rowErr != nil {
		i.logger.Debug("iterate sync status groups", slog.String("label", label), slog.String("error", rowErr.Error()))
	}
}
