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
	SELECT rs.drive_id, rs.item_id
	FROM remote_state rs
	LEFT JOIN baseline b
	  ON b.drive_id = rs.drive_id
	 AND b.item_id = rs.item_id
	WHERE rs.is_filtered = 0
	  AND (
		b.item_id IS NULL OR
		b.path <> rs.path OR
		b.item_type <> rs.item_type OR
		COALESCE(b.remote_hash, '') <> COALESCE(rs.hash, '') OR
		COALESCE(b.remote_mtime, 0) <> COALESCE(rs.mtime, 0)
	  )
	UNION
	SELECT b.drive_id, b.item_id
	FROM baseline b
	LEFT JOIN remote_state rs
	  ON rs.drive_id = b.drive_id
	 AND rs.item_id = b.item_id
	WHERE rs.item_id IS NULL
) remote_drift`

// StatusSnapshot is the read-only aggregate projection consumed by status
// summaries and daemon/control-plane readers. It intentionally exposes counts
// and metadata only, not raw tables.
type StatusSnapshot struct {
	SyncMetadata       map[string]string
	BaselineEntryCount int
	Issues             IssueSummary
	RemoteDriftItems   int
	DurableIntents     DurableIntentCounts
}

// DriveStatusSnapshot is the per-drive status snapshot consumed by the
// product-facing status command. It keeps the full per-drive sync-health view
// in one store-owned projection.
type DriveStatusSnapshot struct {
	SyncMetadata       map[string]string
	BaselineEntryCount int
	RemoteDriftItems   int
	RetryingItems      int
	IssueGroups        []IssueGroupSnapshot
	DeleteSafety       []DeleteSafetySnapshot
	Conflicts          []ConflictStatusSnapshot
	ConflictHistory    []ConflictHistorySnapshot
}

// groupedIssueProjection is the internal grouped sync-health projection shared
// by aggregate status readers and store/package tests. It stays unexported so
// the public store read surface has one product-facing per-drive projection.
type groupedIssueProjection struct {
	Conflicts      []ConflictRecord
	Groups         []IssueGroupSnapshot
	HeldDeletes    []HeldDeleteSnapshot
	PendingRetries []PendingRetrySnapshot
}

// DeleteSafetySnapshot is one held-delete approval row exposed through the
// per-drive status projection.
type DeleteSafetySnapshot struct {
	Path       string
	State      string
	LastSeenAt int64
	ApprovedAt int64
}

// ConflictStatusSnapshot is one unresolved conflict plus any active queued or
// applying request metadata.
type ConflictStatusSnapshot struct {
	ID                  string
	Path                string
	ConflictType        string
	DetectedAt          int64
	RequestedResolution string
	RequestState        string
	LastRequestError    string
	LastRequestedAt     int64
}

// ConflictHistorySnapshot is one resolved conflict record exposed when the
// caller opts into resolved conflict history.
type ConflictHistorySnapshot struct {
	ID           string
	Path         string
	ConflictType string
	DetectedAt   int64
	Resolution   string
	ResolvedAt   int64
	ResolvedBy   string
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

// HeldDeleteSnapshot is one held-delete entry surfaced in the sync-health read
// model. Held deletes stay distinct from generic grouped failures.
type HeldDeleteSnapshot struct {
	Path       string
	LastSeenAt int64
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
	statusScopeShortcut  = "shortcut"
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
// status command. It centralizes how conflicts, actionable rows, and special
// derived scopes count toward status.
type IssueSummary struct {
	Groups   []IssueGroupCount
	Retrying int
}

func (s groupedIssueProjection) Empty() bool {
	return len(s.Groups) == 0 && len(s.HeldDeletes) == 0
}

func (s IssueSummary) VisibleTotal() int {
	total := 0
	for _, group := range s.Groups {
		total += group.Count
	}

	return total
}

func (s IssueSummary) ConflictCount() int {
	return s.countForKey(SummaryConflictUnresolved)
}

func (s IssueSummary) ActionableCount() int {
	total := 0
	for _, group := range s.Groups {
		if group.Key == SummaryConflictUnresolved ||
			group.Key == SummaryRemoteWriteDenied ||
			group.Key == SummaryAuthenticationRequired {
			continue
		}
		total += group.Count
	}

	return total
}

func (s IssueSummary) RemoteBlockedCount() int {
	return s.countForKey(SummaryRemoteWriteDenied)
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

func (i *Inspector) ReadDurableIntentCounts(ctx context.Context) (DurableIntentCounts, error) {
	counts, err := countDurableIntents(ctx, i.db)
	if err != nil {
		return DurableIntentCounts{}, fmt.Errorf("read durable intent counts: %w", err)
	}

	return counts, nil
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
	counts, err := i.ReadDurableIntentCounts(ctx)
	if err != nil {
		i.logger.Debug("read durable intent counts", slog.String("error", err.Error()))
	} else {
		snapshot.DurableIntents = counts
	}
	projection, err := i.readGroupedIssueProjectionModel(ctx)
	if err != nil {
		i.logger.Debug("read visible issue status projection", slog.String("error", err.Error()))
	} else {
		projection.summary.Groups = appendConflictSummaryCount(
			projection.summary.Groups,
			i.countOrZero(ctx, "unresolved conflicts", "SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'"),
		)
		projection.summary.Retrying = i.countOrZero(
			ctx,
			"retrying sync failures",
			"SELECT COUNT(*) FROM sync_failures WHERE category = 'transient' AND failure_count >= 3",
		)
		snapshot.Issues = projection.summary
	}
	snapshot.RemoteDriftItems = i.countOrZero(
		ctx,
		"remote drift items",
		sqlCountRemoteDriftItems,
	)

	return snapshot
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

	deleteSafety, err := i.listDeleteSafety(ctx)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("list delete safety rows: %w", err)
	}
	snapshot.DeleteSafety = deleteSafety

	conflicts, err := i.listDriveStatusConflicts(ctx)
	if err != nil {
		return DriveStatusSnapshot{}, fmt.Errorf("list unresolved conflicts: %w", err)
	}
	snapshot.Conflicts = conflicts

	if history {
		conflictHistory, err := i.listConflictHistory(ctx)
		if err != nil {
			return DriveStatusSnapshot{}, fmt.Errorf("list conflict history: %w", err)
		}
		snapshot.ConflictHistory = conflictHistory
	}

	return snapshot, nil
}

// readGroupedIssueProjection returns the grouped sync-health projection used by
// internal readers and tests. history only widens the auxiliary conflicts
// slice for callers that also want resolved conflict history.
func (i *Inspector) readGroupedIssueProjection(ctx context.Context, history bool) (groupedIssueProjection, error) {
	projection, err := i.readGroupedIssueProjectionModel(ctx)
	if err != nil {
		return groupedIssueProjection{}, err
	}

	conflicts, err := i.listConflicts(ctx, history)
	if err != nil {
		return groupedIssueProjection{}, fmt.Errorf("list conflicts: %w", err)
	}
	projection.snapshot.Conflicts = conflicts

	return projection.snapshot, nil
}

type groupedIssueProjectionModel struct {
	snapshot groupedIssueProjection
	summary  IssueSummary
}

func (i *Inspector) readGroupedIssueProjectionModel(ctx context.Context) (groupedIssueProjectionModel, error) {
	shortcuts, err := i.listShortcuts(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list shortcuts: %w", err)
	}

	actionableFailures, err := i.listActionableFailures(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list actionable failures: %w", err)
	}

	heldDeletes, err := i.listHeldDeletes(ctx)
	if err != nil {
		return groupedIssueProjectionModel{}, fmt.Errorf("list held deletes: %w", err)
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
		heldDeletes,
		remoteBlocked,
		scopeBlocks,
		pendingRetries,
		shortcuts,
	), nil
}

func buildGroupedIssueProjection(
	actionableFailures []SyncFailureRow,
	heldDeletes []HeldDeleteRecord,
	remoteBlocked []SyncFailureRow,
	scopeBlocks []*ScopeBlock,
	pendingRetries []PendingRetryGroup,
	shortcuts []Shortcut,
) groupedIssueProjectionModel {
	builder := newGroupedIssueProjectionBuilder(shortcuts, len(pendingRetries))
	builder.addActionableFailures(actionableFailures)
	builder.addHeldDeletes(heldDeletes)
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
	shortcuts   []Shortcut
	groupCounts issueGroupAccumulator
	groupIndex  map[issueGroupKey]int
	snapshot    groupedIssueProjection
}

func newGroupedIssueProjectionBuilder(shortcuts []Shortcut, pendingRetryCap int) *groupedIssueProjectionBuilder {
	return &groupedIssueProjectionBuilder{
		shortcuts:   shortcuts,
		groupCounts: make(issueGroupAccumulator),
		groupIndex:  make(map[issueGroupKey]int),
		snapshot: groupedIssueProjection{
			Groups:         make([]IssueGroupSnapshot, 0),
			HeldDeletes:    make([]HeldDeleteSnapshot, 0),
			PendingRetries: make([]PendingRetrySnapshot, 0, pendingRetryCap),
		},
	}
}

func (b *groupedIssueProjectionBuilder) addSummary(
	key SummaryKey,
	scopeKey ScopeKey,
	role FailureRole,
) {
	scopeKind, scope := statusIssueScope(scopeKey, role, b.shortcuts)
	b.groupCounts.Add(key, 1, scopeKind, scope)
}

func (b *groupedIssueProjectionBuilder) addGroupedPath(
	summaryKey SummaryKey,
	issueType string,
	scopeKey ScopeKey,
	path string,
) bool {
	if summaryKey == "" {
		return false
	}

	key := issueGroupKey{summaryKey: summaryKey, scopeKey: scopeKey.String()}
	if idx, ok := b.groupIndex[key]; ok {
		b.snapshot.Groups[idx].Paths = append(b.snapshot.Groups[idx].Paths, path)
		b.snapshot.Groups[idx].Count++
		return false
	}

	b.groupIndex[key] = len(b.snapshot.Groups)
	b.snapshot.Groups = append(b.snapshot.Groups, IssueGroupSnapshot{
		SummaryKey:       summaryKey,
		PrimaryIssueType: issueType,
		ScopeKey:         scopeKey,
		ScopeLabel:       scopeKey.Humanize(b.shortcuts),
		Paths:            []string{path},
		Count:            1,
	})
	return true
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
		ScopeLabel:       scopeKey.Humanize(b.shortcuts),
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

func (b *groupedIssueProjectionBuilder) addHeldDeletes(rows []HeldDeleteRecord) {
	for i := range rows {
		row := rows[i]
		b.snapshot.HeldDeletes = append(b.snapshot.HeldDeletes, HeldDeleteSnapshot{
			Path:       row.Path,
			LastSeenAt: row.LastPlannedAt,
		})
		b.addSummary(SummaryHeldDeletes, ScopeKey{}, FailureRoleItem)
	}
}

func (b *groupedIssueProjectionBuilder) addRemoteBlocked(rows []SyncFailureRow) {
	for i := range rows {
		row := rows[i]
		summaryKey := SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
		if b.addGroupedPath(summaryKey, row.IssueType, row.ScopeKey, row.Path) {
			b.addSummary(summaryKey, row.ScopeKey, row.Role)
		}
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
			ScopeLabel:   group.ScopeKey.Humanize(b.shortcuts),
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

func appendConflictSummaryCount(groups []IssueGroupCount, count int) []IssueGroupCount {
	if count <= 0 {
		return groups
	}

	groups = append(groups, IssueGroupCount{
		Key:       SummaryConflictUnresolved,
		Count:     count,
		ScopeKind: statusScopeFile,
	})
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

func statusIssueScope(
	scopeKey ScopeKey,
	role FailureRole,
	shortcuts []Shortcut,
) (string, string) {
	if !scopeKey.IsZero() {
		switch scopeKey.Kind {
		case ScopeAuthAccount, ScopeThrottleAccount:
			return statusScopeAccount, scopeKey.Humanize(shortcuts)
		case ScopeThrottleTarget:
			if scopeKey.IsThrottleShared() {
				return statusScopeShortcut, scopeKey.Humanize(shortcuts)
			}
			return statusScopeDrive, scopeKey.Humanize(shortcuts)
		case ScopeService:
			return statusScopeService, scopeKey.Humanize(shortcuts)
		case ScopeQuotaOwn:
			return statusScopeDrive, scopeKey.Humanize(shortcuts)
		case ScopeQuotaShortcut, ScopePermRemoteWrite:
			return statusScopeShortcut, scopeKey.Humanize(shortcuts)
		case ScopePermLocalRead, ScopePermLocalWrite:
			return statusScopeDirectory, scopeKey.Humanize(shortcuts)
		case ScopeDiskLocal:
			return statusScopeDisk, scopeKey.Humanize(shortcuts)
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

func (i *Inspector) ListConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return i.listConflicts(ctx, false)
}

func (i *Inspector) ListAllConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return i.listConflicts(ctx, true)
}

func (i *Inspector) listConflicts(ctx context.Context, history bool) ([]ConflictRecord, error) {
	query := sqlListConflicts
	if history {
		query = sqlListAllConflicts
	}

	rows, err := i.db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTableErr(err) {
			return []ConflictRecord{}, nil
		}
		return nil, fmt.Errorf("query conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []ConflictRecord
	for rows.Next() {
		conflict, err := scanConflictRow(rows)
		if err != nil {
			return nil, err
		}

		conflicts = append(conflicts, *conflict)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflicts: %w", err)
	}

	return conflicts, nil
}

func (i *Inspector) listActionableFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable'
		AND (issue_type IS NULL OR issue_type != ?)
		ORDER BY last_seen_at DESC`,
		IssueDeleteSafetyHeld,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query actionable failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

func (i *Inspector) listHeldDeletes(ctx context.Context) ([]HeldDeleteRecord, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT drive_id, action_type, path, item_id, state, held_at, approved_at,
			last_planned_at, last_error
		FROM held_deletes
		WHERE state = ?
		ORDER BY last_planned_at DESC`,
		HeldDeleteStateHeld,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []HeldDeleteRecord{}, nil
		}
		return nil, fmt.Errorf("query held deletes: %w", err)
	}
	defer rows.Close()

	return scanHeldDeleteRows(rows)
}

func (i *Inspector) listDeleteSafety(ctx context.Context) ([]DeleteSafetySnapshot, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT path, state, last_planned_at, approved_at
		FROM held_deletes
		WHERE state IN (?, ?)
		ORDER BY CASE state
			WHEN ? THEN 0
			WHEN ? THEN 1
			ELSE 2
		END, last_planned_at DESC, path`,
		HeldDeleteStateHeld,
		HeldDeleteStateApproved,
		HeldDeleteStateHeld,
		HeldDeleteStateApproved,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []DeleteSafetySnapshot{}, nil
		}
		return nil, fmt.Errorf("query delete safety rows: %w", err)
	}
	defer rows.Close()

	var snapshots []DeleteSafetySnapshot
	for rows.Next() {
		var (
			snapshot   DeleteSafetySnapshot
			approvedAt sql.NullInt64
		)

		if err := rows.Scan(
			&snapshot.Path,
			&snapshot.State,
			&snapshot.LastSeenAt,
			&approvedAt,
		); err != nil {
			return nil, fmt.Errorf("scan delete safety row: %w", err)
		}
		if approvedAt.Valid {
			snapshot.ApprovedAt = approvedAt.Int64
		}

		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate delete safety rows: %w", err)
	}

	return snapshots, nil
}

func (i *Inspector) listDriveStatusConflicts(ctx context.Context) ([]ConflictStatusSnapshot, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT c.id, c.path, c.conflict_type, c.detected_at,
		       COALESCE(r.requested_resolution, ''),
		       COALESCE(r.state, ''),
		       COALESCE(r.requested_at, 0),
		       COALESCE(r.last_error, '')
		FROM conflicts c
		LEFT JOIN conflict_requests r ON r.conflict_id = c.id
		WHERE c.resolution = ?
		ORDER BY c.detected_at, c.path`,
		ResolutionUnresolved,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []ConflictStatusSnapshot{}, nil
		}
		return nil, fmt.Errorf("query unresolved conflicts for drive status: %w", err)
	}
	defer rows.Close()

	var snapshots []ConflictStatusSnapshot
	for rows.Next() {
		var snapshot ConflictStatusSnapshot
		if err := rows.Scan(
			&snapshot.ID,
			&snapshot.Path,
			&snapshot.ConflictType,
			&snapshot.DetectedAt,
			&snapshot.RequestedResolution,
			&snapshot.RequestState,
			&snapshot.LastRequestedAt,
			&snapshot.LastRequestError,
		); err != nil {
			return nil, fmt.Errorf("scan drive status conflict row: %w", err)
		}

		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate drive status conflict rows: %w", err)
	}

	return snapshots, nil
}

func (i *Inspector) listConflictHistory(ctx context.Context) ([]ConflictHistorySnapshot, error) {
	rows, err := i.db.QueryContext(ctx, `
		SELECT id, path, conflict_type, detected_at, resolution, resolved_at, resolved_by
		FROM conflicts
		WHERE resolution != ?
		ORDER BY resolved_at DESC, detected_at DESC, path`,
		ResolutionUnresolved,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []ConflictHistorySnapshot{}, nil
		}
		return nil, fmt.Errorf("query conflict history: %w", err)
	}
	defer rows.Close()

	var snapshots []ConflictHistorySnapshot
	for rows.Next() {
		var (
			snapshot   ConflictHistorySnapshot
			resolvedAt sql.NullInt64
			resolvedBy sql.NullString
		)
		if err := rows.Scan(
			&snapshot.ID,
			&snapshot.Path,
			&snapshot.ConflictType,
			&snapshot.DetectedAt,
			&snapshot.Resolution,
			&resolvedAt,
			&resolvedBy,
		); err != nil {
			return nil, fmt.Errorf("scan conflict history row: %w", err)
		}
		if resolvedAt.Valid {
			snapshot.ResolvedAt = resolvedAt.Int64
		}
		snapshot.ResolvedBy = resolvedBy.String
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate conflict history rows: %w", err)
	}

	return snapshots, nil
}

func (i *Inspector) listRemoteBlockedFailures(ctx context.Context) ([]SyncFailureRow, error) {
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

	return scanSyncFailureRows(rows)
}

func (i *Inspector) listShortcuts(ctx context.Context) ([]Shortcut, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		if isMissingTableErr(err) {
			return []Shortcut{}, nil
		}
		return nil, fmt.Errorf("query shortcuts: %w", err)
	}
	defer rows.Close()

	var shortcuts []Shortcut
	for rows.Next() {
		var shortcut Shortcut
		if err := rows.Scan(
			&shortcut.ItemID,
			&shortcut.RemoteDrive,
			&shortcut.RemoteItem,
			&shortcut.LocalPath,
			&shortcut.DriveType,
			&shortcut.Observation,
			&shortcut.DiscoveredAt,
		); err != nil {
			return nil, fmt.Errorf("scan shortcut row: %w", err)
		}

		shortcuts = append(shortcuts, shortcut)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shortcut rows: %w", err)
	}

	return shortcuts, nil
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
