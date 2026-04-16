package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// Inspector is a read-only sync-state boundary for CLI status and other
// administrative readers that must not own raw SQLite access themselves.
type Inspector struct {
	db     *sql.DB
	logger *slog.Logger
}

// sqlCountRemoteDriftItems counts already-observed remote-side differences
// that are not yet reflected in baseline. This is the durable remote half of
// "not fully converged yet"; exact local-only drift still requires a live scan.
const sqlCountRemoteDriftItems = `
SELECT COUNT(*) FROM (
	SELECT rs.item_id
	FROM remote_state rs
	LEFT JOIN baseline b
	  ON b.item_id = rs.item_id
	WHERE (
		b.item_id IS NULL OR
		b.path <> rs.path OR
		b.item_type <> rs.item_type OR
		COALESCE(b.remote_hash, '') <> COALESCE(rs.hash, '') OR
		COALESCE(b.remote_mtime, 0) <> COALESCE(rs.mtime, 0)
	  )
	UNION
	SELECT b.item_id
	FROM baseline b
	LEFT JOIN remote_state rs
	  ON rs.item_id = b.item_id
	WHERE rs.item_id IS NULL
) remote_drift`

// DriveStatusSnapshot is the per-drive status snapshot consumed by the
// product-facing status command. It keeps the full per-drive sync-health view
// in one store-owned projection.
type DriveStatusSnapshot struct {
	SyncMetadata       map[string]string
	BaselineEntryCount int
	RemoteDriftItems   int
	RetryingItems      int
	IssueGroups        []IssueGroupSnapshot
}

// groupedIssueProjection is the internal grouped sync-health projection shared
// by the per-drive status snapshot builder and store/package tests. It stays
// unexported so the public store read surface has one product-facing per-drive
// projection.
type groupedIssueProjection struct {
	Groups         []IssueGroupSnapshot
	PendingRetries []PendingRetrySnapshot
}

// IssueGroupSnapshot is one visible grouped issue family in the read-only
// sync-health projection.
type IssueGroupSnapshot struct {
	SummaryKey       SummaryKey
	PrimaryIssueType string
	ScopeKey         ScopeKey
	ScopeLabel       string
	Paths            []string
	Count            int
}

// PendingRetrySnapshot is one aggregated transient retry group in the
// store-owned sync-health projection. It remains available for internal
// observers and tests that assert retry state.
type PendingRetrySnapshot struct {
	ScopeKey     ScopeKey
	ScopeLabel   string
	Count        int
	EarliestNext time.Time
}

// IssueGroupCount is one derived visible issue family with its aggregated
// count in the read-only status projection.
type IssueGroupCount struct {
	Key       SummaryKey
	Count     int
	ScopeKind string
	Scope     string
}

const (
	statusScopeFile      = "file"
	statusScopeDirectory = "directory"
	statusScopeDrive     = "drive"
	statusScopeAccount   = "account"
	statusScopeService   = "service"
	statusScopeDisk      = "disk"
)

type issueGroupIdentity struct {
	Key       SummaryKey
	ScopeKind string
	Scope     string
}

type issueGroupAccumulator map[issueGroupIdentity]int

func (a issueGroupAccumulator) Add(key SummaryKey, count int, scopeKind, scope string) {
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
// status command. It centralizes how actionable rows and special derived
// scopes count toward status.
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
	return 0
}

func (s IssueSummary) ActionableCount() int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == SummarySharedFolderWritesBlocked ||
			group.Key == SummaryAuthenticationRequired {
			continue
		}
		total += group.Count
	}

	return total
}

func (s IssueSummary) RemoteBlockedCount() int {
	return s.countForKey(SummarySharedFolderWritesBlocked)
}

func (s IssueSummary) AuthRequiredCount() int {
	return s.countForKey(SummaryAuthenticationRequired)
}

func (s IssueSummary) RetryingCount() int {
	return s.Retrying
}

func (s IssueSummary) countForKey(key SummaryKey) int {
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
	db, err := openReadOnlySyncStoreDB(dbPath)
	if err != nil {
		return nil, err
	}

	return &Inspector{
		db:     db,
		logger: logger,
	}, nil
}

func openReadOnlySyncStoreDB(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(1000)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only sync store %s: %w", dbPath, err)
	}

	return db, nil
}

func (i *Inspector) Close() error {
	if err := i.db.Close(); err != nil {
		return fmt.Errorf("close read-only sync store: %w", err)
	}

	return nil
}

// HasScopeBlock reports whether the read-only store currently contains the
// requested scope block. Missing scope-block tables are treated as empty so
// partially initialized state databases can still be inspected.
func (i *Inspector) HasScopeBlock(ctx context.Context, key ScopeKey) (bool, error) {
	var exists int
	err := i.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM scope_blocks WHERE scope_key = ? LIMIT 1`,
		key.String(),
	).Scan(&exists)
	if err == nil {
		return true, nil
	}
	if err == sql.ErrNoRows || isMissingTableErr(err) {
		return false, nil
	}

	return false, fmt.Errorf("query scope block %s: %w", key, err)
}

// ReadDriveStatusSnapshot returns the per-drive status projection used by the
// product-facing status command. Missing tables are tolerated so older or
// partially initialized DBs still yield best-effort status information.
func (i *Inspector) ReadDriveStatusSnapshot(ctx context.Context, history bool) (DriveStatusSnapshot, error) {
	snapshot := DriveStatusSnapshot{
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
			i.logger.Debug("read drive status sync metadata snapshot", slog.String("error", rowErr.Error()))
		}
	}

	snapshot.BaselineEntryCount = i.countOrZero(ctx, "baseline entries", "SELECT COUNT(*) FROM baseline")
	snapshot.RemoteDriftItems = i.countOrZero(
		ctx,
		"remote drift items",
		sqlCountRemoteDriftItems,
	)
	snapshot.RetryingItems = i.countOrZero(
		ctx,
		"retrying sync failures",
		"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
	)

	projection, err := i.readGroupedIssueProjectionModel(ctx)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("read drive status projection: %w", err)
	}
	snapshot.IssueGroups = projection.snapshot.Groups
	_ = history

	return snapshot, nil
}

type groupedIssueProjectionModel struct {
	snapshot groupedIssueProjection
	summary  IssueSummary
}

func (i *Inspector) readGroupedIssueProjectionModel(ctx context.Context) (groupedIssueProjectionModel, error) {
	actionableFailures, err := i.listActionableFailures(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list actionable failures: %w", err)
	}

	remoteBlocked, err := i.listRemoteBlockedFailures(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list remote blocked failures: %w", err)
	}

	scopeBlocks, err := i.listScopeBlocks(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list scope blocks: %w", err)
	}

	pendingRetries, err := i.pendingRetrySummary(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("pending retry summary: %w", err)
	}

	return buildGroupedIssueProjection(
		actionableFailures,
		remoteBlocked,
		scopeBlocks,
		pendingRetries,
	), nil
}

func buildGroupedIssueProjection(
	actionableFailures []SyncFailureRow,
	remoteBlocked []SyncFailureRow,
	scopeBlocks []*ScopeBlock,
	pendingRetries []PendingRetryGroup,
) groupedIssueProjectionModel {
	builder := newGroupedIssueProjectionBuilder(len(pendingRetries))
	builder.addActionableFailures(actionableFailures)
	builder.addRemoteBlocked(remoteBlocked)
	builder.addAuthScopeBlocks(scopeBlocks)
	builder.addPendingRetries(pendingRetries)
	return builder.projection()
}

type issueGroupKey struct {
	summaryKey SummaryKey
	scopeKey   string
}

type groupedIssueProjectionBuilder struct {
	groupCounts issueGroupAccumulator
	groupIndex  map[issueGroupKey]int
	snapshot    groupedIssueProjection
}

func newGroupedIssueProjectionBuilder(pendingRetryCap int) *groupedIssueProjectionBuilder {
	return &groupedIssueProjectionBuilder{
		groupCounts: make(issueGroupAccumulator),
		groupIndex:  make(map[issueGroupKey]int),
		snapshot: groupedIssueProjection{
			Groups:         make([]IssueGroupSnapshot, 0),
			PendingRetries: make([]PendingRetrySnapshot, 0, pendingRetryCap),
		},
	}
}

func (b *groupedIssueProjectionBuilder) addSummary(
	key SummaryKey,
	scopeKey ScopeKey,
	role FailureRole,
) {
	scopeKind, scope := statusIssueScope(scopeKey, role)
	b.groupCounts.Add(key, 1, scopeKind, scope)
}

func (b *groupedIssueProjectionBuilder) addGroupedPath(
	summaryKey SummaryKey,
	issueType string,
	scopeKey ScopeKey,
	path string,
) {
	if summaryKey == "" {
		return
	}

	key := issueGroupKey{summaryKey: summaryKey, scopeKey: scopeKey.String()}
	if idx, ok := b.groupIndex[key]; ok {
		b.snapshot.Groups[idx].Paths = append(b.snapshot.Groups[idx].Paths, path)
		b.snapshot.Groups[idx].Count++
		return
	}

	b.groupIndex[key] = len(b.snapshot.Groups)
	b.snapshot.Groups = append(b.snapshot.Groups, IssueGroupSnapshot{
		SummaryKey:       summaryKey,
		PrimaryIssueType: issueType,
		ScopeKey:         scopeKey,
		ScopeLabel:       scopeKey.Humanize(),
		Paths:            []string{path},
		Count:            1,
	})
}

func (b *groupedIssueProjectionBuilder) addScopeOnlyGroup(
	summaryKey SummaryKey,
	issueType string,
	scopeKey ScopeKey,
) bool {
	if summaryKey == "" {
		return false
	}

	key := issueGroupKey{summaryKey: summaryKey, scopeKey: scopeKey.String()}
	if _, ok := b.groupIndex[key]; ok {
		return false
	}

	b.groupIndex[key] = len(b.snapshot.Groups)
	b.snapshot.Groups = append(b.snapshot.Groups, IssueGroupSnapshot{
		SummaryKey:       summaryKey,
		PrimaryIssueType: issueType,
		ScopeKey:         scopeKey,
		ScopeLabel:       scopeKey.Humanize(),
		Paths:            []string{},
		Count:            1,
	})
	return true
}

func (b *groupedIssueProjectionBuilder) addActionableFailures(rows []SyncFailureRow) {
	for i := range rows {
		row := rows[i]
		summaryKey := SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
		b.addGroupedPath(summaryKey, row.IssueType, row.ScopeKey, row.Path)
		b.addSummary(summaryKey, row.ScopeKey, row.Role)
	}
}

func (b *groupedIssueProjectionBuilder) addRemoteBlocked(rows []SyncFailureRow) {
	for i := range rows {
		row := rows[i]
		summaryKey := SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
		b.addGroupedPath(summaryKey, row.IssueType, row.ScopeKey, row.Path)
		b.addSummary(summaryKey, row.ScopeKey, row.Role)
	}
}

func (b *groupedIssueProjectionBuilder) addAuthScopeBlocks(blocks []*ScopeBlock) {
	for i := range blocks {
		block := blocks[i]
		if block.Key != SKAuthAccount() {
			continue
		}

		summaryKey := SummaryKeyForScopeBlock(block.IssueType, block.Key)
		if b.addScopeOnlyGroup(summaryKey, block.IssueType, block.Key) {
			b.addSummary(summaryKey, block.Key, FailureRoleBoundary)
		}
	}
}

func (b *groupedIssueProjectionBuilder) addPendingRetries(groups []PendingRetryGroup) {
	for i := range groups {
		group := groups[i]
		b.snapshot.PendingRetries = append(b.snapshot.PendingRetries, PendingRetrySnapshot{
			ScopeKey:     group.ScopeKey,
			ScopeLabel:   group.ScopeKey.Humanize(),
			Count:        group.Count,
			EarliestNext: group.EarliestNext,
		})
	}
}

func (b *groupedIssueProjectionBuilder) projection() groupedIssueProjectionModel {
	sortIssueGroups(b.snapshot.Groups)

	summary := IssueSummary{
		Groups: b.groupCounts.Groups(),
	}

	return groupedIssueProjectionModel{
		snapshot: b.snapshot,
		summary:  summary,
	}
}

func statusIssueScope(
	scopeKey ScopeKey,
	role FailureRole,
) (string, string) {
	if !scopeKey.IsZero() {
		switch scopeKey.Kind {
		case ScopeAuthAccount, ScopeThrottleAccount:
			return statusScopeAccount, scopeKey.Humanize()
		case ScopeThrottleTarget:
			return statusScopeDrive, scopeKey.Humanize()
		case ScopeService:
			return statusScopeService, scopeKey.Humanize()
		case ScopeQuotaOwn:
			return statusScopeDrive, scopeKey.Humanize()
		case ScopePermRemote:
			return statusScopeDirectory, scopeKey.Humanize()
		case ScopePermDir:
			return statusScopeDirectory, scopeKey.Humanize()
		case ScopeDiskLocal:
			return statusScopeDisk, scopeKey.Humanize()
		}
	}

	if role == FailureRoleBoundary {
		return statusScopeDirectory, ""
	}

	return statusScopeFile, ""
}

func sortIssueGroups(groups []IssueGroupSnapshot) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		if groups[i].SummaryKey != groups[j].SummaryKey {
			return string(groups[i].SummaryKey) < string(groups[j].SummaryKey)
		}

		return groups[i].ScopeLabel < groups[j].ScopeLabel
	})

	for i := range groups {
		sort.Strings(groups[i].Paths)
	}
}

func (i *Inspector) listActionableFailures(ctx context.Context) ([]SyncFailureRow, error) {
	configuredDriveID, err := configuredDriveIDForDB(ctx, i.db)
	if err != nil {
		return nil, err
	}

	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable'
		ORDER BY last_seen_at DESC`,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query actionable failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows, configuredDriveID)
}

func (i *Inspector) listRemoteBlockedFailures(ctx context.Context) ([]SyncFailureRow, error) {
	configuredDriveID, err := configuredDriveIDForDB(ctx, i.db)
	if err != nil {
		return nil, err
	}

	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE failure_role = ?
			AND (scope_key LIKE 'perm:remote-write:%' OR scope_key LIKE 'perm:remote:%')
		ORDER BY last_seen_at DESC`,
		FailureRoleHeld,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query remote blocked failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows, configuredDriveID)
}

func (i *Inspector) listScopeBlocks(ctx context.Context) ([]*ScopeBlock, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count
		FROM scope_blocks`)
	if err != nil {
		if isMissingTableErr(err) {
			return []*ScopeBlock{}, nil
		}
		return nil, fmt.Errorf("query scope blocks: %w", err)
	}
	defer rows.Close()

	var result []*ScopeBlock
	for rows.Next() {
		var (
			wireKey       string
			issueType     string
			timingSource  string
			blockedAtNano int64
			intervalNano  int64
			nextTrialNano int64
			preserveNano  int64
			trialCount    int
		)

		if err := rows.Scan(
			&wireKey,
			&issueType,
			&timingSource,
			&blockedAtNano,
			&intervalNano,
			&nextTrialNano,
			&preserveNano,
			&trialCount,
		); err != nil {
			return nil, fmt.Errorf("scan scope block row: %w", err)
		}

		block := &ScopeBlock{
			Key:           ParseScopeKey(wireKey),
			IssueType:     issueType,
			TimingSource:  ScopeTimingSource(timingSource),
			BlockedAt:     time.Unix(0, blockedAtNano).UTC(),
			TrialInterval: time.Duration(intervalNano),
			TrialCount:    trialCount,
		}
		if nextTrialNano != 0 {
			block.NextTrialAt = time.Unix(0, nextTrialNano).UTC()
		}
		if preserveNano != 0 {
			block.PreserveUntil = time.Unix(0, preserveNano).UTC()
		}

		result = append(result, block)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scope block rows: %w", err)
	}

	return result, nil
}

func (i *Inspector) pendingRetrySummary(ctx context.Context) ([]PendingRetryGroup, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT COALESCE(scope_key, ''), COUNT(*), MIN(next_retry_at)
		 FROM sync_failures
		 WHERE category = 'transient' AND next_retry_at > 0
		 GROUP BY scope_key
		 ORDER BY COUNT(*) DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []PendingRetryGroup{}, nil
		}
		return nil, fmt.Errorf("query pending retry summary: %w", err)
	}
	defer rows.Close()

	var result []PendingRetryGroup
	for rows.Next() {
		var (
			group        PendingRetryGroup
			wireScopeKey string
			minNano      int64
		)

		if err := rows.Scan(&wireScopeKey, &group.Count, &minNano); err != nil {
			return nil, fmt.Errorf("scan pending retry group: %w", err)
		}

		group.ScopeKey = ParseScopeKey(wireScopeKey)
		if minNano > 0 {
			group.EarliestNext = time.Unix(0, minNano)
		}
		result = append(result, group)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending retry groups: %w", err)
	}

	return result, nil
}

func (i *Inspector) countOrZero(ctx context.Context, label, query string) int {
	var count int
	if err := i.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		i.logger.Debug("read sync status count", slog.String("label", label), slog.String("error", err.Error()))
		return 0
	}

	return count
}
