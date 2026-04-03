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
	Key       synctypes.SummaryKey
	Count     int
	ScopeKind string
	Scope     string
}

const (
	statusScopeFile      = "file"
	statusScopeDirectory = "directory"
	statusScopeDrive     = "drive"
	statusScopeShortcut  = "shortcut"
	statusScopeAccount   = "account"
	statusScopeService   = "service"
	statusScopeDisk      = "disk"
)

type issueGroupIdentity struct {
	Key       synctypes.SummaryKey
	ScopeKind string
	Scope     string
}

type issueGroupAccumulator map[issueGroupIdentity]int

func (a issueGroupAccumulator) Add(key synctypes.SummaryKey, count int, scopeKind, scope string) {
	if key == "" || count <= 0 || scopeKind == "" {
		return
	}

	a[issueGroupIdentity{
		Key:       key,
		ScopeKind: scopeKind,
		Scope:     scope,
	}] += count
}

func (a issueGroupAccumulator) Groups() []IssueGroupCount {
	groups := make([]IssueGroupCount, 0, len(a))
	for identity, count := range a {
		groups = append(groups, IssueGroupCount{
			Key:       identity.Key,
			Count:     count,
			ScopeKind: identity.ScopeKind,
			Scope:     identity.Scope,
		})
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Key != groups[j].Key {
			return string(groups[i].Key) < string(groups[j].Key)
		}
		if groups[i].ScopeKind != groups[j].ScopeKind {
			return groups[i].ScopeKind < groups[j].ScopeKind
		}

		return groups[i].Scope < groups[j].Scope
	})

	return groups
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
	shortcuts := i.readShortcuts(ctx)
	groupCounts := make(issueGroupAccumulator)

	groupCounts.Add(
		synctypes.SummaryConflictUnresolved,
		i.countOrZero(ctx, "unresolved conflicts", "SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'"),
		statusScopeFile,
		"",
	)
	i.collectActionableIssueGroups(ctx, groupCounts, shortcuts)
	i.collectHeldRemoteIssueGroups(ctx, groupCounts, shortcuts)
	i.collectAuthScopeIssueGroups(ctx, groupCounts, shortcuts)

	summary := IssueSummary{
		Groups: groupCounts.Groups(),
		Retrying: i.countOrZero(
			ctx,
			"retrying sync failures",
			"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
		),
	}

	return summary
}

func (i *Inspector) collectActionableIssueGroups(
	ctx context.Context,
	groupCounts issueGroupAccumulator,
	shortcuts []synctypes.Shortcut,
) {
	i.iterateGroupedRows(
		ctx,
		"actionable sync failure groups",
		`SELECT COALESCE(issue_type, ''), COALESCE(scope_key, ''), COALESCE(failure_role, ''), COUNT(*)
		FROM sync_failures
		WHERE category = 'actionable'
		GROUP BY issue_type, scope_key, failure_role`,
		func(rows *sql.Rows) error {
			var issueType string
			var scopeKeyWire string
			var failureRoleWire string
			var count int
			if err := rows.Scan(&issueType, &scopeKeyWire, &failureRoleWire, &count); err != nil {
				return fmt.Errorf("scan actionable status group row: %w", err)
			}

			role := synctypes.FailureRole(failureRoleWire)
			scopeKind, scope := statusIssueScope(
				synctypes.ParseScopeKey(scopeKeyWire),
				role,
				shortcuts,
			)
			groupCounts.Add(
				synctypes.SummaryKeyForPersistedFailure(issueType, synctypes.CategoryActionable, role),
				count,
				scopeKind,
				scope,
			)

			return nil
		},
	)
}

func (i *Inspector) collectHeldRemoteIssueGroups(
	ctx context.Context,
	groupCounts issueGroupAccumulator,
	shortcuts []synctypes.Shortcut,
) {
	i.readGroupedKeys(
		ctx,
		"remote blocked scope groups",
		`SELECT COALESCE(issue_type, ''), scope_key FROM sync_failures
		WHERE failure_role = 'held' AND scope_key LIKE 'perm:remote:%'
		GROUP BY issue_type, scope_key`,
		func(issueType, scopeKey string) {
			scopeKind, scope := statusIssueScope(
				synctypes.ParseScopeKey(scopeKey),
				synctypes.FailureRoleHeld,
				shortcuts,
			)
			groupCounts.Add(
				synctypes.SummaryKeyForPersistedFailure(
					issueType,
					synctypes.CategoryTransient,
					synctypes.FailureRoleHeld,
				),
				1,
				scopeKind,
				scope,
			)
		},
	)
}

func (i *Inspector) collectAuthScopeIssueGroups(
	ctx context.Context,
	groupCounts issueGroupAccumulator,
	shortcuts []synctypes.Shortcut,
) {
	i.readGroupedKeys(
		ctx,
		"auth scope block groups",
		`SELECT COALESCE(issue_type, ''), scope_key FROM scope_blocks
		WHERE scope_key = 'auth:account'
		GROUP BY issue_type, scope_key`,
		func(issueType, scopeKey string) {
			scopeKind, scope := statusIssueScope(
				synctypes.ParseScopeKey(scopeKey),
				synctypes.FailureRoleBoundary,
				shortcuts,
			)
			groupCounts.Add(
				synctypes.SummaryKeyForScopeBlock(issueType, synctypes.ParseScopeKey(scopeKey)),
				1,
				scopeKind,
				scope,
			)
		},
	)
}

func (i *Inspector) readShortcuts(ctx context.Context) []synctypes.Shortcut {
	rows, err := i.db.QueryContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		i.logger.Debug("read sync status shortcuts", slog.String("error", err.Error()))
		return nil
	}
	defer rows.Close()

	var shortcuts []synctypes.Shortcut
	for rows.Next() {
		var sc synctypes.Shortcut
		if scanErr := rows.Scan(
			&sc.ItemID,
			&sc.RemoteDrive,
			&sc.RemoteItem,
			&sc.LocalPath,
			&sc.DriveType,
			&sc.Observation,
			&sc.DiscoveredAt,
		); scanErr != nil {
			i.logger.Debug("scan sync status shortcut", slog.String("error", scanErr.Error()))
			return nil
		}

		shortcuts = append(shortcuts, sc)
	}
	if rowErr := rows.Err(); rowErr != nil {
		i.logger.Debug("iterate sync status shortcuts", slog.String("error", rowErr.Error()))
		return nil
	}

	return shortcuts
}

func statusIssueScope(
	scopeKey synctypes.ScopeKey,
	role synctypes.FailureRole,
	shortcuts []synctypes.Shortcut,
) (string, string) {
	if !scopeKey.IsZero() {
		switch scopeKey.Kind {
		case synctypes.ScopeAuthAccount, synctypes.ScopeThrottleAccount:
			return statusScopeAccount, scopeKey.Humanize(shortcuts)
		case synctypes.ScopeService:
			return statusScopeService, scopeKey.Humanize(shortcuts)
		case synctypes.ScopeQuotaOwn:
			return statusScopeDrive, scopeKey.Humanize(shortcuts)
		case synctypes.ScopeQuotaShortcut, synctypes.ScopePermRemote:
			return statusScopeShortcut, scopeKey.Humanize(shortcuts)
		case synctypes.ScopePermDir:
			return statusScopeDirectory, scopeKey.Humanize(shortcuts)
		case synctypes.ScopeDiskLocal:
			return statusScopeDisk, scopeKey.Humanize(shortcuts)
		}
	}

	if role == synctypes.FailureRoleBoundary {
		return statusScopeDirectory, ""
	}

	return statusScopeFile, ""
}

func (i *Inspector) countOrZero(ctx context.Context, label, query string) int {
	var count int
	if err := i.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		i.logger.Debug("read sync status count", slog.String("label", label), slog.String("error", err.Error()))
		return 0
	}

	return count
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
