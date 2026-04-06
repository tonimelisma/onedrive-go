package syncstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

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

// ScopeStateSnapshot is the read-only projection of durable sync-scope state
// consumed by operator-facing inspection surfaces.
type ScopeStateSnapshot struct {
	Found                 bool
	Generation            int64
	EffectiveSnapshotJSON string
	ObservationPlanHash   string
	ObservationMode       synctypes.ScopeObservationMode
	WebsocketEnabled      bool
	PendingReentry        bool
	LastReconcileKind     synctypes.ScopeReconcileKind
	UpdatedAt             int64
}

// IssuesSnapshot is the read-only projection consumed by the CLI issues
// command. It centralizes visible grouped issues and held deletes under one
// store-owned read model.
type IssuesSnapshot struct {
	Conflicts      []synctypes.ConflictRecord
	Groups         []IssueGroupSnapshot
	HeldDeletes    []HeldDeleteSnapshot
	PendingRetries []PendingRetrySnapshot
}

// IssueGroupSnapshot is one visible grouped issue family in the read-only
// issues projection.
type IssueGroupSnapshot struct {
	SummaryKey       synctypes.SummaryKey
	PrimaryIssueType string
	ScopeKey         synctypes.ScopeKey
	ScopeLabel       string
	Paths            []string
	Count            int
}

// HeldDeleteSnapshot is one held-delete entry surfaced in the issues read
// model. Held deletes stay distinct from generic grouped failures.
type HeldDeleteSnapshot struct {
	Path       string
	LastSeenAt int64
}

// PendingRetrySnapshot is one aggregated transient retry group in the
// store-owned issues projection. The simplified issues CLI no longer renders
// this section, but the snapshot keeps it available for internal observers and
// tests that assert retry state.
type PendingRetrySnapshot struct {
	ScopeKey     synctypes.ScopeKey
	ScopeLabel   string
	Count        int
	EarliestNext time.Time
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

func (s IssuesSnapshot) Empty() bool {
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

// HasScopeBlock reports whether the read-only store currently contains the
// requested scope block. Missing scope-block tables are treated as empty so
// partially initialized state databases can still be inspected.
func (i *Inspector) HasScopeBlock(ctx context.Context, key synctypes.ScopeKey) (bool, error) {
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

// ReadScopeStateSnapshot returns the durable scope-state projection used for
// scope inspection and restart recovery introspection. Missing tables are
// treated as empty so callers can inspect never-synced or older state DBs.
func (i *Inspector) ReadScopeStateSnapshot(ctx context.Context) (ScopeStateSnapshot, error) {
	row := i.db.QueryRowContext(ctx, `
		SELECT generation, effective_snapshot_json, observation_plan_hash,
		       observation_mode, websocket_enabled, pending_reentry,
		       last_reconcile_kind, updated_at
		FROM scope_state
		WHERE singleton = 1`,
	)

	var (
		snapshot         ScopeStateSnapshot
		websocketEnabled int
		pendingReentry   int
	)
	if err := row.Scan(
		&snapshot.Generation,
		&snapshot.EffectiveSnapshotJSON,
		&snapshot.ObservationPlanHash,
		&snapshot.ObservationMode,
		&websocketEnabled,
		&pendingReentry,
		&snapshot.LastReconcileKind,
		&snapshot.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows || isMissingTableErr(err) {
			return ScopeStateSnapshot{}, nil
		}

		return ScopeStateSnapshot{}, fmt.Errorf("read scope state snapshot: %w", err)
	}

	snapshot.Found = true
	snapshot.WebsocketEnabled = websocketEnabled != 0
	snapshot.PendingReentry = pendingReentry != 0

	return snapshot, nil
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
	projection, err := i.readVisibleIssueProjection(ctx)
	if err != nil {
		i.logger.Debug("read issues status projection", slog.String("error", err.Error()))
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
	snapshot.PendingSyncItems = i.countOrZero(
		ctx,
		"pending sync items",
		"SELECT COUNT(*) FROM remote_state WHERE sync_status NOT IN ('synced','deleted','filtered')",
	)

	return snapshot
}

// ReadIssuesSnapshot returns the read-only issues projection used by the CLI
// issues command. history only widens the auxiliary conflicts slice; the
// simplified issues CLI ignores it, but tests and internal observers still use
// this seam.
func (i *Inspector) ReadIssuesSnapshot(ctx context.Context, history bool) (IssuesSnapshot, error) {
	projection, err := i.readVisibleIssueProjection(ctx)
	if err != nil {
		return IssuesSnapshot{}, err
	}

	conflicts, err := i.listConflicts(ctx, history)
	if err != nil {
		return IssuesSnapshot{}, fmt.Errorf("list conflicts: %w", err)
	}
	projection.snapshot.Conflicts = conflicts

	return projection.snapshot, nil
}

type issuesProjection struct {
	snapshot IssuesSnapshot
	summary  IssueSummary
}

func (i *Inspector) readVisibleIssueProjection(ctx context.Context) (issuesProjection, error) {
	shortcuts, err := i.listShortcuts(ctx)
	if err != nil {
		return issuesProjection{}, fmt.Errorf("list shortcuts: %w", err)
	}

	actionableFailures, err := i.listActionableFailures(ctx)
	if err != nil {
		return issuesProjection{}, fmt.Errorf("list actionable failures: %w", err)
	}

	remoteBlocked, err := i.listRemoteBlockedFailures(ctx)
	if err != nil {
		return issuesProjection{}, fmt.Errorf("list remote blocked failures: %w", err)
	}

	scopeBlocks, err := i.listScopeBlocks(ctx)
	if err != nil {
		return issuesProjection{}, fmt.Errorf("list scope blocks: %w", err)
	}

	pendingRetries, err := i.pendingRetrySummary(ctx)
	if err != nil {
		return issuesProjection{}, fmt.Errorf("pending retry summary: %w", err)
	}

	return buildIssuesProjection(
		actionableFailures,
		remoteBlocked,
		scopeBlocks,
		pendingRetries,
		shortcuts,
	), nil
}

func buildIssuesProjection(
	actionableFailures []synctypes.SyncFailureRow,
	remoteBlocked []synctypes.SyncFailureRow,
	scopeBlocks []*synctypes.ScopeBlock,
	pendingRetries []synctypes.PendingRetryGroup,
	shortcuts []synctypes.Shortcut,
) issuesProjection {
	builder := newIssuesProjectionBuilder(shortcuts, len(pendingRetries))
	builder.addActionableFailures(actionableFailures)
	builder.addRemoteBlocked(remoteBlocked)
	builder.addAuthScopeBlocks(scopeBlocks)
	builder.addPendingRetries(pendingRetries)
	return builder.projection()
}

type issueGroupKey struct {
	summaryKey synctypes.SummaryKey
	scopeKey   string
}

type issuesProjectionBuilder struct {
	shortcuts   []synctypes.Shortcut
	groupCounts issueGroupAccumulator
	groupIndex  map[issueGroupKey]int
	snapshot    IssuesSnapshot
}

func newIssuesProjectionBuilder(shortcuts []synctypes.Shortcut, pendingRetryCap int) *issuesProjectionBuilder {
	return &issuesProjectionBuilder{
		shortcuts:   shortcuts,
		groupCounts: make(issueGroupAccumulator),
		groupIndex:  make(map[issueGroupKey]int),
		snapshot: IssuesSnapshot{
			Groups:         make([]IssueGroupSnapshot, 0),
			HeldDeletes:    make([]HeldDeleteSnapshot, 0),
			PendingRetries: make([]PendingRetrySnapshot, 0, pendingRetryCap),
		},
	}
}

func (b *issuesProjectionBuilder) addSummary(
	key synctypes.SummaryKey,
	scopeKey synctypes.ScopeKey,
	role synctypes.FailureRole,
) {
	scopeKind, scope := statusIssueScope(scopeKey, role, b.shortcuts)
	b.groupCounts.Add(key, 1, scopeKind, scope)
}

func (b *issuesProjectionBuilder) addGroupedPath(
	summaryKey synctypes.SummaryKey,
	issueType string,
	scopeKey synctypes.ScopeKey,
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

func (b *issuesProjectionBuilder) addScopeOnlyGroup(
	summaryKey synctypes.SummaryKey,
	issueType string,
	scopeKey synctypes.ScopeKey,
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

func (b *issuesProjectionBuilder) addActionableFailures(rows []synctypes.SyncFailureRow) {
	for i := range rows {
		row := rows[i]
		summaryKey := synctypes.SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
		if row.IssueType == synctypes.IssueBigDeleteHeld {
			b.snapshot.HeldDeletes = append(b.snapshot.HeldDeletes, HeldDeleteSnapshot{
				Path:       row.Path,
				LastSeenAt: row.LastSeenAt,
			})
			b.addSummary(summaryKey, row.ScopeKey, row.Role)
			continue
		}

		b.addGroupedPath(summaryKey, row.IssueType, row.ScopeKey, row.Path)
		b.addSummary(summaryKey, row.ScopeKey, row.Role)
	}
}

func (b *issuesProjectionBuilder) addRemoteBlocked(rows []synctypes.SyncFailureRow) {
	for i := range rows {
		row := rows[i]
		summaryKey := synctypes.SummaryKeyForPersistedFailure(row.IssueType, row.Category, row.Role)
		if b.addGroupedPath(summaryKey, row.IssueType, row.ScopeKey, row.Path) {
			b.addSummary(summaryKey, row.ScopeKey, row.Role)
		}
	}
}

func (b *issuesProjectionBuilder) addAuthScopeBlocks(blocks []*synctypes.ScopeBlock) {
	for i := range blocks {
		block := blocks[i]
		if block.Key != synctypes.SKAuthAccount() {
			continue
		}

		summaryKey := synctypes.SummaryKeyForScopeBlock(block.IssueType, block.Key)
		if b.addScopeOnlyGroup(summaryKey, block.IssueType, block.Key) {
			b.addSummary(summaryKey, block.Key, synctypes.FailureRoleBoundary)
		}
	}
}

func (b *issuesProjectionBuilder) addPendingRetries(groups []synctypes.PendingRetryGroup) {
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

func (b *issuesProjectionBuilder) projection() issuesProjection {
	sortIssueGroups(b.snapshot.Groups)

	summary := IssueSummary{
		Groups: b.groupCounts.Groups(),
	}

	return issuesProjection{
		snapshot: b.snapshot,
		summary:  summary,
	}
}

func appendConflictSummaryCount(groups []IssueGroupCount, count int) []IssueGroupCount {
	if count <= 0 {
		return groups
	}

	groups = append(groups, IssueGroupCount{
		Key:       synctypes.SummaryConflictUnresolved,
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
	scopeKey synctypes.ScopeKey,
	role synctypes.FailureRole,
	shortcuts []synctypes.Shortcut,
) (string, string) {
	if !scopeKey.IsZero() {
		switch scopeKey.Kind {
		case synctypes.ScopeAuthAccount, synctypes.ScopeThrottleAccount:
			return statusScopeAccount, scopeKey.Humanize(shortcuts)
		case synctypes.ScopeThrottleTarget:
			if scopeKey.IsThrottleShared() {
				return statusScopeShortcut, scopeKey.Humanize(shortcuts)
			}
			return statusScopeDrive, scopeKey.Humanize(shortcuts)
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

func sortIssueGroups(groups []IssueGroupSnapshot) {
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}

		left := synctypes.DescribeSummary(groups[i].SummaryKey).Title
		right := synctypes.DescribeSummary(groups[j].SummaryKey).Title
		if left != right {
			return left < right
		}

		return groups[i].ScopeLabel < groups[j].ScopeLabel
	})

	for i := range groups {
		sort.Strings(groups[i].Paths)
	}
}

func (i *Inspector) ListConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return i.listConflicts(ctx, false)
}

func (i *Inspector) ListAllConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return i.listConflicts(ctx, true)
}

func (i *Inspector) listConflicts(ctx context.Context, history bool) ([]synctypes.ConflictRecord, error) {
	query := sqlListConflicts
	if history {
		query = sqlListAllConflicts
	}

	rows, err := i.db.QueryContext(ctx, query)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.ConflictRecord{}, nil
		}
		return nil, fmt.Errorf("query conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []synctypes.ConflictRecord
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

func (i *Inspector) listActionableFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable' ORDER BY last_seen_at DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query actionable failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

func (i *Inspector) listRemoteBlockedFailures(ctx context.Context) ([]synctypes.SyncFailureRow, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE failure_role = ? AND scope_key LIKE 'perm:remote:%'
		ORDER BY last_seen_at DESC`,
		synctypes.FailureRoleHeld,
	)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.SyncFailureRow{}, nil
		}
		return nil, fmt.Errorf("query remote blocked failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

func (i *Inspector) listShortcuts(ctx context.Context) ([]synctypes.Shortcut, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.Shortcut{}, nil
		}
		return nil, fmt.Errorf("query shortcuts: %w", err)
	}
	defer rows.Close()

	var shortcuts []synctypes.Shortcut
	for rows.Next() {
		var shortcut synctypes.Shortcut
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

func (i *Inspector) listScopeBlocks(ctx context.Context) ([]*synctypes.ScopeBlock, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT scope_key, issue_type, timing_source, blocked_at, trial_interval, next_trial_at, preserve_until, trial_count
		FROM scope_blocks`)
	if err != nil {
		if isMissingTableErr(err) {
			return []*synctypes.ScopeBlock{}, nil
		}
		return nil, fmt.Errorf("query scope blocks: %w", err)
	}
	defer rows.Close()

	var result []*synctypes.ScopeBlock
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

		block := &synctypes.ScopeBlock{
			Key:           synctypes.ParseScopeKey(wireKey),
			IssueType:     issueType,
			TimingSource:  synctypes.ScopeTimingSource(timingSource),
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

func (i *Inspector) pendingRetrySummary(ctx context.Context) ([]synctypes.PendingRetryGroup, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT COALESCE(scope_key, ''), COUNT(*), MIN(next_retry_at)
		 FROM sync_failures
		 WHERE category = 'transient' AND next_retry_at > 0
		 GROUP BY scope_key
		 ORDER BY COUNT(*) DESC`)
	if err != nil {
		if isMissingTableErr(err) {
			return []synctypes.PendingRetryGroup{}, nil
		}
		return nil, fmt.Errorf("query pending retry summary: %w", err)
	}
	defer rows.Close()

	var result []synctypes.PendingRetryGroup
	for rows.Next() {
		var (
			group        synctypes.PendingRetryGroup
			wireScopeKey string
			minNano      int64
		)

		if err := rows.Scan(&wireScopeKey, &group.Count, &minNano); err != nil {
			return nil, fmt.Errorf("scan pending retry group: %w", err)
		}

		group.ScopeKey = synctypes.ParseScopeKey(wireScopeKey)
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
